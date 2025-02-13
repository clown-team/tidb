// Copyright 2022 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ddl

import (
	"context"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/domain/infosync"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/meta"
	"github.com/pingcap/tidb/metrics"
	"github.com/pingcap/tidb/parser/model"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/util/gcutil"
	"github.com/tikv/client-go/v2/oracle"
)

var pdScheduleKey = []string{
	"hot-region-schedule-limit",
	"leader-schedule-limit",
	"merge-schedule-limit",
	"region-schedule-limit",
	"replica-schedule-limit",
}

func closePDSchedule(job *model.Job) error {
	if err := savePDSchedule(job); err != nil {
		return err
	}
	saveValue := make(map[string]interface{})
	for _, key := range pdScheduleKey {
		saveValue[key] = 0
	}
	return infosync.SetPDScheduleConfig(context.Background(), saveValue)
}

func savePDSchedule(job *model.Job) error {
	retValue, err := infosync.GetPDScheduleConfig(context.Background())
	if err != nil {
		return err
	}
	saveValue := make(map[string]interface{})
	for _, key := range pdScheduleKey {
		saveValue[key] = retValue[key]
	}
	job.Args = append(job.Args, saveValue)
	return nil
}

func recoverPDSchedule(pdScheduleParam map[string]interface{}) error {
	if pdScheduleParam == nil {
		return nil
	}
	return infosync.SetPDScheduleConfig(context.Background(), pdScheduleParam)
}

// ValidateFlashbackTS validates that flashBackTS in range [gcSafePoint, currentTS).
func ValidateFlashbackTS(ctx context.Context, sctx sessionctx.Context, flashBackTS uint64) error {
	currentTS, err := sctx.GetStore().GetOracle().GetStaleTimestamp(ctx, oracle.GlobalTxnScope, 0)
	// If we fail to calculate currentTS from local time, fallback to get a timestamp from PD.
	if err != nil {
		metrics.ValidateReadTSFromPDCount.Inc()
		currentVer, err := sctx.GetStore().CurrentVersion(oracle.GlobalTxnScope)
		if err != nil {
			return errors.Errorf("fail to validate flashback timestamp: %v", err)
		}
		currentTS = currentVer.Ver
	}
	if oracle.GetTimeFromTS(flashBackTS).After(oracle.GetTimeFromTS(currentTS)) {
		return errors.Errorf("cannot set flashback timestamp to future time")
	}
	gcSafePoint, err := gcutil.GetGCSafePoint(sctx)
	if err != nil {
		return err
	}

	return gcutil.ValidateSnapshotWithGCSafePoint(flashBackTS, gcSafePoint)
}

func checkFlashbackCluster(w *worker, d *ddlCtx, t *meta.Meta, job *model.Job, flashbackTS uint64) (err error) {
	sess, err := w.sessPool.get()
	if err != nil {
		return errors.Trace(err)
	}
	defer w.sessPool.put(sess)

	if err = gcutil.DisableGC(sess); err != nil {
		return err
	}
	if err = closePDSchedule(job); err != nil {
		return err
	}
	if err = ValidateFlashbackTS(d.ctx, sess, flashbackTS); err != nil {
		return err
	}

	nowSchemaVersion, err := t.GetSchemaVersion()
	if err != nil {
		return errors.Trace(err)
	}

	flashbackSchemaVersion, err := meta.NewSnapshotMeta(d.store.GetSnapshot(kv.NewVersion(flashbackTS))).GetSchemaVersion()
	if err != nil {
		return errors.Trace(err)
	}

	// If flashbackSchemaVersion not same as nowSchemaVersion, we've done ddl during [flashbackTs, now).
	if flashbackSchemaVersion != nowSchemaVersion {
		return errors.Errorf("schema version not same, have done ddl during [flashbackTS, now)")
	}

	jobs, err := GetAllDDLJobs(sess, t)
	if err != nil {
		return errors.Trace(err)
	}
	// Other ddl jobs in queue, return error.
	if len(jobs) != 1 {
		var otherJob *model.Job
		for _, j := range jobs {
			if j.ID != job.ID {
				otherJob = j
				break
			}
		}
		return errors.Errorf("have other ddl jobs(jobID: %d) in queue, can't do flashback", otherJob.ID)
	}
	return nil
}

// A Flashback has 3 different stages.
// 1. before lock flashbackClusterJobID, check clusterJobID and lock it.
// 2. before reorg start, check timestamp, disable GC and close PD schedule.
// 3. before reorg done.
func (w *worker) onFlashbackCluster(d *ddlCtx, t *meta.Meta, job *model.Job) (ver int64, err error) {
	var flashbackTS uint64
	if err := job.DecodeArgs(&flashbackTS); err != nil {
		job.State = model.JobStateCancelled
		return ver, errors.Trace(err)
	}

	flashbackJobID, err := t.GetFlashbackClusterJobID()
	if err != nil {
		return ver, err
	}

	// stage 1, check and set FlashbackClusterJobID.
	if flashbackJobID == 0 {
		err = kv.RunInNewTxn(w.ctx, w.store, true, func(ctx context.Context, txn kv.Transaction) error {
			return meta.NewMeta(txn).SetFlashbackClusterJobID(job.ID)
		})
		if err != nil {
			job.State = model.JobStateCancelled
		}
		return ver, errors.Trace(err)
	} else if flashbackJobID != job.ID {
		job.State = model.JobStateCancelled
		return ver, errors.Errorf("Other flashback job(ID: %d) is running", job.ID)
	}

	// stage 2, before reorg start, SnapshotVer == 0 means, job has not started reorg
	if job.SnapshotVer == 0 {
		if err = checkFlashbackCluster(w, d, t, job, flashbackTS); err != nil {
			job.State = model.JobStateCancelled
			return ver, errors.Trace(err)
		}
		// get the current version for reorganization.
		snapVer, err := getValidCurrentVersion(d.store)
		if err != nil {
			job.State = model.JobStateCancelled
			return ver, errors.Trace(err)
		}
		job.SnapshotVer = snapVer.Ver
		return ver, nil
	}

	job.State = model.JobStateDone
	return ver, errors.Trace(err)
}

func finishFlashbackCluster(w *worker, job *model.Job) error {
	var flashbackTS uint64
	var pdScheduleValue map[string]interface{}
	if err := job.DecodeArgs(&flashbackTS, &pdScheduleValue); err != nil {
		return errors.Trace(err)
	}

	err := kv.RunInNewTxn(w.ctx, w.store, true, func(ctx context.Context, txn kv.Transaction) error {
		t := meta.NewMeta(txn)
		jobID, err := t.GetFlashbackClusterJobID()
		if err != nil {
			return err
		}
		if jobID == job.ID {
			if pdScheduleValue != nil {
				if err = recoverPDSchedule(pdScheduleValue); err != nil {
					return err
				}
			}
			if err = enableGC(w); err != nil {
				return err
			}
			err = t.SetFlashbackClusterJobID(0)
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	return nil
}
