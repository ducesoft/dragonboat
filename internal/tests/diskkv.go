// Copyright 2017-2019 Lei Ni (nilei81@gmail.com)
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

package tests

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/lni/dragonboat/internal/logdb/gorocksdb"
	"github.com/lni/dragonboat/internal/tests/kvpb"
	"github.com/lni/dragonboat/internal/utils/fileutil"
	"github.com/lni/dragonboat/internal/utils/logutil"
	"github.com/lni/dragonboat/internal/utils/random"
	sm "github.com/lni/dragonboat/statemachine"
)

const (
	appliedIndexKey    string = "disk_kv_applied_index"
	dbNamePrefix       string = "test_rocksdb_db_dir_"
	currentDBFilename  string = "current"
	updatingDBFilename string = "current.updating"
)

type rocksdb struct {
	mu     sync.RWMutex
	db     *gorocksdb.DB
	ro     *gorocksdb.ReadOptions
	wo     *gorocksdb.WriteOptions
	opts   *gorocksdb.Options
	closed bool
}

func (r *rocksdb) lookup(query []byte) ([]byte, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return nil, errors.New("db already closed")
	}
	val, err := r.db.Get(r.ro, query)
	if err != nil {
		return nil, err
	}
	defer val.Free()
	data := val.Data()
	if len(data) == 0 {
		return []byte(""), nil
	}
	v := make([]byte, len(data))
	copy(v, data)
	return v, nil
}

func (r *rocksdb) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	if r.db != nil {
		r.db.Close()
	}
	if r.opts != nil {
		r.opts.Destroy()
	}
	if r.wo != nil {
		r.wo.Destroy()
	}
	if r.ro != nil {
		r.ro.Destroy()
	}
	r.db = nil
}

func createDB(dbdir string) (*rocksdb, error) {
	bbto := gorocksdb.NewDefaultBlockBasedTableOptions()
	bbto.SetWholeKeyFiltering(true)
	bbto.SetBlockSize(1024)
	bbto.SetNoBlockCache(true)
	opts := gorocksdb.NewDefaultOptions()
	opts.SetBlockBasedTableFactory(bbto)
	opts.SetCreateIfMissing(true)
	opts.SetUseFsync(true)
	opts.SetCompression(gorocksdb.NoCompression)
	// rocksdb perallocates size for its log file and the size is calculated
	// based on the write buffer size.
	opts.SetWriteBufferSize(1024)
	wo := gorocksdb.NewDefaultWriteOptions()
	wo.SetSync(true)
	ro := gorocksdb.NewDefaultReadOptions()
	ro.SetFillCache(false)
	ro.SetTotalOrderSeek(true)
	ro.IgnoreRangeDeletions(true)
	db, err := gorocksdb.OpenDb(opts, dbdir)
	if err != nil {
		return nil, err
	}
	return &rocksdb{
		db:   db,
		ro:   ro,
		wo:   wo,
		opts: opts,
	}, nil
}

func isNewRun(dir string) bool {
	fp := path.Join(dir, currentDBFilename)
	if _, err := os.Stat(fp); os.IsNotExist(err) {
		return true
	}
	return false
}

func getNodeDBDirName(clusterID uint64, nodeID uint64) string {
	part := "%d_%d"
	return fmt.Sprintf(dbNamePrefix+part, clusterID, nodeID)
}

func getNewRandomDBDirName(dir string) string {
	part := "_%d_%d"
	rn := random.LockGuardedRand.Uint64()
	ct := time.Now().UnixNano()
	return path.Join(dir, fmt.Sprintf(dir+part, rn, ct))
}

func replaceCurrentDBFile(dir string) error {
	fp := path.Join(dir, currentDBFilename)
	tmpFp := path.Join(dir, updatingDBFilename)
	if err := os.Rename(tmpFp, fp); err != nil {
		return err
	}
	return fileutil.SyncDir(dir)
}

func saveCurrentDBDirName(dir string, dbdir string) error {
	h := md5.New()
	if _, err := h.Write([]byte(dbdir)); err != nil {
		return err
	}
	fp := path.Join(dir, updatingDBFilename)
	f, err := os.Create(fp)
	if err != nil {
		return err
	}
	defer func() {
		if err := f.Close(); err != nil {
			panic(err)
		}
		if err := fileutil.SyncDir(dir); err != nil {
			panic(err)
		}
	}()
	if _, err := f.Write(h.Sum(nil)[:8]); err != nil {
		return err
	}
	if _, err := f.Write([]byte(dbdir)); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	return nil
}

func getCurrentDBDirName(dir string) (string, error) {
	fp := path.Join(dir, currentDBFilename)
	f, err := os.OpenFile(fp, os.O_RDONLY, 0755)
	if err != nil {
		return "", err
	}
	defer func() {
		if err := f.Close(); err != nil {
			panic(err)
		}
	}()
	data, err := ioutil.ReadAll(f)
	if err != nil {
		return "", err
	}
	if len(data) <= 8 {
		panic("corrupted content")
	}
	crc := data[:8]
	content := data[8:]
	h := md5.New()
	if _, err := h.Write(content); err != nil {
		return "", err
	}
	if !bytes.Equal(crc, h.Sum(nil)[:8]) {
		panic("corrupted content with not matched crc")
	}
	return string(content), nil
}

func createNodeDataDir(dir string) error {
	return os.MkdirAll(dir, 0755)
}

func cleanupNodeDataDir(dir string) error {
	os.RemoveAll(path.Join(dir, updatingDBFilename))
	dbdir, err := getCurrentDBDirName(dir)
	if err != nil {
		return err
	}
	files, err := ioutil.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, fi := range files {
		if !fi.IsDir() {
			continue
		}
		fmt.Printf("dbdir %s, fi.name %s, dir %s\n", dbdir, fi.Name(), dir)
		toDelete := path.Join(dir, fi.Name())
		if toDelete != dbdir {
			fmt.Printf("removing %s\n", toDelete)
			if err := os.RemoveAll(toDelete); err != nil {
				return err
			}
		}
	}
	return nil
}

type DiskKVTest struct {
	clusterID uint64
	nodeID    uint64
	db        unsafe.Pointer
	closed    bool
	aborted   bool
}

func NewDiskKVTest(clusterID uint64, nodeID uint64) sm.IOnDiskStateMachine {
	d := &DiskKVTest{
		clusterID: clusterID,
		nodeID:    nodeID,
	}
	fmt.Printf("[DKVE] %s is being created\n", d.describe())
	return d
}

func (s *DiskKVTest) describe() string {
	id := logutil.DescribeNode(s.clusterID, s.nodeID)
	return fmt.Sprintf("%s %s", time.Now().Format("2006-01-02 15:04:05.000000"), id)
}

func (d *DiskKVTest) Open() (uint64, error) {
	fmt.Printf("[DKVE] %s is being opened\n", d.describe())
	generateRandomDelay()
	dir := getNodeDBDirName(d.clusterID, d.nodeID)
	createNodeDataDir(dir)
	var dbdir string
	if !isNewRun(dir) {
		if err := cleanupNodeDataDir(dir); err != nil {
			return 0, err
		}
		var err error
		dbdir, err = getCurrentDBDirName(dir)
		if err != nil {
			return 0, err
		}
		if _, err := os.Stat(dbdir); err != nil {
			if os.IsNotExist(err) {
				panic("db dir unexpectedly deleted")
			}
		}
		fmt.Printf("[DKVE] %s being re-opened at %s\n", d.describe(), dbdir)
	} else {
		fmt.Printf("[DKVE] %s doing a new run\n", d.describe())
		dbdir = getNewRandomDBDirName(dir)
		if err := saveCurrentDBDirName(dir, dbdir); err != nil {
			return 0, err
		}
		if err := replaceCurrentDBFile(dir); err != nil {
			return 0, err
		}
	}
	fmt.Printf("[DKVE] %s going to create db at %s\n", d.describe(), dbdir)
	db, err := createDB(dbdir)
	if err != nil {
		fmt.Printf("[DKVE] %s failed to create db\n", d.describe())
		return 0, err
	}
	fmt.Printf("[DKVE] %s returned from create db\n", d.describe())
	atomic.SwapPointer(&d.db, unsafe.Pointer(db))
	val, err := db.db.Get(db.ro, []byte(appliedIndexKey))
	if err != nil {
		fmt.Printf("[DKVE] %s failed to query applied index\n", d.describe())
		return 0, err
	}
	defer val.Free()
	data := val.Data()
	if len(data) == 0 {
		fmt.Printf("[DKVE] %s does not have applied index stored yet\n", d.describe())
		return 0, nil
	}
	v := binary.LittleEndian.Uint64(data)
	fmt.Printf("[DKVE] %s opened its disk sm, index %d\n", d.describe(), v)
	return v, nil
}

func (d *DiskKVTest) Lookup(key []byte) ([]byte, error) {
	db := (*rocksdb)(atomic.LoadPointer(&d.db))
	if db != nil {
		v, err := db.lookup(key)
		if err == nil && d.closed {
			panic("lookup returned valid result when DiskKVTest is already closed")
		}
		return v, err
	}
	return nil, errors.New("db closed")
}

func (d *DiskKVTest) Update(ents []sm.Entry) []sm.Entry {
	if d.aborted {
		panic("update() called after abort set to true")
	}
	if d.closed {
		panic("update called after Close()")
	}
	generateRandomDelay()
	wb := gorocksdb.NewWriteBatch()
	defer wb.Destroy()
	db := (*rocksdb)(atomic.LoadPointer(&d.db))
	for idx, e := range ents {
		dataKv := &kvpb.PBKV{}
		if err := dataKv.Unmarshal(e.Cmd); err != nil {
			panic(err)
		}
		key := dataKv.GetKey()
		val := dataKv.GetVal()
		wb.Put([]byte(key), []byte(val))
		ents[idx].Result = uint64(len(ents[idx].Cmd))
	}
	idx := make([]byte, 8)
	binary.LittleEndian.PutUint64(idx, ents[len(ents)-1].Index)
	wb.Put([]byte(appliedIndexKey), idx)
	fmt.Printf("[DKVE] %s applied index recorded as %d\n", d.describe(), ents[len(ents)-1].Index)
	if err := db.db.Write(db.wo, wb); err != nil {
		panic(err)
	}
	return ents
}

type diskKVCtx struct {
	db       *rocksdb
	snapshot *gorocksdb.Snapshot
}

func (d *DiskKVTest) PrepareSnapshot() (interface{}, error) {
	if d.closed {
		panic("prepare snapshot called after Close()")
	}
	if d.aborted {
		panic("prepare snapshot called after abort")
	}
	db := (*rocksdb)(atomic.LoadPointer(&d.db))
	return &diskKVCtx{
		db:       db,
		snapshot: db.db.NewSnapshot(),
	}, nil
}

func iteratorIsValid(iter *gorocksdb.Iterator) bool {
	v, err := iter.IsValid()
	if err != nil {
		panic(err)
	}
	return v
}

func (d *DiskKVTest) saveToWriter(db *rocksdb,
	ss *gorocksdb.Snapshot, w io.Writer) (uint64, error) {
	ro := gorocksdb.NewDefaultReadOptions()
	ro.SetSnapshot(ss)
	ro.SetFillCache(false)
	ro.SetTotalOrderSeek(true)
	ro.IgnoreRangeDeletions(true)
	iter := db.db.NewIterator(ro)
	defer iter.Close()
	count := uint64(0)
	for iter.SeekToFirst(); iteratorIsValid(iter); iter.Next() {
		count++
	}
	fmt.Printf("[DKVE] %s have %d pairs of KV\n", d.describe(), count)
	total := uint64(0)
	sz := make([]byte, 8)
	binary.LittleEndian.PutUint64(sz, count)
	if _, err := w.Write(sz); err != nil {
		return 0, err
	}
	total += 8
	for iter.SeekToFirst(); iteratorIsValid(iter); iter.Next() {
		key, ok := iter.OKey()
		if !ok {
			panic("failed to get key")
		}
		val, ok := iter.OValue()
		if !ok {
			panic("failed to get value")
		}
		dataKv := &kvpb.PBKV{
			Key: string(key.Data()),
			Val: string(val.Data()),
		}
		if dataKv.Key == appliedIndexKey {
			v := binary.LittleEndian.Uint64([]byte(dataKv.Val))
			fmt.Printf("[DKVE] %s saving appliedIndexKey as %d\n", d.describe(), v)
		}
		data, err := dataKv.Marshal()
		if err != nil {
			panic(err)
		}
		binary.LittleEndian.PutUint64(sz, uint64(len(data)))
		if _, err := w.Write(sz); err != nil {
			return 0, err
		}
		total += 8
		if _, err := w.Write(data); err != nil {
			return 0, err
		}
		total += uint64(len(data))
	}
	return total, nil
}

func (d *DiskKVTest) CreateSnapshot(ctx interface{},
	w io.Writer, done <-chan struct{}) (uint64, error) {
	if d.closed {
		panic("prepare snapshot called after Close()")
	}
	if d.aborted {
		panic("prepare snapshot called after abort")
	}
	delay := getLargeRandomDelay()
	fmt.Printf("random delay %d ms\n", delay)
	for delay > 0 {
		delay -= 10
		time.Sleep(10 * time.Millisecond)
		select {
		case <-done:
			return 0, sm.ErrSnapshotStopped
		default:
		}
	}
	ctxdata := ctx.(*diskKVCtx)
	db := ctxdata.db
	db.mu.RLock()
	defer db.mu.RUnlock()
	ss := ctxdata.snapshot
	return d.saveToWriter(db, ss, w)
}

func (d *DiskKVTest) RecoverFromSnapshot(r io.Reader,
	done <-chan struct{}) error {
	if d.closed {
		panic("recover from snapshot called after Close()")
	}
	delay := getLargeRandomDelay()
	fmt.Printf("random delay %d ms\n", delay)
	for delay > 0 {
		delay -= 10
		time.Sleep(10 * time.Millisecond)
		select {
		case <-done:
			d.aborted = true
			return sm.ErrSnapshotStopped
		default:
		}
	}
	dir := getNodeDBDirName(d.clusterID, d.nodeID)
	dbdir := getNewRandomDBDirName(dir)
	oldDirName, err := getCurrentDBDirName(dir)
	if err != nil {
		return err
	}
	fmt.Printf("[DKVE] %s is creating a tmp db at %s\n", d.describe(), dbdir)
	db, err := createDB(dbdir)
	if err != nil {
		return err
	}
	sz := make([]byte, 8)
	if _, err := io.ReadFull(r, sz); err != nil {
		return err
	}
	total := binary.LittleEndian.Uint64(sz)
	fmt.Printf("[DKVE] %s recovering from a snapshot with %d pairs of KV\n", d.describe(), total)
	wb := gorocksdb.NewWriteBatch()
	for i := uint64(0); i < total; i++ {
		if _, err := io.ReadFull(r, sz); err != nil {
			return err
		}
		toRead := binary.LittleEndian.Uint64(sz)
		data := make([]byte, toRead)
		if _, err := io.ReadFull(r, data); err != nil {
			return err
		}
		dataKv := &kvpb.PBKV{}
		if err := dataKv.Unmarshal(data); err != nil {
			panic(err)
		}
		if dataKv.Key == appliedIndexKey {
			v := binary.LittleEndian.Uint64([]byte(dataKv.Val))
			fmt.Printf("[DKVE] %s recovering appliedIndexKey to %d\n", d.describe(), v)
		}
		wb.Put([]byte(dataKv.Key), []byte(dataKv.Val))
	}
	if err := db.db.Write(db.wo, wb); err != nil {
		return err
	}
	if err := saveCurrentDBDirName(dir, dbdir); err != nil {
		return err
	}
	if err := replaceCurrentDBFile(dir); err != nil {
		return err
	}
	fmt.Printf("[DKVE] %s replaced db %s with %s\n", d.describe(), oldDirName, dbdir)
	old := (*rocksdb)(atomic.SwapPointer(&d.db, unsafe.Pointer(db)))
	if old != nil {
		old.close()
	}
	fmt.Printf("[DKVE] %s to delete olddb at %s\n", d.describe(), oldDirName)
	return os.RemoveAll(oldDirName)
}

func (d *DiskKVTest) Close() {
	fmt.Printf("[DKVE] %s called close\n", d.describe())
	db := (*rocksdb)(atomic.SwapPointer(&d.db, unsafe.Pointer(nil)))
	if db != nil {
		d.closed = true
		db.close()
	} else {
		if d.closed {
			panic("close called twice")
		}
	}
}

func (d *DiskKVTest) GetHash() uint64 {
	fmt.Printf("[DKVE] %s called GetHash\n", d.describe())
	h := md5.New()
	db := (*rocksdb)(atomic.LoadPointer(&d.db))
	ss := db.db.NewSnapshot()
	db.mu.RLock()
	defer db.mu.RUnlock()
	if _, err := d.saveToWriter(db, ss, h); err != nil {
		panic(err)
	}
	md5sum := h.Sum(nil)
	return binary.LittleEndian.Uint64(md5sum[:8])
}