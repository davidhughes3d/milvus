// Licensed to the LF AI & Data foundation under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package querynode

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/milvus-io/milvus/internal/proto/querypb"
	"github.com/milvus-io/milvus/pkg/log"
	"github.com/milvus-io/milvus/pkg/util/funcutil"
	"github.com/milvus-io/milvus/pkg/util/timerecord"
	"github.com/milvus-io/milvus/pkg/util/tsoutil"
)

type readTask interface {
	task

	Ctx() context.Context

	GetCollectionID() UniqueID

	Ready() (bool, error)
	Merge(readTask)
	CanMergeWith(readTask) bool
	CPUUsage() int32
	Timeout() bool
	TimeoutError() error

	SetMaxCPUUsage(int32)
	SetStep(step TaskStep)
}

var _ readTask = (*baseReadTask)(nil)

type baseReadTask struct {
	baseTask

	QS *queryShard

	DataScope          querypb.DataScope
	cpu                int32
	maxCPU             int32
	DbID               int64
	CollectionID       int64
	TravelTimestamp    uint64
	GuaranteeTimestamp uint64
	TimeoutTimestamp   uint64
	step               TaskStep
	queueDur           time.Duration
	reduceDur          time.Duration
	waitTsDur          time.Duration
	waitTSafeTr        *timerecord.TimeRecorder
	tr                 *timerecord.TimeRecorder
}

func (b *baseReadTask) SetStep(step TaskStep) {
	b.step = step
	switch step {
	case TaskStepEnqueue:
		b.queueDur = 0
		b.tr.RecordSpan()
	case TaskStepPreExecute:
		b.queueDur = b.tr.RecordSpan()
	}
}

func (b *baseReadTask) OnEnqueue() error {
	b.SetStep(TaskStepEnqueue)
	return nil
}

func (b *baseReadTask) SetMaxCPUUsage(cpu int32) {
	b.maxCPU = cpu
}

func (b *baseReadTask) PreExecute(ctx context.Context) error {
	b.SetStep(TaskStepPreExecute)
	return nil
}

func (b *baseReadTask) Execute(ctx context.Context) error {
	b.SetStep(TaskStepExecute)
	return nil
}

func (b *baseReadTask) PostExecute(ctx context.Context) error {
	b.SetStep(TaskStepPostExecute)
	return nil
}

func (b *baseReadTask) Notify(err error) {
	switch b.step {
	case TaskStepEnqueue:
		b.queueDur = b.tr.RecordSpan()
	case TaskStepPostExecute:
		b.tr.RecordSpan()
	}
	b.baseTask.Notify(err)
}

// GetCollectionID return CollectionID.
func (b *baseReadTask) GetCollectionID() UniqueID {
	return b.CollectionID
}

func (b *baseReadTask) CanMergeWith(t readTask) bool {
	return false
}

func (b *baseReadTask) Merge(t readTask) {
}

func (b *baseReadTask) CPUUsage() int32 {
	return 0
}

func (b *baseReadTask) Timeout() bool {
	return !funcutil.CheckCtxValid(b.Ctx())
}

func (b *baseReadTask) TimeoutError() error {
	return b.ctx.Err()
}

func (b *baseReadTask) Ready() (bool, error) {
	if b.waitTSafeTr == nil {
		b.waitTSafeTr = timerecord.NewTimeRecorder("waitTSafeTimeRecorder")
	}
	if b.Timeout() {
		return false, b.TimeoutError()
	}
	var channel Channel
	if b.DataScope == querypb.DataScope_Streaming {
		channel = b.QS.channel
	} else if b.DataScope == querypb.DataScope_Historical {
		channel = b.QS.deltaChannel
	} else {
		return false, fmt.Errorf("unexpected dataScope %s", b.DataScope.String())
	}

	if _, released := b.QS.collection.getReleaseTime(); released {
		log.Info("collection release before search", zap.Int64("collectionID", b.CollectionID))
		return false, fmt.Errorf("collection has been released, taskID = %d, collectionID = %d", b.ID(), b.CollectionID)
	}

	serviceTime, err := b.QS.getServiceableTime(channel)
	if err != nil {
		return false, fmt.Errorf("failed to get service timestamp, taskID = %d, collectionID = %d, err=%w", b.ID(), b.CollectionID, err)
	}
	guaranteeTs := b.GuaranteeTimestamp
	gt, _ := tsoutil.ParseTS(guaranteeTs)
	st, _ := tsoutil.ParseTS(serviceTime)
	if guaranteeTs > serviceTime {
		lag := gt.Sub(st)
		maxLag := Params.QueryNodeCfg.MaxTimestampLag.GetAsDuration(time.Second)
		if serviceTime != 0 && lag > maxLag {
			log.Warn("guarantee and servicable ts larger than MaxLag",
				zap.Time("guaranteeTime", gt),
				zap.Time("serviceableTime", st),
				zap.Duration("lag", lag),
				zap.Duration("maxTsLag", maxLag),
			)
			return false, WrapErrTsLagTooLarge(lag, maxLag)
		}
		return false, nil
	}
	log.Debug("query msg can do",
		zap.Int64("collectionID", b.CollectionID),
		zap.Time("sm.GuaranteeTimestamp", gt),
		zap.Time("serviceTime", st),
		zap.Int64("delta milliseconds", gt.Sub(st).Milliseconds()),
		zap.String("channel", channel),
		zap.Int64("msgID", b.ID()))
	b.waitTsDur = b.waitTSafeTr.Elapse("wait for tsafe done")
	return true, nil
}
