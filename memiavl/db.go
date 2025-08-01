package memiavl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alitto/pond"
	"github.com/tidwall/wal"
)

const (
	DefaultSnapshotInterval    = 1000
	LockFileName               = "LOCK"
	DefaultSnapshotWriterLimit = 4
	TmpSuffix                  = "-tmp"
)

var errReadOnly = errors.New("db is read-only")

// DB implements DB-like functionalities on top of MultiTree:
// - async snapshot rewriting
// - Write-ahead-log
//
// The memiavl.db directory looks like this:
// ```
// > current -> snapshot-N
// > snapshot-N
// >  bank
// >    kvs
// >    nodes
// >    metadata
// >  acc
// >  ... other stores
// > wal
// ```
type DB struct {
	MultiTree
	dir      string
	logger   Logger
	fileLock FileLock
	readOnly bool

	// result channel of snapshot rewrite goroutine
	snapshotRewriteChan chan snapshotResult
	// context cancel function to cancel the snapshot rewrite goroutine
	snapshotRewriteCancel context.CancelFunc

	// the number of old snapshots to keep (excluding the latest one)
	snapshotKeepRecent uint32
	// block interval to take a new snapshot
	snapshotInterval uint32
	// make sure only one snapshot rewrite is running
	pruneSnapshotLock      sync.Mutex
	triggerStateSyncExport func(height int64)

	// invariant: the LastIndex always match the current version of MultiTree
	wal         *wal.Log
	walChanSize int
	walChan     chan *walEntry
	walQuit     chan error

	// pending changes, will be written into WAL in next Commit call
	pendingLog WALEntry

	// The assumptions to concurrency:
	// - The methods on DB are protected by a mutex
	// - Each call of Load loads a separate instance, in query scenarios,
	//   it should be immutable, the cache stores will handle the temporary writes.
	// - The DB for the state machine will handle writes through the Commit call,
	//   this method is the sole entry point for tree modifications, and there's no concurrency internally
	//   (the background snapshot rewrite is handled separately), so we don't need locks in the Tree.
	mtx sync.Mutex
	// worker goroutine IdleTimeout = 5s
	snapshotWriterPool *pond.WorkerPool

	// reusable write batch
	wbatch wal.Batch
}

type Options struct {
	Logger          Logger
	CreateIfMissing bool
	InitialVersion  uint32
	ReadOnly        bool
	// the initial stores when initialize the empty instance
	InitialStores          []string
	SnapshotKeepRecent     uint32
	SnapshotInterval       uint32
	TriggerStateSyncExport func(height int64)
	// load the target version instead of latest version
	TargetVersion uint32
	// Buffer size for the asynchronous commit queue, -1 means synchronous commit,
	// default to 0.
	AsyncCommitBuffer int
	// ZeroCopy if true, the get and iterator methods could return a slice pointing to mmaped blob files.
	ZeroCopy bool
	// CacheSize defines the cache's max entry size for each memiavl store.
	CacheSize int
	// LoadForOverwriting if true rollbacks the state, specifically the Load method will
	// truncate the versions after the `TargetVersion`, the `TargetVersion` becomes the latest version.
	// it do nothing if the target version is `0`.
	LoadForOverwriting bool

	SnapshotWriterLimit int
}

func (opts Options) Validate() error {
	if opts.ReadOnly && opts.CreateIfMissing {
		return errors.New("can't create db in read-only mode")
	}

	if opts.ReadOnly && opts.LoadForOverwriting {
		return errors.New("can't rollback db in read-only mode")
	}

	return nil
}

func (opts *Options) FillDefaults() {
	if opts.Logger == nil {
		opts.Logger = NewNopLogger()
	}

	if opts.SnapshotInterval == 0 {
		opts.SnapshotInterval = DefaultSnapshotInterval
	}

	if opts.SnapshotWriterLimit <= 0 {
		opts.SnapshotWriterLimit = DefaultSnapshotWriterLimit
	}
}

const (
	SnapshotPrefix = "snapshot-"
	SnapshotDirLen = len(SnapshotPrefix) + 20
)

func Load(dir string, opts Options) (*DB, error) {
	if err := opts.Validate(); err != nil {
		return nil, fmt.Errorf("invalid options: %w", err)
	}
	opts.FillDefaults()

	if opts.CreateIfMissing {
		if err := createDBIfNotExist(dir, opts.InitialVersion); err != nil {
			return nil, fmt.Errorf("fail to load db: %w", err)
		}
	}

	var (
		err      error
		fileLock FileLock
	)
	if !opts.ReadOnly {
		fileLock, err = LockFile(filepath.Join(dir, LockFileName))
		if err != nil {
			return nil, fmt.Errorf("fail to lock db: %w", err)
		}

		// cleanup any temporary directories left by interrupted snapshot rewrite
		if err := removeTmpDirs(dir); err != nil {
			return nil, fmt.Errorf("fail to cleanup tmp directories: %w", err)
		}
	}

	snapshot := "current"
	if opts.TargetVersion > 0 {
		// find the biggest snapshot version that's less than or equal to the target version
		snapshotVersion, err := seekSnapshot(dir, opts.TargetVersion)
		if err != nil {
			return nil, fmt.Errorf("fail to seek snapshot: %w", err)
		}
		snapshot = snapshotName(snapshotVersion)
	}

	path := filepath.Join(dir, snapshot)
	mtree, err := LoadMultiTree(path, opts.ZeroCopy, opts.CacheSize)
	if err != nil {
		return nil, err
	}

	wal, err := OpenWAL(walPath(dir), &wal.Options{NoCopy: true, NoSync: true})
	if err != nil {
		return nil, err
	}

	if opts.TargetVersion == 0 || int64(opts.TargetVersion) > mtree.Version() {
		if err := mtree.CatchupWAL(wal, int64(opts.TargetVersion)); err != nil {
			return nil, errors.Join(err, wal.Close())
		}
	}

	if opts.LoadForOverwriting && opts.TargetVersion > 0 {
		currentSnapshot, err := os.Readlink(currentPath(dir))
		if err != nil {
			return nil, fmt.Errorf("fail to read current version: %w", err)
		}

		if snapshot != currentSnapshot {
			// downgrade `"current"` link first
			opts.Logger.Info("downgrade current link to", "snapshot", snapshot)
			if err := updateCurrentSymlink(dir, snapshot); err != nil {
				return nil, fmt.Errorf("fail to update current snapshot link: %w", err)
			}
		}

		// truncate the WAL
		opts.Logger.Info("truncate WAL from back", "version", opts.TargetVersion)
		if err := wal.TruncateBack(walIndex(int64(opts.TargetVersion), mtree.initialVersion)); err != nil {
			return nil, fmt.Errorf("fail to truncate wal logs: %w", err)
		}

		// prune snapshots that's larger than the target version
		if err := traverseSnapshots(dir, false, func(version int64) (bool, error) {
			if version <= int64(opts.TargetVersion) {
				return true, nil
			}

			if err := atomicRemoveDir(filepath.Join(dir, snapshotName(version))); err != nil {
				opts.Logger.Error("fail to prune snapshot", "version", version, "err", err)
			} else {
				opts.Logger.Info("prune snapshot", "version", version)
			}
			return false, nil
		}); err != nil {
			return nil, fmt.Errorf("fail to prune snapshots: %w", err)
		}
	}
	// create worker pool. recv tasks to write snapshot
	workerPool := pond.New(opts.SnapshotWriterLimit, opts.SnapshotWriterLimit*10)

	db := &DB{
		MultiTree:              *mtree,
		logger:                 opts.Logger,
		dir:                    dir,
		fileLock:               fileLock,
		readOnly:               opts.ReadOnly,
		wal:                    wal,
		walChanSize:            opts.AsyncCommitBuffer,
		snapshotKeepRecent:     opts.SnapshotKeepRecent,
		snapshotInterval:       opts.SnapshotInterval,
		triggerStateSyncExport: opts.TriggerStateSyncExport,
		snapshotWriterPool:     workerPool,
	}

	if !db.readOnly && db.Version() == 0 && len(opts.InitialStores) > 0 {
		// do the initial upgrade with the `opts.InitialStores`
		var upgrades []*TreeNameUpgrade
		for _, name := range opts.InitialStores {
			upgrades = append(upgrades, &TreeNameUpgrade{Name: name})
		}
		if err := db.ApplyUpgrades(upgrades); err != nil {
			return nil, errors.Join(err, db.Close())
		}
	}

	return db, nil
}

func removeTmpDirs(rootDir string) error {
	entries, err := os.ReadDir(rootDir)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasSuffix(entry.Name(), TmpSuffix) {
			continue
		}

		if err := os.RemoveAll(filepath.Join(rootDir, entry.Name())); err != nil {
			return err
		}
	}

	return nil
}

// ReadOnly returns whether the DB is opened in read-only mode.
func (db *DB) ReadOnly() bool {
	return db.readOnly
}

// SetInitialVersion wraps `MultiTree.SetInitialVersion`.
// it do an immediate snapshot rewrite, because we can't use wal log to record this change,
// because we need it to convert versions to wal index in the first place.
func (db *DB) SetInitialVersion(initialVersion int64) error {
	db.mtx.Lock()
	defer db.mtx.Unlock()

	if db.readOnly {
		return errReadOnly
	}

	if db.lastCommitInfo.Version > 0 {
		return errors.New("initial version can only be set before any commit")
	}

	if err := db.MultiTree.SetInitialVersion(initialVersion); err != nil {
		return err
	}

	return initEmptyDB(db.dir, db.initialVersion)
}

// ApplyUpgrades wraps MultiTree.ApplyUpgrades, it also append the upgrades in a pending log,
// which will be persisted to the WAL in next Commit call.
func (db *DB) ApplyUpgrades(upgrades []*TreeNameUpgrade) error {
	db.mtx.Lock()
	defer db.mtx.Unlock()

	if db.readOnly {
		return errReadOnly
	}

	if err := db.MultiTree.ApplyUpgrades(upgrades); err != nil {
		return err
	}

	db.pendingLog.Upgrades = append(db.pendingLog.Upgrades, upgrades...)
	return nil
}

// ApplyChangeSets wraps MultiTree.ApplyChangeSets, it also append the changesets in the pending log,
// which will be persisted to the WAL in next Commit call.
func (db *DB) ApplyChangeSets(changeSets []*NamedChangeSet) error {
	if len(changeSets) == 0 {
		return nil
	}

	db.mtx.Lock()
	defer db.mtx.Unlock()

	if db.readOnly {
		return errReadOnly
	}

	if len(db.pendingLog.Changesets) == 0 {
		db.pendingLog.Changesets = changeSets
		return db.MultiTree.ApplyChangeSets(changeSets)
	}

	// slow path, merge into exist changesets one store at a time,
	// should not happen in normal state machine life-cycle.
	for _, cs := range changeSets {
		if err := db.applyChangeSet(cs.Name, cs.Changeset); err != nil {
			return err
		}
	}
	return nil
}

// ApplyChangeSet wraps MultiTree.ApplyChangeSet, it also append the changesets in the pending log,
// which will be persisted to the WAL in next Commit call.
func (db *DB) ApplyChangeSet(name string, changeSet ChangeSet) error {
	if len(changeSet.Pairs) == 0 {
		return nil
	}

	db.mtx.Lock()
	defer db.mtx.Unlock()

	return db.applyChangeSet(name, changeSet)
}

func (db *DB) applyChangeSet(name string, changeSet ChangeSet) error {
	if len(changeSet.Pairs) == 0 {
		return nil
	}

	if db.readOnly {
		return errReadOnly
	}

	var updated bool
	for _, cs := range db.pendingLog.Changesets {
		if cs.Name == name {
			cs.Changeset.Pairs = append(cs.Changeset.Pairs, changeSet.Pairs...)
			updated = true
			break
		}
	}

	if !updated {
		db.pendingLog.Changesets = append(db.pendingLog.Changesets, &NamedChangeSet{
			Name:      name,
			Changeset: changeSet,
		})
		sort.SliceStable(db.pendingLog.Changesets, func(i, j int) bool {
			return db.pendingLog.Changesets[i].Name < db.pendingLog.Changesets[j].Name
		})
	}

	return db.MultiTree.ApplyChangeSet(name, changeSet)
}

// checkAsyncTasks checks the status of background tasks non-blocking-ly and process the result
func (db *DB) checkAsyncTasks() error {
	return errors.Join(
		db.checkAsyncCommit(),
		db.checkBackgroundSnapshotRewrite(),
	)
}

// checkAsyncCommit check the quit signal of async wal writing
func (db *DB) checkAsyncCommit() error {
	select {
	case err := <-db.walQuit:
		// async wal writing failed, we need to abort the state machine
		return fmt.Errorf("async wal writing goroutine quit unexpectedly: %w", err)
	default:
	}

	return nil
}

// CommittedVersion returns the latest version written in wal, or snapshot version if wal is empty.
func (db *DB) CommittedVersion() (int64, error) {
	lastIndex, err := db.wal.LastIndex()
	if err != nil {
		return 0, err
	}
	if lastIndex == 0 {
		return db.SnapshotVersion(), nil
	}
	return walVersion(lastIndex, db.initialVersion), nil
}

// checkBackgroundSnapshotRewrite check the result of background snapshot rewrite, cleans up the old snapshots and switches to a new multitree
func (db *DB) checkBackgroundSnapshotRewrite() error {
	// check the completeness of background snapshot rewriting
	select {
	case result := <-db.snapshotRewriteChan:
		db.snapshotRewriteChan = nil
		db.snapshotRewriteCancel = nil

		if result.mtree == nil {
			if result.err != nil {
				// background snapshot rewrite failed
				return fmt.Errorf("background snapshot rewriting failed: %w", result.err)
			}

			// background snapshot rewrite don't success, but no error to propagate, ignore it.
			return nil
		}

		// wait for potential pending wal writings to finish, to make sure we catch up to latest state.
		// in real world, block execution should be slower than wal writing, so this should not block for long.
		for {
			committedVersion, err := db.CommittedVersion()
			if err != nil {
				return fmt.Errorf("get wal version failed: %w", err)
			}
			if db.lastCommitInfo.Version == committedVersion {
				break
			}
			time.Sleep(time.Nanosecond)
		}

		// catchup the remaining wal
		if err := result.mtree.CatchupWAL(db.wal, 0); err != nil {
			return fmt.Errorf("catchup failed: %w", err)
		}

		// do the switch
		if err := db.reloadMultiTree(result.mtree); err != nil {
			return fmt.Errorf("switch multitree failed: %w", err)
		}
		db.logger.Info("switched to new snapshot", "version", db.MultiTree.Version())

		db.pruneSnapshots()

		// trigger state-sync snapshot export
		if db.triggerStateSyncExport != nil {
			db.triggerStateSyncExport(db.SnapshotVersion())
		}
	default:
	}

	return nil
}

// pruneSnapshot prune the old snapshots
func (db *DB) pruneSnapshots() {
	// wait until last prune finish
	db.pruneSnapshotLock.Lock()

	go func() {
		defer db.pruneSnapshotLock.Unlock()

		currentVersion, err := currentVersion(db.dir)
		if err != nil {
			db.logger.Error("failed to read current snapshot version", "err", err)
			return
		}

		counter := db.snapshotKeepRecent
		if err := traverseSnapshots(db.dir, false, func(version int64) (bool, error) {
			if version >= currentVersion {
				// ignore any newer snapshot directories, there could be ongoning snapshot rewrite.
				return false, nil
			}

			if counter > 0 {
				counter--
				return false, nil
			}

			name := snapshotName(version)
			db.logger.Info("prune snapshot", "name", name)

			if err := atomicRemoveDir(filepath.Join(db.dir, name)); err != nil {
				db.logger.Error("failed to prune snapshot", "err", err)
			}

			return false, nil
		}); err != nil {
			db.logger.Error("fail to prune snapshots", "err", err)
			return
		}

		// truncate WAL until the earliest remaining snapshot
		earliestVersion, err := firstSnapshotVersion(db.dir)
		if err != nil {
			db.logger.Error("failed to find first snapshot", "err", err)
		}

		if err := db.wal.TruncateFront(walIndex(earliestVersion+1, db.initialVersion)); err != nil {
			db.logger.Error("failed to truncate wal", "err", err, "version", earliestVersion+1)
		}
	}()
}

// Commit wraps SaveVersion to bump the version and writes the pending changes into log files to persist on disk
func (db *DB) Commit() (int64, error) {
	db.mtx.Lock()
	defer db.mtx.Unlock()

	if db.readOnly {
		return 0, errReadOnly
	}

	v, err := db.MultiTree.SaveVersion(true)
	if err != nil {
		return 0, err
	}

	// write logs if enabled
	if db.wal != nil {
		entry := walEntry{index: walIndex(v, db.initialVersion), data: db.pendingLog}
		if db.walChanSize >= 0 {
			if db.walChan == nil {
				db.initAsyncCommit()
			}

			// async wal writing
			db.walChan <- &entry
		} else {
			lastIndex, err := db.wal.LastIndex()
			if err != nil {
				return 0, err
			}

			db.wbatch.Clear()
			if err := writeEntry(&db.wbatch, db.logger, lastIndex, &entry); err != nil {
				return 0, err
			}

			if err := db.wal.WriteBatch(&db.wbatch); err != nil {
				return 0, err
			}
		}
	}

	db.pendingLog = WALEntry{}

	if err := db.checkAsyncTasks(); err != nil {
		return 0, err
	}
	db.rewriteIfApplicable(v)

	return v, nil
}

func (db *DB) initAsyncCommit() {
	walChan := make(chan *walEntry, db.walChanSize)
	walQuit := make(chan error)

	go func() {
		defer close(walQuit)

		batch := wal.Batch{}
		for {
			entries := channelBatchRecv(walChan)
			if len(entries) == 0 {
				// channel is closed
				break
			}

			lastIndex, err := db.wal.LastIndex()
			if err != nil {
				walQuit <- err
				return
			}

			for _, entry := range entries {
				if err := writeEntry(&batch, db.logger, lastIndex, entry); err != nil {
					walQuit <- err
					return
				}
			}

			if err := db.wal.WriteBatch(&batch); err != nil {
				walQuit <- err
				return
			}
			batch.Clear()
		}
	}()

	db.walChan = walChan
	db.walQuit = walQuit
}

// WaitAsyncCommit waits for the completion of async commit
func (db *DB) WaitAsyncCommit() error {
	db.mtx.Lock()
	defer db.mtx.Unlock()

	return db.waitAsyncCommit()
}

func (db *DB) waitAsyncCommit() error {
	if db.walChan == nil {
		return nil
	}

	close(db.walChan)
	err := <-db.walQuit

	db.walChan = nil
	db.walQuit = nil
	return err
}

func (db *DB) Copy() *DB {
	db.mtx.Lock()
	defer db.mtx.Unlock()

	return db.copy(db.cacheSize)
}

func (db *DB) copy(cacheSize int) *DB {
	mtree := db.MultiTree.Copy(cacheSize)

	return &DB{
		MultiTree:          *mtree,
		logger:             db.logger,
		dir:                db.dir,
		snapshotWriterPool: db.snapshotWriterPool,
	}
}

// RewriteSnapshot writes the current version of memiavl into a snapshot, and update the `current` symlink.
func (db *DB) RewriteSnapshot() error {
	return db.RewriteSnapshotWithContext(context.Background())
}

// RewriteSnapshotWithContext writes the current version of memiavl into a snapshot, and update the `current` symlink.
func (db *DB) RewriteSnapshotWithContext(ctx context.Context) error {
	db.mtx.Lock()
	defer db.mtx.Unlock()

	if db.readOnly {
		return errReadOnly
	}

	snapshotDir := snapshotName(db.lastCommitInfo.Version)
	tmpDir := snapshotDir + TmpSuffix
	path := filepath.Join(db.dir, tmpDir)
	if err := db.MultiTree.WriteSnapshotWithContext(ctx, path, db.snapshotWriterPool); err != nil {
		return errors.Join(err, os.RemoveAll(path))
	}
	if err := os.Rename(path, filepath.Join(db.dir, snapshotDir)); err != nil {
		return err
	}
	return updateCurrentSymlink(db.dir, snapshotDir)
}

func (db *DB) Reload() error {
	db.mtx.Lock()
	defer db.mtx.Unlock()

	return db.reload()
}

func (db *DB) reload() error {
	mtree, err := LoadMultiTree(currentPath(db.dir), db.zeroCopy, db.cacheSize)
	if err != nil {
		return err
	}
	return db.reloadMultiTree(mtree)
}

func (db *DB) reloadMultiTree(mtree *MultiTree) error {
	if err := db.MultiTree.Close(); err != nil {
		return err
	}

	db.MultiTree = *mtree
	// catch-up the pending changes
	return db.applyWALEntry(db.pendingLog)
}

// rewriteIfApplicable execute the snapshot rewrite strategy according to current height
func (db *DB) rewriteIfApplicable(height int64) {
	if height%int64(db.snapshotInterval) != 0 {
		return
	}

	if err := db.rewriteSnapshotBackground(); err != nil {
		db.logger.Error("failed to rewrite snapshot in background", "err", err)
	}
}

type snapshotResult struct {
	mtree *MultiTree
	err   error
}

// RewriteSnapshotBackground rewrite snapshot in a background goroutine,
// `Commit` will check the complete status, and switch to the new snapshot.
func (db *DB) RewriteSnapshotBackground() error {
	db.mtx.Lock()
	defer db.mtx.Unlock()

	if db.readOnly {
		return errReadOnly
	}

	return db.rewriteSnapshotBackground()
}

func (db *DB) rewriteSnapshotBackground() error {
	if db.snapshotRewriteChan != nil {
		return errors.New("there's another ongoing snapshot rewriting process")
	}

	ctx, cancel := context.WithCancel(context.Background())

	ch := make(chan snapshotResult)
	db.snapshotRewriteChan = ch
	db.snapshotRewriteCancel = cancel

	cloned := db.copy(0)
	wal := db.wal
	go func() {
		defer close(ch)

		cloned.logger.Info("start rewriting snapshot", "version", cloned.Version())
		if err := cloned.RewriteSnapshotWithContext(ctx); err != nil {
			// write error log but don't stop the client, it could happen when load an old version.
			cloned.logger.Error("failed to rewrite snapshot", "err", err)
			return
		}
		cloned.logger.Info("finished rewriting snapshot", "version", cloned.Version())
		mtree, err := LoadMultiTree(currentPath(cloned.dir), cloned.zeroCopy, 0)
		if err != nil {
			ch <- snapshotResult{err: err}
			return
		}

		// do a best effort catch-up, will do another final catch-up in main thread.
		if err := mtree.CatchupWAL(wal, 0); err != nil {
			ch <- snapshotResult{err: err}
			return
		}

		cloned.logger.Info("finished best-effort WAL catchup", "version", cloned.Version(), "latest", mtree.Version())

		ch <- snapshotResult{mtree: mtree}
	}()

	return nil
}

func (db *DB) Close() error {
	db.mtx.Lock()
	defer db.mtx.Unlock()

	errs := []error{db.waitAsyncCommit()}

	if db.snapshotRewriteChan != nil {
		db.snapshotRewriteCancel()
		<-db.snapshotRewriteChan
		db.snapshotRewriteChan = nil
		db.snapshotRewriteCancel = nil
	}

	errs = append(errs,
		db.MultiTree.Close(),
		db.wal.Close(),
	)

	db.wal = nil

	if db.fileLock != nil {
		errs = append(errs, db.fileLock.Unlock(), db.fileLock.Destroy())
		db.fileLock = nil
	}

	return errors.Join(errs...)
}

// TreeByName wraps MultiTree.TreeByName to add a lock.
func (db *DB) TreeByName(name string) *Tree {
	db.mtx.Lock()
	defer db.mtx.Unlock()

	return db.MultiTree.TreeByName(name)
}

// Version wraps MultiTree.Version to add a lock.
func (db *DB) Version() int64 {
	db.mtx.Lock()
	defer db.mtx.Unlock()

	return db.MultiTree.Version()
}

// LastCommitInfo returns the last commit info.
func (db *DB) LastCommitInfo() *CommitInfo {
	db.mtx.Lock()
	defer db.mtx.Unlock()

	return db.MultiTree.LastCommitInfo()
}

func (db *DB) SaveVersion(updateCommitInfo bool) (int64, error) {
	db.mtx.Lock()
	defer db.mtx.Unlock()

	if db.readOnly {
		return 0, errReadOnly
	}

	return db.MultiTree.SaveVersion(updateCommitInfo)
}

func (db *DB) WorkingCommitInfo() *CommitInfo {
	db.mtx.Lock()
	defer db.mtx.Unlock()

	return db.MultiTree.WorkingCommitInfo()
}

// UpdateCommitInfo wraps MultiTree.UpdateCommitInfo to add a lock.
func (db *DB) UpdateCommitInfo() {
	db.mtx.Lock()
	defer db.mtx.Unlock()

	if db.readOnly {
		panic("can't update commit info in read-only mode")
	}

	db.MultiTree.UpdateCommitInfo()
}

// WriteSnapshot wraps MultiTree.WriteSnapshot to add a lock.
func (db *DB) WriteSnapshot(dir string) error {
	return db.WriteSnapshotWithContext(context.Background(), dir)
}

// WriteSnapshotWithContext wraps MultiTree.WriteSnapshotWithContext to add a lock.
func (db *DB) WriteSnapshotWithContext(ctx context.Context, dir string) error {
	db.mtx.Lock()
	defer db.mtx.Unlock()

	return db.MultiTree.WriteSnapshotWithContext(ctx, dir, db.snapshotWriterPool)
}

func snapshotName(version int64) string {
	return fmt.Sprintf("%s%020d", SnapshotPrefix, version)
}

func currentPath(root string) string {
	return filepath.Join(root, "current")
}

func currentTmpPath(root string) string {
	return filepath.Join(root, "current-tmp")
}

func currentVersion(root string) (int64, error) {
	name, err := os.Readlink(currentPath(root))
	if err != nil {
		return 0, err
	}

	version, err := parseVersion(name)
	if err != nil {
		return 0, err
	}

	return version, nil
}

func parseVersion(name string) (int64, error) {
	if !isSnapshotName(name) {
		return 0, fmt.Errorf("invalid snapshot name %s", name)
	}

	v, err := strconv.ParseInt(name[len(SnapshotPrefix):], 10, 32)
	if err != nil {
		return 0, fmt.Errorf("snapshot version overflows: %w", err)
	}

	return v, nil
}

// seekSnapshot find the biggest snapshot version that's smaller than or equal to the target version,
// returns 0 if not found.
func seekSnapshot(root string, targetVersion uint32) (int64, error) {
	var (
		snapshotVersion int64
		found           bool
	)
	if err := traverseSnapshots(root, false, func(version int64) (bool, error) {
		if version <= int64(targetVersion) {
			found = true
			snapshotVersion = version
			return true, nil
		}
		return false, nil
	}); err != nil {
		return 0, err
	}

	if !found {
		return 0, fmt.Errorf("target version is pruned: %d", targetVersion)
	}

	return snapshotVersion, nil
}

// firstSnapshotVersion returns the earliest snapshot name in the db
func firstSnapshotVersion(root string) (int64, error) {
	var found int64
	if err := traverseSnapshots(root, true, func(version int64) (bool, error) {
		found = version
		return true, nil
	}); err != nil {
		return 0, err
	}

	if found == 0 {
		return 0, errors.New("empty memiavl db")
	}

	return found, nil
}

func walPath(root string) string {
	return filepath.Join(root, "wal")
}

// init a empty memiavl db
//
// ```
// snapshot-0
//
//	commit_info
//
// current -> snapshot-0
// ```
func initEmptyDB(dir string, initialVersion uint32) error {
	tmp := NewEmptyMultiTree(initialVersion, 0)
	snapshotDir := snapshotName(0)
	// create tmp worker pool
	pool := pond.New(DefaultSnapshotWriterLimit, DefaultSnapshotWriterLimit*10)
	defer pool.Stop()

	if err := tmp.WriteSnapshot(filepath.Join(dir, snapshotDir), pool); err != nil {
		return err
	}
	return updateCurrentSymlink(dir, snapshotDir)
}

// updateCurrentSymlink creates or replace the current symblic link atomically.
// it could fail under concurrent usage for tmp file conflicts.
func updateCurrentSymlink(dir, snapshot string) error {
	tmpPath := currentTmpPath(dir)
	if err := os.Symlink(snapshot, tmpPath); err != nil {
		return err
	}
	// assuming file renaming operation is atomic
	return os.Rename(tmpPath, currentPath(dir))
}

// traverseSnapshots traverse the snapshot list in specified order.
func traverseSnapshots(dir string, ascending bool, callback func(int64) (bool, error)) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}

	process := func(entry os.DirEntry) (bool, error) {
		if !entry.IsDir() || !isSnapshotName(entry.Name()) {
			return false, nil
		}

		version, err := parseVersion(entry.Name())
		if err != nil {
			return true, fmt.Errorf("invalid snapshot name: %w", err)
		}

		return callback(version)
	}

	if ascending {
		for i := 0; i < len(entries); i++ {
			stop, err := process(entries[i])
			if stop || err != nil {
				return err
			}
		}
	} else {
		for i := len(entries) - 1; i >= 0; i-- {
			stop, err := process(entries[i])
			if stop || err != nil {
				return err
			}
		}
	}

	return nil
}

// atomicRemoveDir is equavalent to `mv snapshot snapshot-tmp && rm -r snapshot-tmp`
func atomicRemoveDir(path string) error {
	tmpPath := path + TmpSuffix
	if err := os.Rename(path, tmpPath); err != nil {
		return err
	}

	return os.RemoveAll(tmpPath)
}

// createDBIfNotExist detects if db does not exist and try to initialize an empty one.
func createDBIfNotExist(dir string, initialVersion uint32) error {
	_, err := os.Stat(filepath.Join(dir, "current", MetadataFileName))
	if err != nil && os.IsNotExist(err) {
		return initEmptyDB(dir, initialVersion)
	}
	return nil
}

type walEntry struct {
	index uint64
	data  WALEntry
}

func isSnapshotName(name string) bool {
	return strings.HasPrefix(name, SnapshotPrefix) && len(name) == SnapshotDirLen
}

// GetLatestVersion finds the latest version number without loading the whole db,
// it's needed for upgrade module to check store upgrades,
// it returns 0 if db don't exists or is empty.
func GetLatestVersion(dir string) (int64, error) {
	metadata, err := readMetadata(currentPath(dir))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	wal, err := OpenWAL(walPath(dir), &wal.Options{NoCopy: true})
	if err != nil {
		return 0, err
	}
	lastIndex, err := wal.LastIndex()
	if err != nil {
		return 0, err
	}
	return walVersion(lastIndex, uint32(metadata.InitialVersion)), nil
}

func channelBatchRecv[T any](ch <-chan *T) []*T {
	// block if channel is empty
	item := <-ch
	if item == nil {
		// channel is closed
		return nil
	}

	remaining := len(ch)
	result := make([]*T, 0, remaining+1)
	result = append(result, item)
	for i := 0; i < remaining; i++ {
		result = append(result, <-ch)
	}

	return result
}

func writeEntry(batch *wal.Batch, logger Logger, lastIndex uint64, entry *walEntry) error {
	bz, err := entry.data.Marshal()
	if err != nil {
		return err
	}

	if entry.index <= lastIndex {
		logger.Error("commit old version idempotently", "lastIndex", lastIndex, "version", entry.index)
	} else {
		batch.Write(entry.index, bz)
	}
	return nil
}
