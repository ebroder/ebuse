package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/abligh/gonbdserver/nbd"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ebs"
	lru "github.com/hashicorp/golang-lru"
	"golang.org/x/sync/singleflight"
)

// SnapshotBackend is a backend for gonbdserver backed by an EBS snapshot. It
// caches up to 50M of block data (mostly because NBD sectors are much smaller
// than EBS sectors, so we see a lot of successive reads for the same EBS
// block), and automatically keeps metadata about the snapshot up to date.
type SnapshotBackend struct {
	client   *ebs.EBS
	snapshot string
	exit     chan interface{}

	metadata  *snapshotMetadata
	blockData *blockData

	blockFetchGroup singleflight.Group
	blockCache      *lru.ARCCache
}

type snapshotMetadata struct {
	volumeSize int64 // in gigabytes
	blockSize  int64
}

type blockData struct {
	expiration  time.Time
	blockTokens map[int64]*string
}

// NewSnapshotBackend creates a new SnapshotBackend and initializes the internal
// cache of block data. Callers must call Close or this will leak a goroutine.
func NewSnapshotBackend(initCtx context.Context, client *ebs.EBS, snapshot string) (nbd.Backend, error) {
	blockCache, err := lru.NewARC(100)
	if err != nil {
		panic(err)
	}

	sd := &SnapshotBackend{
		client:     client,
		snapshot:   snapshot,
		exit:       make(chan interface{}),
		blockCache: blockCache,
	}
	metadata, blockData, err := sd.fetchMetadata(initCtx)
	if err != nil {
		return nil, err
	}
	sd.metadata = metadata
	sd.blockData = blockData
	log.Printf("Fetched snapshot metadata: volumeSize=%d", sd.metadata.volumeSize)

	go sd.refreshLoop(context.Background())
	return sd, nil
}

// refreshLoop periodically refreshes metadata about the snapshot as the
// metadata expires. We also rely on the refresh loop to populate initial
// metadata about the snapshot, like block size (which hopefully doesn't change)
func (sd *SnapshotBackend) refreshLoop(ctx context.Context) {
	for {
		// build in some buffer
		wait := time.Until(sd.blockData.expiration) - 5*time.Minute
		select {
		case <-ctx.Done():
			return
		case <-sd.exit:
			return
		case <-time.After(wait):
		}

		metadata, blockData, err := sd.fetchMetadata(ctx)
		if err != nil {
			panic(err)
		}

		// Verify that metadata hasn't changed (which would be weird)
		if sd.metadata.blockSize != metadata.blockSize || sd.metadata.volumeSize != metadata.volumeSize {
			panic(fmt.Errorf("block size or volume size changed: oldblock=%d newblock=%d oldvolume=%d newvolume=%d",
				sd.metadata.blockSize, metadata.blockSize, sd.metadata.volumeSize, metadata.volumeSize))
		}

		atomic.SwapPointer((*unsafe.Pointer)(unsafe.Pointer(&sd.blockData)), unsafe.Pointer(blockData))
	}
}

func (sd *SnapshotBackend) fetchMetadata(ctx context.Context) (*snapshotMetadata, *blockData, error) {
	log.Println("Fetching block metadata")

	expiration := time.Unix(1<<63-62135596801, 999999999)
	volumeSize := int64(0)
	blockSize := int64(0)

	blockTokens := make(map[int64]*string)

	err := sd.client.ListSnapshotBlocksPagesWithContext(ctx, &ebs.ListSnapshotBlocksInput{
		SnapshotId: aws.String(sd.snapshot),
	}, func(listOutput *ebs.ListSnapshotBlocksOutput, _ bool) bool {
		volumeSize = *listOutput.VolumeSize
		blockSize = *listOutput.BlockSize

		if listOutput.ExpiryTime.Before(expiration) {
			expiration = *listOutput.ExpiryTime
		}

		for _, b := range listOutput.Blocks {
			blockTokens[*b.BlockIndex] = b.BlockToken
		}

		return true
	})

	if err != nil {
		return nil, nil, err
	}

	log.Printf("Finished fetching block metadata: expiration=%s", expiration)

	return &snapshotMetadata{
			volumeSize: volumeSize,
			blockSize:  blockSize,
		}, &blockData{
			expiration:  expiration,
			blockTokens: blockTokens,
		}, nil
}

func (sd *SnapshotBackend) readBlock(ctx context.Context, block int64) ([]byte, error) {
	data, err, _ := sd.blockFetchGroup.Do(string(block), func() (interface{}, error) {
		if v, ok := sd.blockCache.Get(block); ok {
			return v, nil
		}

		out, err := sd.client.GetSnapshotBlockWithContext(ctx, &ebs.GetSnapshotBlockInput{
			BlockIndex: &block,
			BlockToken: sd.blockData.blockTokens[block],
			SnapshotId: &sd.snapshot,
		})
		if err != nil {
			log.Printf("error fetching snapshot block: block=%d err=%s", block, err)
			return nil, err
		}

		data, err := ioutil.ReadAll(out.BlockData)
		if err != nil {
			log.Printf("error reading snapshot block data: block=%d err=%s", block, err)
			return nil, err
		}
		log.Printf("fetched block: block=%d len=%d", block, len(data))

		sd.blockCache.Add(block, data)
		return data, nil
	})
	return data.([]byte), err
}

// ReadAt implements nbd.Backend.ReadAt
func (sd *SnapshotBackend) ReadAt(ctx context.Context, b []byte, offset int64) (int, error) {
	bOff := 0
	start := offset
	end := offset + int64(len(b))
	for offset < end {
		block := offset / int64(sd.metadata.blockSize)
		data, err := sd.readBlock(ctx, block)
		if err != nil {
			return bOff, err
		}

		skip := offset - block*sd.metadata.blockSize
		if skip > 0 {
			data = data[skip:]
		}

		n := copy(b[bOff:], data)
		bOff += n
		offset += int64(n)
	}

	return int(offset - start), nil
}

// WriteAt implements nbd.Backend.WriteAt, although we don't support writes so
// this is an immediate error.
func (sd *SnapshotBackend) WriteAt(ctx context.Context, b []byte, offset int64, fua bool) (int, error) {
	return 0, syscall.ENOSYS
}

// TrimAt implements nbd.Backend.TrimAt, although we don't support writes
// (including TRIM) so this is an immediate error
func (sd *SnapshotBackend) TrimAt(ctx context.Context, length int, offset int64) (int, error) {
	return 0, syscall.ENOSYS
}

// Flush implements nbd.Backend.Flush, although we don't support writes so this
// is an immediate error.
func (sd *SnapshotBackend) Flush(ctx context.Context) error {
	return syscall.ENOSYS
}

// Close implements nbd.Backend.Close. The only resource we have to garbage
// collect is the background refresh thread.
func (sd *SnapshotBackend) Close(ctx context.Context) error {
	close(sd.exit)
	return nil
}

// Geometry implements nbd.Backend.Geometry
func (sd *SnapshotBackend) Geometry(ctx context.Context) (uint64, uint64, uint64, uint64, error) {
	return uint64(sd.metadata.volumeSize) * 1024 * 1024 * 1024,
		1,
		uint64(sd.metadata.blockSize),
		uint64(sd.metadata.blockSize),
		nil
}

// HasFua implements nbd.Backend.HasFua
func (sd *SnapshotBackend) HasFua(ctx context.Context) bool {
	return false
}

// HasFlush implements nbd.Backend.HasFlush
func (sd *SnapshotBackend) HasFlush(ctx context.Context) bool {
	return false
}
