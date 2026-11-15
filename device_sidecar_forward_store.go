//go:build !js

// The VPN component's forwarding store (DESIGN-DETAILS 4.2.2's store; §18.6's posture row; owner decision 197;
// SDK-M8-16): one directory per network under the component's own container, never an app group, every record sealed
// under the network's forwarding root key by device.NewSealedPostureStore, so a copy of the files opens to nothing.
//
//	<ForwardStateDir>/fwd/networks                 the networks with a forwarding role here, in the order they joined,
//	                                               which the router takes a colliding block by (§17.4); ids only
//	<ForwardStateDir>/fwd/<network_id hex>/        the network's records, each sealed:
//	  posture/<name>                               device.PostureStore's records: manifest, base entry, rows, state
//	  genesis, compiled, log/<n>, rows/<sha256>,   the Forwarder's records (sidecar.ForwarderStore)
//	  head/<version>-<sha256>, gen/<g>
//	  setup                                        the ForwardingHandOff without its token
//
// A record is replaced whole: temp, fsync, rename and the directory fsynced, so a kill leaves the old record or the new
// one. Rows stream through the seal in 64 KiB chunks both ways; the Forwarder's writes go through Create, which the
// adapter holds whole until Commit, as the Forwarder writes a record it holds whole already.
//
// Absent means only that nothing was ever written under the name: a directory that exists and cannot be read fails
// every call, which a posture restores as pending, never legacy (DP1). Logs name no path, since a path names the
// network (I-13). Untagged: the tunnel links it. Safe for concurrent use.

package sdk

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	sidecardevice "github.com/urnetwork/sidecar/device"
)

// The store's layout's names (the file's comment). Format values.
const (
	deviceForwardDirectoryName    = "fwd"
	deviceForwardNetworksFileName = "networks"
	deviceForwardPosturePrefix    = "posture/"
	deviceForwardSetupName        = "setup"
	deviceForwardTempPrefix       = ".tmp-"
)

// What one segment of a record's name may be: lowercase letters, digits and dashes, starting with a letter or digit.
var deviceForwardSegmentPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,127}$`)

// One network's records on disk: a device.PostureStore whose names are one segment or two, the first a directory.
type sdkForwardFileStore struct {
	directoryPath string

	stateLock sync.Mutex
	// directories made durable, by path
	madeDirectories map[string]bool
}

var _ sidecardevice.PostureStore = (*sdkForwardFileStore)(nil)

// The records under directoryPath, absolute. Makes nothing until the first write.
func newSdkForwardFileStore(directoryPath string) (*sdkForwardFileStore, error) {
	if directoryPath == "" || !filepath.IsAbs(directoryPath) {
		return nil, errors.New("the forwarding store's directory is not absolute")
	}
	return &sdkForwardFileStore{directoryPath: directoryPath, madeDirectories: map[string]bool{}}, nil
}

// The file of name, or an error for a name outside the store.
func (self *sdkForwardFileStore) path(name string) (string, error) {
	segments := strings.Split(name, "/")
	if 2 < len(segments) {
		return "", fmt.Errorf("%w: a record name outside the store", sidecardevice.ErrPostureStoreFailed)
	}
	for _, segment := range segments {
		if !deviceForwardSegmentPattern.MatchString(segment) {
			return "", fmt.Errorf("%w: a record name outside the store", sidecardevice.ErrPostureStoreFailed)
		}
	}
	return filepath.Join(append([]string{self.directoryPath}, segments...)...), nil
}

// Streams the record; device.ErrPostureStoreAbsent only for one that is not there.
func (self *sdkForwardFileStore) Open(name string) (io.ReadCloser, error) {
	path, err := self.path(name)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, sidecardevice.ErrPostureStoreAbsent
	}
	if err != nil {
		return nil, sdkPostureStoreFault(err)
	}
	return file, nil
}

// Replaces the record whole and durably with what write writes.
func (self *sdkForwardFileStore) Write(name string, write func(writer io.Writer) error) error {
	path, err := self.path(name)
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := self.makeDirectories(directory); err != nil {
		return err
	}
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return err
	}
	tempPath := filepath.Join(directory, deviceForwardTempPrefix+hex.EncodeToString(suffix))
	file, err := os.OpenFile(tempPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return sdkPostureStoreFault(err)
	}
	keep := false
	defer func() {
		if !keep {
			file.Close()
			os.Remove(tempPath)
		}
	}()
	buffered := bufio.NewWriter(file)
	if err := write(buffered); err != nil {
		return err
	}
	if err := buffered.Flush(); err != nil {
		return sdkPostureStoreFault(err)
	}
	if err := file.Sync(); err != nil {
		return sdkPostureStoreFault(err)
	}
	if err := file.Close(); err != nil {
		return sdkPostureStoreFault(err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return sdkPostureStoreFault(err)
	}
	keep = true
	return syncSdkPostureDirectory(directory)
}

// Takes the record away; absent is not an error.
func (self *sdkForwardFileStore) Remove(name string) error {
	path, err := self.path(name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return sdkPostureStoreFault(err)
	}
	return nil
}

// The names held, sorted; a write in progress is never listed.
func (self *sdkForwardFileStore) List() ([]string, error) {
	names := []string{}
	entries, err := os.ReadDir(self.directoryPath)
	if errors.Is(err, fs.ErrNotExist) {
		return names, nil
	}
	if err != nil {
		return nil, sdkPostureStoreFault(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), deviceForwardTempPrefix) || !deviceForwardSegmentPattern.MatchString(entry.Name()) {
			continue
		}
		if !entry.IsDir() {
			names = append(names, entry.Name())
			continue
		}
		children, err := os.ReadDir(filepath.Join(self.directoryPath, entry.Name()))
		if err != nil {
			return nil, sdkPostureStoreFault(err)
		}
		for _, child := range children {
			if !child.IsDir() && !strings.HasPrefix(child.Name(), deviceForwardTempPrefix) && deviceForwardSegmentPattern.MatchString(child.Name()) {
				names = append(names, entry.Name()+"/"+child.Name())
			}
		}
	}
	slices.Sort(names)
	return names, nil
}

// What the record takes on disk, 0 for none.
func (self *sdkForwardFileStore) byteCount(name string) int64 {
	path, err := self.path(name)
	if err != nil {
		return 0
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// Makes directory and its parents up to the network's, 0700, each new one durable in its parent.
func (self *sdkForwardFileStore) makeDirectories(directory string) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.madeDirectories[directory] {
		return nil
	}
	missing := []string{}
	for path := directory; ; path = filepath.Dir(path) {
		if _, err := os.Stat(path); err == nil {
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return sdkPostureStoreFault(err)
		}
		missing = append(missing, path)
		if path == filepath.Dir(path) {
			break
		}
	}
	for i := len(missing) - 1; 0 <= i; i -= 1 {
		if err := os.Mkdir(missing[i], LocalStorageDirectoryPermissions); err != nil && !errors.Is(err, fs.ErrExist) {
			return sdkPostureStoreFault(err)
		}
		if err := syncSdkPostureDirectory(filepath.Dir(missing[i])); err != nil {
			return err
		}
	}
	self.madeDirectories[directory] = true
	return nil
}

// Removes every record of the network, its directory included: the sidecar's own sign-out (4.2.6's wiped).
func (self *sdkForwardFileStore) removeAll() error {
	if err := os.RemoveAll(self.directoryPath); err != nil {
		return sdkPostureStoreFault(err)
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	clear(self.madeDirectories)
	return syncSdkPostureDirectory(filepath.Dir(self.directoryPath))
}

// A view of a store under a name prefix: the posture's records as posture/<name>, which device.Posture names alone.
type deviceForwardPrefixStore struct {
	store  sidecardevice.PostureStore
	prefix string
}

var _ sidecardevice.PostureStore = (*deviceForwardPrefixStore)(nil)

func (self *deviceForwardPrefixStore) Open(name string) (io.ReadCloser, error) {
	return self.store.Open(self.prefix + name)
}

func (self *deviceForwardPrefixStore) Write(name string, write func(writer io.Writer) error) error {
	return self.store.Write(self.prefix+name, write)
}

func (self *deviceForwardPrefixStore) Remove(name string) error {
	return self.store.Remove(self.prefix + name)
}

func (self *deviceForwardPrefixStore) List() ([]string, error) {
	names, err := self.store.List()
	if err != nil {
		return nil, err
	}
	viewed := []string{}
	for _, name := range names {
		if rest, ok := strings.CutPrefix(name, self.prefix); ok {
			viewed = append(viewed, rest)
		}
	}
	return viewed, nil
}

// The Forwarder's records over the sealed store (sidecar.ForwarderStore): its absent is the root's, and a record is
// held until Commit, then written whole.
type deviceForwarderStore struct {
	sealed sidecardevice.PostureStore
	files  *sdkForwardFileStore
}

var _ sidecar.ForwarderStore = (*deviceForwarderStore)(nil)

// Streams the record; sidecar.ErrSidecarStoreAbsent only for one never written.
func (self *deviceForwarderStore) Open(name string) (io.ReadCloser, error) {
	reader, err := self.sealed.Open(name)
	if errors.Is(err, sidecardevice.ErrPostureStoreAbsent) {
		return nil, fmt.Errorf("%w: %w", sidecar.ErrSidecarStoreAbsent, err)
	}
	return reader, err
}

// A writer that replaces name at Commit.
func (self *deviceForwarderStore) Create(name string) (sidecar.SidecarStoreWriter, error) {
	if _, err := self.files.path(name); err != nil {
		return nil, err
	}
	return &deviceForwarderStoreWriter{store: self, name: name}, nil
}

// Absent is not an error.
func (self *deviceForwarderStore) Remove(name string) error {
	return self.sealed.Remove(name)
}

// Each record whose name starts with prefix, sorted by name, with what it takes on disk.
func (self *deviceForwarderStore) List(prefix string) ([]sidecar.SidecarStoreEntry, error) {
	names, err := self.sealed.List()
	if err != nil {
		return nil, err
	}
	entries := []sidecar.SidecarStoreEntry{}
	for _, name := range names {
		if strings.HasPrefix(name, prefix) && !strings.HasPrefix(name, deviceForwardPosturePrefix) {
			entries = append(entries, sidecar.SidecarStoreEntry{Name: name, ByteCount: self.files.byteCount(name)})
		}
	}
	return entries, nil
}

// One record being written.
type deviceForwarderStoreWriter struct {
	store  *deviceForwarderStore
	name   string
	buffer bytes.Buffer
	done   bool
}

func (self *deviceForwarderStoreWriter) Write(p []byte) (int, error) {
	if self.done {
		return 0, errors.New("the record was committed or closed")
	}
	return self.buffer.Write(p)
}

// Seals and swaps the record in, durably.
func (self *deviceForwarderStoreWriter) Commit() error {
	if self.done {
		return errors.New("the record was committed or closed")
	}
	self.done = true
	data := self.buffer.Bytes()
	return self.store.sealed.Write(self.name, func(writer io.Writer) error {
		_, err := writer.Write(data)
		return err
	})
}

// Drops what was written unless Commit took it. Safe to call again.
func (self *deviceForwarderStoreWriter) Close() error {
	self.done = true
	self.buffer.Reset()
	return nil
}

// The networks file under the component's container: the ids with a forwarding role, in the order they joined.
type deviceForwardNetworks struct {
	directoryPath string

	stateLock sync.Mutex
}

// The networks listed, in join order; none for no file. A file that does not read is an error, so the device keeps
// every role it cannot list pending rather than forgetting it.
func (self *deviceForwardNetworks) list() ([]connect.Id, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.listWithLock()
}

func (self *deviceForwardNetworks) listWithLock() ([]connect.Id, error) {
	data, err := os.ReadFile(filepath.Join(self.directoryPath, deviceForwardNetworksFileName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, sdkPostureStoreFault(err)
	}
	networkIds := []connect.Id{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		networkId, err := connect.ParseId(line)
		if err != nil {
			return nil, fmt.Errorf("%w: the networks file does not read", sidecardevice.ErrPostureStoreFailed)
		}
		networkIds = append(networkIds, networkId)
	}
	return networkIds, nil
}

// Adds networkId at the end, once.
func (self *deviceForwardNetworks) add(networkId connect.Id) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	networkIds, err := self.listWithLock()
	if err != nil {
		return err
	}
	if slices.Contains(networkIds, networkId) {
		return nil
	}
	return self.writeWithLock(append(networkIds, networkId))
}

// Drops networkId.
func (self *deviceForwardNetworks) remove(networkId connect.Id) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	networkIds, err := self.listWithLock()
	if err != nil {
		return err
	}
	if !slices.Contains(networkIds, networkId) {
		return nil
	}
	return self.writeWithLock(slices.DeleteFunc(networkIds, func(listed connect.Id) bool { return listed == networkId }))
}

// Writes the file whole and durably.
func (self *deviceForwardNetworks) writeWithLock(networkIds []connect.Id) error {
	files, err := newSdkForwardFileStore(self.directoryPath)
	if err != nil {
		return err
	}
	return files.Write(deviceForwardNetworksFileName, func(writer io.Writer) error {
		for _, networkId := range networkIds {
			if _, err := io.WriteString(writer, networkId.String()+"\n"); err != nil {
				return err
			}
		}
		return nil
	})
}
