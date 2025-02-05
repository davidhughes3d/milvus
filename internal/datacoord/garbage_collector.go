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

package datacoord

import (
	"context"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/samber/lo"
	"go.uber.org/zap"

	"github.com/milvus-io/milvus-proto/go-api/commonpb"
	"github.com/milvus-io/milvus/internal/proto/datapb"
	"github.com/milvus-io/milvus/internal/storage"
	"github.com/milvus-io/milvus/pkg/common"
	"github.com/milvus-io/milvus/pkg/log"
	"github.com/milvus-io/milvus/pkg/util/metautil"
	"github.com/milvus-io/milvus/pkg/util/typeutil"
)

const (
	//TODO silverxia change to configuration
	insertLogPrefix = `insert_log`
	statsLogPrefix  = `stats_log`
	deltaLogPrefix  = `delta_log`
)

type collectionValidator func(int64) bool

// GcOption garbage collection options
type GcOption struct {
	cli              storage.ChunkManager // client
	enabled          bool                 // enable switch
	checkInterval    time.Duration        // each interval
	missingTolerance time.Duration        // key missing in meta tolerance time
	dropTolerance    time.Duration        // dropped segment related key tolerance time
	collValidator    collectionValidator  // validates collection id
}

// garbageCollector handles garbage files in object storage
// which could be dropped collection remanent or data node failure traces
type garbageCollector struct {
	option  GcOption
	meta    *meta
	handler Handler

	startOnce sync.Once
	stopOnce  sync.Once
	wg        sync.WaitGroup
	closeCh   chan struct{}
}

// newGarbageCollector create garbage collector with meta and option
func newGarbageCollector(meta *meta, handler Handler, opt GcOption) *garbageCollector {
	log.Info("GC with option", zap.Bool("enabled", opt.enabled), zap.Duration("interval", opt.checkInterval),
		zap.Duration("missingTolerance", opt.missingTolerance), zap.Duration("dropTolerance", opt.dropTolerance))
	return &garbageCollector{
		meta:    meta,
		handler: handler,
		option:  opt,
		closeCh: make(chan struct{}),
	}
}

// start a goroutine and perform gc check every `checkInterval`
func (gc *garbageCollector) start() {
	if gc.option.enabled {
		if gc.option.cli == nil {
			log.Warn("DataCoord gc enabled, but SSO client is not provided")
			return
		}
		gc.startOnce.Do(func() {
			gc.wg.Add(1)
			go gc.work()
		})
	}
}

// work contains actual looping check logic
func (gc *garbageCollector) work() {
	defer gc.wg.Done()
	ticker := time.NewTicker(gc.option.checkInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			gc.clearEtcd()
			gc.recycleUnusedIndexes()
			gc.recycleUnusedSegIndexes()
			gc.scan()
			gc.recycleUnusedIndexFiles()
		case <-gc.closeCh:
			log.Warn("garbage collector quit")
			return
		}
	}
}

func (gc *garbageCollector) isCollectionPrefixValid(p string, prefix string) bool {
	if gc.option.collValidator == nil {
		return true
	}

	if !strings.HasPrefix(p, prefix) {
		return false
	}

	p = strings.Trim(p[len(prefix):], "/")
	collectionID, err := strconv.ParseInt(p, 10, 64)
	if err != nil {
		return false
	}

	return gc.option.collValidator(collectionID)
}

func (gc *garbageCollector) close() {
	gc.stopOnce.Do(func() {
		close(gc.closeCh)
		gc.wg.Wait()
	})
}

// scan load meta file info and compares OSS keys
// if missing found, performs gc cleanup
func (gc *garbageCollector) scan() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		total   = 0
		valid   = 0
		missing = 0

		segmentMap = typeutil.NewUniqueSet()
		filesMap   = typeutil.NewSet[string]()
	)
	segments := gc.meta.GetAllSegmentsUnsafe()
	for _, segment := range segments {
		segmentMap.Insert(segment.GetID())
		for _, log := range getLogs(segment) {
			filesMap.Insert(log.GetLogPath())
		}
	}

	// walk only data cluster related prefixes
	prefixes := make([]string, 0, 3)
	prefixes = append(prefixes, path.Join(gc.option.cli.RootPath(), insertLogPrefix))
	prefixes = append(prefixes, path.Join(gc.option.cli.RootPath(), statsLogPrefix))
	prefixes = append(prefixes, path.Join(gc.option.cli.RootPath(), deltaLogPrefix))
	var removedKeys []string

	for _, prefix := range prefixes {
		// list first level prefix, then perform collection id validation
		collectionPrefixes, _, err := gc.option.cli.ListWithPrefix(ctx, prefix+"/", false)
		if err != nil {
			log.Warn("failed to list collection prefix",
				zap.String("prefix", prefix),
				zap.Error(err),
			)
		}
		for _, collPrefix := range collectionPrefixes {
			if !gc.isCollectionPrefixValid(collPrefix, prefix) {
				log.Warn("garbage collector meet invalid collection prefix, ignore it",
					zap.String("collPrefix", collPrefix),
					zap.String("prefix", prefix),
				)
				continue
			}
			infoKeys, modTimes, err := gc.option.cli.ListWithPrefix(ctx, collPrefix, true)
			if err != nil {
				log.Error("failed to list files with collPrefix",
					zap.String("collPrefix", collPrefix),
					zap.String("error", err.Error()),
				)
				continue
			}
			for i, infoKey := range infoKeys {
				total++
				_, has := filesMap[infoKey]
				if has {
					valid++
					continue
				}

				segmentID, err := storage.ParseSegmentIDByBinlog(gc.option.cli.RootPath(), infoKey)
				if err != nil {
					missing++
					log.Warn("parse segment id error",
						zap.String("infoKey", infoKey),
						zap.Error(err))
					continue
				}

				if strings.Contains(prefix, statsLogPrefix) &&
					segmentMap.Contain(segmentID) {
					valid++
					continue
				}

				// not found in meta, check last modified time exceeds tolerance duration
				if time.Since(modTimes[i]) > gc.option.missingTolerance {
					// ignore error since it could be cleaned up next time
					removedKeys = append(removedKeys, infoKey)
					err = gc.option.cli.Remove(ctx, infoKey)
					if err != nil {
						missing++
						log.Error("failed to remove object",
							zap.String("infoKey", infoKey),
							zap.Error(err))
					}
				}
			}
		}
	}
	log.Info("scan file to do garbage collection",
		zap.Int("total", total),
		zap.Int("valid", valid),
		zap.Int("missing", missing),
		zap.Strings("removedKeys", removedKeys))
}

func (gc *garbageCollector) clearEtcd() {
	all := gc.meta.SelectSegments(func(si *SegmentInfo) bool { return true })
	drops := make(map[int64]*SegmentInfo, 0)
	compactTo := make(map[int64]*SegmentInfo)
	channels := typeutil.NewSet[string]()
	for _, segment := range all {
		if segment.GetState() == commonpb.SegmentState_Dropped {
			drops[segment.GetID()] = segment
			channels.Insert(segment.GetInsertChannel())
			//continue
			// A(indexed), B(indexed) -> C(no indexed), D(no indexed) -> E(no indexed), A, B can not be GC
		}
		for _, from := range segment.GetCompactionFrom() {
			compactTo[from] = segment
		}
	}

	droppedCompactTo := make(map[*SegmentInfo]struct{})
	for id := range drops {
		if to, ok := compactTo[id]; ok {
			droppedCompactTo[to] = struct{}{}
		}
	}
	indexedSegments := FilterInIndexedSegments(gc.handler, gc.meta, lo.Keys(droppedCompactTo)...)
	indexedSet := make(typeutil.UniqueSet)
	for _, segment := range indexedSegments {
		indexedSet.Insert(segment.GetID())
	}

	channelCPs := make(map[string]uint64)
	for channel := range channels {
		pos := gc.meta.GetChannelCheckpoint(channel)
		channelCPs[channel] = pos.GetTimestamp()
	}

	for _, segment := range drops {
		log := log.With(zap.Int64("segmentID", segment.ID))
		if !gc.isExpire(segment.GetDroppedAt()) {
			continue
		}
		segInsertChannel := segment.GetInsertChannel()
		// Ignore segments from potentially dropped collection. Check if collection is to be dropped by checking if channel is dropped.
		// We do this because collection meta drop relies on all segment being GCed.
		if gc.meta.catalog.ChannelExists(context.Background(), segInsertChannel) &&
			segment.GetDmlPosition().GetTimestamp() > channelCPs[segInsertChannel] {
			// segment gc shall only happen when channel cp is after segment dml cp.
			log.WithRateGroup("GC_FAIL_CP_BEFORE", 1, 60).
				RatedInfo(60, "dropped segment dml position after channel cp, skip meta gc",
					zap.Uint64("dmlPosTs", segment.GetDmlPosition().GetTimestamp()),
					zap.Uint64("channelCpTs", channelCPs[segInsertChannel]),
				)
			continue
		}
		// For compact A, B -> C, don't GC A or B if C is not indexed,
		// guarantee replacing A, B with C won't downgrade performance
		if to, ok := compactTo[segment.GetID()]; ok && !indexedSet.Contain(to.GetID()) {
			log.WithRateGroup("GC_FAIL_COMPACT_TO_NOT_INDEXED", 1, 60).
				RatedWarn(60, "skipping GC when compact target segment is not indexed",
					zap.Int64("segmentID", to.GetID()))
			continue
		}
		logs := getLogs(segment)
		log.Info("GC segment", zap.Int64("segmentID", segment.GetID()))
		if gc.removeLogs(logs) {
			_ = gc.meta.DropSegment(segment.GetID())
		}
		if segList := gc.meta.GetSegmentsByChannel(segInsertChannel); len(segList) == 0 &&
			!gc.meta.catalog.ChannelExists(context.Background(), segInsertChannel) {
			log.Info("empty channel found during gc, manually cleanup channel checkpoints",
				zap.String("vChannel", segInsertChannel))

			if err := gc.meta.DropChannelCheckpoint(segInsertChannel); err != nil {
				// Fail-open as there's nothing to do.
				log.Warn("failed to drop channel check point during segment garbage collection", zap.Error(err))
			}
		}
	}
}

func (gc *garbageCollector) isExpire(dropts Timestamp) bool {
	droptime := time.Unix(0, int64(dropts))
	return time.Since(droptime) > gc.option.dropTolerance
}

func getLogs(sinfo *SegmentInfo) []*datapb.Binlog {
	var logs []*datapb.Binlog
	for _, flog := range sinfo.GetBinlogs() {
		logs = append(logs, flog.GetBinlogs()...)
	}

	for _, flog := range sinfo.GetStatslogs() {
		logs = append(logs, flog.GetBinlogs()...)
	}

	for _, flog := range sinfo.GetDeltalogs() {
		logs = append(logs, flog.GetBinlogs()...)
	}
	return logs
}

func (gc *garbageCollector) removeLogs(logs []*datapb.Binlog) bool {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	delFlag := true
	for _, l := range logs {
		err := gc.option.cli.Remove(ctx, l.GetLogPath())
		if err != nil {
			switch err.(type) {
			case minio.ErrorResponse:
				errResp := minio.ToErrorResponse(err)
				if errResp.Code != "" && errResp.Code != "NoSuchKey" {
					delFlag = false
				}
			default:
				delFlag = false
			}
		}
	}
	return delFlag
}

func (gc *garbageCollector) recycleUnusedIndexes() {
	log.Info("start recycleUnusedIndexes")
	deletedIndexes := gc.meta.GetDeletedIndexes()
	for _, index := range deletedIndexes {
		if err := gc.meta.RemoveIndex(index.CollectionID, index.IndexID); err != nil {
			log.Warn("remove index on collection fail", zap.Int64("collID", index.CollectionID),
				zap.Int64("indexID", index.IndexID), zap.Error(err))
			continue
		}
	}
}

func (gc *garbageCollector) recycleUnusedSegIndexes() {
	segIndexes := gc.meta.GetAllSegIndexes()
	for _, segIdx := range segIndexes {
		if gc.meta.GetSegment(segIdx.SegmentID) == nil || !gc.meta.IsIndexExist(segIdx.CollectionID, segIdx.IndexID) {
			if err := gc.meta.RemoveSegmentIndex(segIdx.CollectionID, segIdx.PartitionID, segIdx.SegmentID, segIdx.IndexID, segIdx.BuildID); err != nil {
				log.Warn("delete index meta from etcd failed, wait to retry", zap.Int64("buildID", segIdx.BuildID),
					zap.Int64("segID", segIdx.SegmentID), zap.Int64("nodeID", segIdx.NodeID), zap.Error(err))
				continue
			}
			log.Info("index meta recycle success", zap.Int64("buildID", segIdx.BuildID),
				zap.Int64("segID", segIdx.SegmentID))
		}
	}
}

// recycleUnusedIndexFiles is used to delete those index files that no longer exist in the meta.
func (gc *garbageCollector) recycleUnusedIndexFiles() {
	log.Info("start recycleUnusedIndexFiles")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	prefix := path.Join(gc.option.cli.RootPath(), common.SegmentIndexPath) + "/"
	// list dir first
	keys, _, err := gc.option.cli.ListWithPrefix(ctx, prefix, false)
	if err != nil {
		log.Error("garbageCollector recycleUnusedIndexFiles list keys from chunk manager failed", zap.Error(err))
		return
	}
	for _, key := range keys {
		log.Debug("indexFiles keys", zap.String("key", key))
		buildID, err := parseBuildIDFromFilePath(key)
		if err != nil {
			log.Error("garbageCollector recycleUnusedIndexFiles parseIndexFileKey", zap.String("key", key), zap.Error(err))
			continue
		}
		log.Info("garbageCollector will recycle index files", zap.Int64("buildID", buildID))
		canRecycle, segIdx := gc.meta.CleanSegmentIndex(buildID)
		if !canRecycle {
			// Even if the index is marked as deleted, the index file will not be recycled, wait for the next gc,
			// and delete all index files about the buildID at one time.
			log.Warn("garbageCollector can not recycle index files", zap.Int64("buildID", buildID))
			continue
		}
		if segIdx == nil {
			// buildID no longer exists in meta, remove all index files
			log.Info("garbageCollector recycleUnusedIndexFiles find meta has not exist, remove index files",
				zap.Int64("buildID", buildID))
			err = gc.option.cli.RemoveWithPrefix(ctx, key)
			if err != nil {
				log.Warn("garbageCollector recycleUnusedIndexFiles remove index files failed",
					zap.Int64("buildID", buildID), zap.String("prefix", key), zap.Error(err))
				continue
			}
			continue
		}
		filesMap := make(map[string]struct{})
		for _, fileID := range segIdx.IndexFileKeys {
			filepath := metautil.BuildSegmentIndexFilePath(gc.option.cli.RootPath(), segIdx.BuildID, segIdx.IndexVersion,
				segIdx.PartitionID, segIdx.SegmentID, fileID)
			filesMap[filepath] = struct{}{}
		}
		files, _, err := gc.option.cli.ListWithPrefix(ctx, key, true)
		if err != nil {
			log.Warn("garbageCollector recycleUnusedIndexFiles list files failed",
				zap.Int64("buildID", buildID), zap.String("prefix", key), zap.Error(err))
			continue
		}
		log.Info("recycle index files", zap.Int64("buildID", buildID), zap.Int("meta files num", len(filesMap)),
			zap.Int("chunkManager files num", len(files)))
		deletedFilesNum := 0
		for _, file := range files {
			if _, ok := filesMap[file]; !ok {
				if err = gc.option.cli.Remove(ctx, file); err != nil {
					log.Warn("garbageCollector recycleUnusedIndexFiles remove file failed",
						zap.Int64("buildID", buildID), zap.String("file", file), zap.Error(err))
					continue
				}
				deletedFilesNum++
			}
		}
		log.Info("index files recycle success", zap.Int64("buildID", buildID),
			zap.Int("delete index files num", deletedFilesNum))
	}
}
