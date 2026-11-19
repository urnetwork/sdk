package sdk

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// extender_seed_store.go — where a network space keeps its accepted seed
// bundles (connect EXTENDER-UPDATE2.md §2.2, §2.3).
//
// The seed store holds, for each network host the space accepts, the greatest
// accepted sequence, the digest of its body and the envelope itself. connect
// decides what is accepted and serializes it (ExtenderSeedStore); this file is
// only the platform storage under it: one dot file beside `.extenders`, read
// and replaced whole.
//
// The replacement is atomic. The bytes go to a temporary file in the same
// directory, are synced, and the temporary file is renamed over the store, so
// a process that ends at any point leaves either the previous document or the
// new one and never a torn one. That is what keeps the accepted artifact and
// its sequence together: a restart cannot find a higher sequence without the
// bundle that carried it (§2.2).
//
// The store is extender state, so a sign-out keeps it (keptAcrossSignOut) and
// "Reset extenders" clears it (E7). A reset returns the space to a fresh
// install's, which has no sequence history; the spec bounds a fresh install's
// replay exposure by the bundle validity window, and clearing is also how a
// floor raised under a root key the user later abandons stops binding.
//
// Unlike `.extenders`, the store is written by every process that holds the
// space, including the ios app process, which sets the extender store read
// only (K5): the directory is rewritten on every change by a coalescing loop
// in each process, while a seed is written only when a strictly newer bundle
// is accepted, and both processes need what an import brought -- the app for
// its api dials, the packet tunnel extension for the tunnel. Two processes
// accepting different bundles at the same instant can leave the earlier of
// the two on disk; that is a floor one step lower, still a bundle that
// verified, and the spec's fresh-install bound already covers it.

// The seed store file, a dot file beside the directory's `.extenders`.
const extenderSeedStoreFileName = ".extender_seeds"

// The prefix of the temporary files a replacement writes before it renames.
// A process that ends between the write and the rename leaves one behind;
// the next successful replacement removes it once it is old enough that no
// other process can still be writing it.
const extenderSeedStoreTempPrefix = extenderSeedStoreFileName + ".tmp-"

// The largest store document read or written. connect's document holds at most
// eight networks' envelopes of at most 64 KiB each, in base64 with the json
// around them, which is a little over 1 MiB; this leaves room above it, so a
// document connect wrote is never refused here, while a corrupt or hostile
// file larger than that is never read whole into the memory of a phone.
const extenderSeedStoreMaxByteCount = 2 * 1024 * 1024

// A temporary file older than this is no longer being written by anyone.
const extenderSeedStoreStaleTempTimeout = time.Hour

// Refusals of the store, for a caller that tells them apart. The detail of an
// i/o failure is wrapped; none of these carries the document.
var (
	errExtenderSeedStoreTooLarge   = errors.New("the extender seed store is over its size limit")
	errExtenderSeedStoreNotRegular = errors.New("the extender seed store is not a regular file")
	errExtenderSeedStoreClosed     = errors.New("the extender seed store has no storage")
)

// localStateExtenderSeedStore keeps the seed store document of one space in
// its local state directory, as connect's ExtenderSeedStore. The zero value
// has no storage: every load is empty and every save is refused, which is what
// a space without local state gets in place of a file.
//
// A load that fails is an error, which the acceptor treats as a refusal rather
// than as no history, so a transient read failure never lowers a floor. A
// document over the size limit is not an error but no document: it is not one
// connect wrote, the acceptor would read it as no history anyway, and the next
// accept replaces it.
//
// Safe for concurrent use. Writers in one process are serialized; readers
// read whatever the last rename installed. Two processes may share the file:
// a replacement removes only temporary files too old to be another live
// replacement's.
type localStateExtenderSeedStore struct {
	localStorageDir string

	// serializes the replacements of this process, so two of them never
	// interleave their renames and the temp cleanup never races a write
	writeLock sync.Mutex

	// Test seam: when set, called before each step of a replacement, and a
	// non-nil error fails the replacement at that step exactly as the
	// operation would. The steps are "create", "write", "sync", "close",
	// "rename" and "directory sync". Production never sets it.
	testingCommitStep func(step string) error
}

func newLocalStateExtenderSeedStore(localState *LocalState) *localStateExtenderSeedStore {
	if localState == nil {
		return &localStateExtenderSeedStore{}
	}
	return &localStateExtenderSeedStore{
		localStorageDir: localState.localStorageDir,
	}
}

func (self *localStateExtenderSeedStore) path() string {
	return filepath.Join(self.localStorageDir, extenderSeedStoreFileName)
}

// Load returns the stored document, or nil when there is none. A document
// over the size limit is no document. A store path that is not a regular file
// is an error: nothing this build writes is anything else.
func (self *localStateExtenderSeedStore) Load() ([]byte, error) {
	if self.localStorageDir == "" {
		return nil, nil
	}
	file, err := os.Open(self.path())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errExtenderSeedStoreNotRegular
	}
	if extenderSeedStoreMaxByteCount < info.Size() {
		return nil, nil
	}
	// read at most one byte past the limit, so a file that grew after the
	// stat is still not read whole
	data, err := io.ReadAll(io.LimitReader(file, extenderSeedStoreMaxByteCount+1))
	if err != nil {
		return nil, err
	}
	if extenderSeedStoreMaxByteCount < len(data) || len(data) == 0 {
		return nil, nil
	}
	return data, nil
}

// Save replaces the stored document atomically. Empty removes it.
func (self *localStateExtenderSeedStore) Save(data []byte) error {
	if self.localStorageDir == "" {
		return errExtenderSeedStoreClosed
	}
	if extenderSeedStoreMaxByteCount < len(data) {
		return errExtenderSeedStoreTooLarge
	}
	self.writeLock.Lock()
	defer self.writeLock.Unlock()
	if len(data) == 0 {
		return self.removeWithLock()
	}
	if err := self.replaceWithLock(data); err != nil {
		return err
	}
	self.removeStaleTempsWithLock()
	return nil
}

// Clear removes the stored document, which is what a reset leaves (E7).
func (self *localStateExtenderSeedStore) Clear() error {
	if self.localStorageDir == "" {
		return nil
	}
	self.writeLock.Lock()
	defer self.writeLock.Unlock()
	return self.removeWithLock()
}

func (self *localStateExtenderSeedStore) removeWithLock() error {
	if err := os.Remove(self.path()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	syncExtenderSeedStoreDirectory(self.localStorageDir)
	return nil
}

func (self *localStateExtenderSeedStore) commitStep(step string) error {
	if self.testingCommitStep == nil {
		return nil
	}
	return self.testingCommitStep(step)
}

// The durable replacement: create in the destination directory, restrict the
// permissions before anything is written, write, sync, close, rename over the
// store, then sync the directory where the platform supports it. The temporary
// file is removed on every failure. A store path that exists and is not a
// regular file is refused: the rename would replace whatever it is.
func (self *localStateExtenderSeedStore) replaceWithLock(data []byte) (returnErr error) {
	path := self.path()
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return errExtenderSeedStoreNotRegular
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := self.commitStep("create"); err != nil {
		return err
	}
	temp, err := os.CreateTemp(self.localStorageDir, extenderSeedStoreTempPrefix)
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	closed := false
	defer func() {
		if !closed {
			_ = temp.Close()
		}
		if returnErr != nil {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(LocalStorageFilePermissions); err != nil && runtime.GOOS != "windows" {
		return err
	}
	if err := self.commitStep("write"); err != nil {
		return err
	}
	if n, err := temp.Write(data); err != nil {
		return err
	} else if n != len(data) {
		return fmt.Errorf("the extender seed store wrote %d of %d bytes", n, len(data))
	}
	if err := self.commitStep("sync"); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := self.commitStep("close"); err != nil {
		return err
	}
	closed = true
	if err := temp.Close(); err != nil {
		return err
	}
	if err := self.commitStep("rename"); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	// The rename has committed: every reader from here on sees the new
	// document, so a failed directory sync is not reported as a failed save.
	if err := self.commitStep("directory sync"); err == nil {
		syncExtenderSeedStoreDirectory(self.localStorageDir)
	}
	return nil
}

// Removes the temporary files a process that ended mid-replacement left, once
// they are old enough that no live replacement can own them.
func (self *localStateExtenderSeedStore) removeStaleTempsWithLock() {
	matches, _ := filepath.Glob(filepath.Join(self.localStorageDir, extenderSeedStoreTempPrefix+"*"))
	staleBefore := time.Now().Add(-extenderSeedStoreStaleTempTimeout)
	for _, match := range matches {
		info, err := os.Lstat(match)
		if err == nil && info.Mode().IsRegular() && info.ModTime().Before(staleBefore) {
			_ = os.Remove(match)
		}
	}
}

// Windows' directory handles do not support File.Sync, and its rename is the
// atomic replacement primitive there already. Elsewhere the directory is synced
// so the rename itself survives a power loss; it is best effort, since the
// rename has already committed for every reader.
func syncExtenderSeedStoreDirectory(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	directory, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = directory.Sync()
	_ = directory.Close()
}
