package sdk

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	sidecardevice "github.com/urnetwork/sidecar/device"
)

// A role's store over a test's key store and directory, open.
func testingForwardRole(t *testing.T, store *testingForwardKeyStore, directory string, networkId connect.Id) *deviceLocalForwarding {
	t.Helper()
	files, err := newSdkForwardFileStore(filepath.Join(directory, "network"))
	if err != nil {
		t.Fatal(err)
	}
	role := &deviceLocalForwarding{
		networkId: networkId,
		keys:      newDeviceForwardKeys(&deviceForwardSecrets{store: store}, networkId),
		files:     files,
		settings:  defaultDeviceForwardingSettings(),
	}
	if err := role.openStore(true); err != nil {
		t.Fatal(err)
	}
	return role
}

// Every record goes to disk sealed: a file opens with UFS1 and holds no byte of its plaintext, reads back whole, and a
// flipped byte fails the read, never as absent; the Forwarder's absent is the root's, and its listing leaves the
// posture's records out.
func TestForwardStoreSealsEveryRecord(t *testing.T) {
	store := newTestingForwardKeyStore()
	directory := t.TempDir()
	role := testingForwardRole(t, store, directory, connect.NewId())
	plaintext := bytes.Repeat([]byte("synthetic-forward-record "), 4096)
	posture := &deviceForwardPrefixStore{store: role.sealed, prefix: deviceForwardPosturePrefix}
	if err := posture.Write("manifest", func(writer io.Writer) error {
		_, err := writer.Write(plaintext)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	writer, err := role.forwarderStore.Create("log/00000000000000000001")
	if err != nil {
		t.Fatal(err)
	}
	writer.Write(plaintext[:100])
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"posture/manifest", "log/00000000000000000001"} {
		onDisk, err := os.ReadFile(filepath.Join(directory, "network", filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.HasPrefix(onDisk, []byte("UFS1")) || bytes.Contains(onDisk, []byte("synthetic-forward-record")) {
			t.Errorf("%s is not sealed on disk", path)
		}
	}
	reader, err := posture.Open("manifest")
	if err != nil {
		t.Fatal(err)
	}
	read, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || !bytes.Equal(read, plaintext) {
		t.Fatalf("the record read back %d bytes, %v", len(read), err)
	}
	if names, err := posture.List(); err != nil || !slices.Equal(names, []string{"manifest"}) {
		t.Errorf("the posture's view lists %v, %v", names, err)
	}
	entries, err := role.forwarderStore.List("")
	if err != nil || len(entries) != 1 || entries[0].Name != "log/00000000000000000001" || entries[0].ByteCount <= 100 {
		t.Errorf("the Forwarder's listing %+v, %v", entries, err)
	}
	if _, err := role.forwarderStore.Open("compiled"); !errors.Is(err, sidecar.ErrSidecarStoreAbsent) {
		t.Errorf("a record never written opened as %v", err)
	}

	path := filepath.Join(directory, "network", "posture", "manifest")
	onDisk, _ := os.ReadFile(path)
	onDisk[len(onDisk)/2] ^= 1
	if err := os.WriteFile(path, onDisk, 0600); err != nil {
		t.Fatal(err)
	}
	reader, err = posture.Open("manifest")
	if err == nil {
		_, err = io.ReadAll(reader)
		reader.Close()
	}
	if !errors.Is(err, sidecardevice.ErrPostureStoreFailed) || errors.Is(err, sidecardevice.ErrPostureStoreAbsent) {
		t.Errorf("a flipped byte read as %v", err)
	}

	// the same files under another root key open to nothing
	other := newTestingForwardKeyStore()
	reopened := testingForwardRole(t, other, directory, connect.NewId())
	if _, err := reopened.forwarderStore.Open("log/00000000000000000001"); err == nil {
		readerOther, _ := reopened.forwarderStore.Open("log/00000000000000000001")
		if readerOther != nil {
			if _, err := io.ReadAll(readerOther); err == nil {
				t.Error("another root key opened the record")
			}
		}
	}
}

// A name outside the store, a third segment or a dot, is refused before anything is written.
func TestForwardStoreRefusesNamesOutsideIt(t *testing.T) {
	files, err := newSdkForwardFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../escape", "a/b/c", ".hidden", "Upper", "log/../x", ""} {
		if err := files.Write(name, func(writer io.Writer) error { return nil }); !errors.Is(err, sidecardevice.ErrPostureStoreFailed) {
			t.Errorf("%q was written: %v", name, err)
		}
	}
	if _, err := newSdkForwardFileStore("relative"); err == nil {
		t.Error("a relative directory made a store")
	}
}

// The networks file keeps the order the networks joined, each once, and a removal drops one.
func TestForwardNetworksKeepJoinOrder(t *testing.T) {
	networks := &deviceForwardNetworks{directoryPath: t.TempDir()}
	first, second := connect.NewId(), connect.NewId()
	if listed, err := networks.list(); err != nil || len(listed) != 0 {
		t.Fatalf("no file listed %v, %v", listed, err)
	}
	for _, networkId := range []connect.Id{second, first, second} {
		if err := networks.add(networkId); err != nil {
			t.Fatal(err)
		}
	}
	if listed, _ := networks.list(); !slices.Equal(listed, []connect.Id{second, first}) {
		t.Errorf("listed %v", listed)
	}
	if err := networks.remove(second); err != nil {
		t.Fatal(err)
	}
	if listed, _ := networks.list(); !slices.Equal(listed, []connect.Id{first}) {
		t.Errorf("after the removal listed %v", listed)
	}
	os.WriteFile(filepath.Join(networks.directoryPath, deviceForwardNetworksFileName), []byte("not an id\n"), 0600)
	if _, err := networks.list(); err == nil {
		t.Error("a networks file that does not read listed")
	}
}
