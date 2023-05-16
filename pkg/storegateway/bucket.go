package storegateway

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/grafana/mimir/pkg/storegateway"
	"github.com/oklog/ulid"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"github.com/thanos-io/objstore"
	"golang.org/x/sync/errgroup"

	phlareobjstore "github.com/grafana/phlare/pkg/objstore"
	"github.com/grafana/phlare/pkg/phlaredb/block"
)

// TODO move this to a config.
const blockSyncConcurrency = 100

type BucketStore struct {
	bucket            phlareobjstore.Bucket
	tenantID, syncDir string

	logger log.Logger

	blocksMx sync.RWMutex
	blocks   map[ulid.ULID]*bucketBlock
	blockSet *bucketBlockSet

	filters []BlockMetaFilter
	metrics *Metrics
}

func NewBucketStore(bucket phlareobjstore.Bucket, tenantID string, syncDir string, filters []BlockMetaFilter, logger log.Logger, Metrics *Metrics) (*BucketStore, error) {
	s := &BucketStore{
		bucket:   phlareobjstore.BucketWithPrefix(bucket, tenantID+"/phlaredb"),
		tenantID: tenantID,
		syncDir:  syncDir,
		logger:   logger,
		filters:  filters,
		blockSet: newBucketBlockSet(),
		blocks:   map[ulid.ULID]*bucketBlock{},
		metrics:  Metrics,
	}

	if err := os.MkdirAll(syncDir, 0o750); err != nil {
		return nil, errors.Wrap(err, "create dir")
	}

	return s, nil
}

func (b *BucketStore) InitialSync(ctx context.Context) error {
	if err := b.SyncBlocks(ctx); err != nil {
		return errors.Wrap(err, "sync block")
	}

	fis, err := os.ReadDir(b.syncDir)
	if err != nil {
		return errors.Wrap(err, "read dir")
	}
	names := make([]string, 0, len(fis))
	for _, fi := range fis {
		names = append(names, fi.Name())
	}
	for _, n := range names {
		id, ok := block.IsBlockDir(n)
		if !ok {
			continue
		}
		if b := b.getBlock(id); b != nil {
			continue
		}

		// No such block loaded, remove the local dir.
		if err := os.RemoveAll(path.Join(b.syncDir, id.String())); err != nil {
			level.Warn(b.logger).Log("msg", "failed to remove block which is not needed", "err", err)
		}
	}

	return nil
}

func (s *BucketStore) getBlock(id ulid.ULID) *bucketBlock {
	s.blocksMx.RLock()
	defer s.blocksMx.RUnlock()
	return s.blocks[id]
}

func (s *BucketStore) SyncBlocks(ctx context.Context) error {
	metas, metaFetchErr := s.fetchBlocksMeta(ctx)
	// For partial view allow adding new blocks at least.
	if metaFetchErr != nil && metas == nil {
		return metaFetchErr
	}

	var wg sync.WaitGroup
	blockc := make(chan *block.Meta)

	for i := 0; i < blockSyncConcurrency; i++ {
		wg.Add(1)
		go func() {
			for meta := range blockc {
				if err := s.addBlock(ctx, meta); err != nil {
					continue
				}
			}
			wg.Done()
		}()
	}

	for id, meta := range metas {
		if b := s.getBlock(id); b != nil {
			continue
		}
		select {
		case <-ctx.Done():
		case blockc <- meta:
		}
	}

	close(blockc)
	wg.Wait()

	if metaFetchErr != nil {
		return metaFetchErr
	}

	// Drop all blocks that are no longer present in the bucket.
	for id := range s.blocks {
		if _, ok := metas[id]; ok {
			continue
		}
		if err := s.removeBlock(id); err != nil {
			level.Warn(s.logger).Log("msg", "drop of outdated block failed", "block", id, "err", err)
		}
		level.Info(s.logger).Log("msg", "dropped outdated block", "block", id)
	}

	return nil
}

func (bs *BucketStore) addBlock(_ context.Context, meta *block.Meta) (err error) {
	level.Debug(bs.logger).Log("msg", "loading new block", "id", meta.ULID)

	dir := bs.locaPath(meta.ULID.String())
	start := time.Now()
	defer func() {
		if err != nil {
			bs.metrics.blockLoadFailures.Inc()
			if err2 := os.RemoveAll(dir); err2 != nil {
				level.Warn(bs.logger).Log("msg", "failed to remove block we cannot load", "err", err2)
			}
			level.Warn(bs.logger).Log("msg", "loading block failed", "elapsed", time.Since(start), "id", meta.ULID, "err", err)
		} else {
			level.Info(bs.logger).Log("msg", "loaded new block", "elapsed", time.Since(start), "id", meta.ULID)
		}
	}()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return errors.Wrap(err, "create dir")
	}
	// add meta.json
	if _, err := meta.WriteToFile(bs.logger, dir); err != nil {
		return errors.Wrap(err, "write meta.json")
	}

	bs.metrics.blockLoads.Inc()

	bs.blocksMx.Lock()
	defer bs.blocksMx.Unlock()
	// todo create the block dir and download the index, but only download stacktraces for some blocks (14d)
	b := NewBucketBlock(meta)
	if err = bs.blockSet.add(b); err != nil {
		return errors.Wrap(err, "add block to set")
	}
	bs.blocks[meta.ULID] = b

	return nil
}

func (b *BucketStore) Stats() storegateway.BucketStoreStats {
	return storegateway.BucketStoreStats{}
}

func (s *BucketStore) removeBlock(id ulid.ULID) (returnErr error) {
	defer func() {
		if returnErr != nil {
			s.metrics.blockDropFailures.Inc()
		}
	}()

	s.blocksMx.Lock()
	b, ok := s.blocks[id]
	if ok {
		s.blockSet.remove(id)
		delete(s.blocks, id)
	}
	s.blocksMx.Unlock()

	if !ok {
		return nil
	}

	// // The block has already been removed from BucketStore, so we track it as removed
	// // even if releasing its resources could fail below.
	s.metrics.blockDrops.Inc()

	if err := b.Close(); err != nil {
		return errors.Wrap(err, "close block")
	}
	if err := os.RemoveAll(s.locaPath(id.String())); err != nil {
		return errors.Wrap(err, "delete block")
	}
	return nil
}

func (s *BucketStore) locaPath(id string) string {
	return filepath.Join(s.syncDir, id)
}

// RemoveBlocksAndClose remove all blocks from local disk and releases all resources associated with the BucketStore.
func (s *BucketStore) RemoveBlocksAndClose() error {
	// todo cleanup
	// err := s.removeAllBlocks()

	// // Release other resources even if it failed to close some blocks.
	// s.indexReaderPool.Close()

	// return err
	return nil
}

func (s *BucketStore) fetchBlocksMeta(ctx context.Context) (map[ulid.ULID]*block.Meta, error) {
	var (
		to    = time.Now()
		from  = to.Add(-time.Hour * 24 * 31)
		metas []*block.Meta
		mtx   sync.Mutex
	)
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(100)

	err := s.listAllBlock(ctx, from, to, func(name string) error {
		g.Go(func() error {
			r, err := s.bucket.Get(ctx, name+block.MetaFilename)
			if err != nil {
				return err
			}

			m, err := block.Read(r)
			if err != nil {
				return err
			}
			mtx.Lock()
			metas = append(metas, m)
			mtx.Unlock()

			if err := r.Close(); err != nil {
				return err
			}
			return nil
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	metaMap := lo.SliceToMap(metas, func(item *block.Meta) (ulid.ULID, *block.Meta) {
		return item.ULID, item
	})
	if len(metaMap) == 0 {
		return nil, nil
	}
	for _, filter := range s.filters {
		// NOTE: filter can update synced metric accordingly to the reason of the exclude.
		// todo: wire up the filter with the metrics.
		if err := filter.Filter(ctx, metaMap, s.metrics.Synced); err != nil {
			return nil, errors.Wrap(err, "filter metas")
		}
	}
	return metaMap, nil
}

func (s *BucketStore) listAllBlock(ctx context.Context, from, to time.Time, cb func(name string) error) error {
	// todo: We should cache prefixes listing per tenants.
	blockPrefixes, err := blockPrefixesFromTo(from, to, 4)
	if err != nil {
		return err
	}
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(10)
	for _, prefix := range blockPrefixes {
		prefix := prefix
		g.Go(func() error {
			level.Debug(s.logger).Log("msg", "listing blocks", "prefix", prefix)
			return s.bucket.Iter(ctx, prefix, func(name string) error {
				if err := cb(name); err != nil {
					return err
				}
				return nil
			}, objstore.WithoutApendingDirDelim)
		})
	}
	return nil
}

// orderOfSplit is the number of bytes of the ulid id used for the split. The duration of the split is:
// 0: 1114y
// 1: 34.8y
// 2: 1y
// 3: 12.4d
// 4: 9h19m
// TODO: To needs to be adapted based on the MaxBlockDuration.
func blockPrefixesFromTo(from, to time.Time, orderOfSplit uint8) (prefixes []string, err error) {
	var id ulid.ULID

	if orderOfSplit > 9 {
		return nil, fmt.Errorf("order of split must be between 0 and 9")
	}

	byteShift := (9 - orderOfSplit) * 5

	ms := uint64(from.UnixMilli()) >> byteShift
	ms = ms << byteShift
	for ms <= uint64(to.UnixMilli()) {
		if err := id.SetTime(ms); err != nil {
			return nil, err
		}
		prefixes = append(prefixes, id.String()[:orderOfSplit+1])

		ms = ms >> byteShift
		ms += 1
		ms = ms << byteShift
	}

	return prefixes, nil
}

// bucketBlockSet holds all blocks.
type bucketBlockSet struct {
	mtx    sync.RWMutex
	blocks []*bucketBlock // Blocks sorted by mint, then maxt.
}

// newBucketBlockSet initializes a new set with the known downsampling windows hard-configured.
// (Mimir only supports no-downsampling)
// The set currently does not support arbitrary ranges.
func newBucketBlockSet() *bucketBlockSet {
	return &bucketBlockSet{}
}

func (s *bucketBlockSet) add(b *bucketBlock) error {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	s.blocks = append(s.blocks, b)

	// Always sort blocks by min time, then max time.
	sort.Slice(s.blocks, func(j, k int) bool {
		if s.blocks[j].Meta.MinTime == s.blocks[k].Meta.MinTime {
			return s.blocks[j].Meta.MaxTime < s.blocks[k].Meta.MaxTime
		}
		return s.blocks[j].Meta.MinTime < s.blocks[k].Meta.MinTime
	})
	return nil
}

func (s *bucketBlockSet) remove(id ulid.ULID) {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	for i, b := range s.blocks {
		if b.Meta.ULID != id {
			continue
		}
		s.blocks = append(s.blocks[:i], s.blocks[i+1:]...)
		return
	}
}

// getFor returns a time-ordered list of blocks that cover date between mint and maxt.
// It supports overlapping blocks.
//
// NOTE: s.blocks are expected to be sorted in minTime order.
func (s *bucketBlockSet) getFor(mint, maxt int64) (bs []*bucketBlock) {
	if mint > maxt {
		return nil
	}

	s.mtx.RLock()
	defer s.mtx.RUnlock()

	// Fill the given interval with the blocks within the request mint and maxt.
	for _, b := range s.blocks {
		if int64(b.Meta.MaxTime) <= mint {
			continue
		}
		// NOTE: Block intervals are half-open: [b.MinTime, b.MaxTime).
		if int64(b.Meta.MinTime) > maxt {
			break
		}

		bs = append(bs, b)
	}

	return bs
}
