package sdk

// Tests of the files of other processes in a log upload (UploadLogsFile,
// DeviceLocal.UploadLogsWithFiles): on windows and linux the app (gui) and the
// service (urnetworkd) log into separate directories, and the service's upload
// is the only one a feedback keeps, so the app's files ride in its zip.

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// Opens each file for reading, closed when the test ends.
func openTestingUploadLogsFiles(t *testing.T, paths ...string) []*os.File {
	t.Helper()
	files := []*os.File{}
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			t.Fatalf("Open(%q): %v", path, err)
		}
		t.Cleanup(func() {
			file.Close()
		})
		files = append(files, file)
	}
	return files
}

// The descriptor of an open file, as a caller hands it over.
func testingFileDescriptor(file *os.File) int64 {
	return int64(file.Fd())
}

// The service (urnetworkd on linux) logs into its own directory (SetLogDir) and
// carries the upload; the app (the gui) logs into another one, which the
// service's upload used to leave behind: it zipped only its own process's log
// directory, so the app's logs never reached support. The service's device now
// takes the app's files as open descriptors and its one zip holds them under
// the app's folder, beside its own files under their bare names, as before.
// The descriptors are the caller's again once the call returns: still open.
func TestDeviceLocalUploadLogsWithFilesHoldTheAppsLogs(t *testing.T) {
	restoreTestingLogDir(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	device, err := testing_newViewControllerDevice(ctx)
	if err != nil {
		t.Fatalf("device: %v", err)
	}
	defer device.Close()

	serviceDir := t.TempDir()
	modTime := time.Now().Add(-time.Hour)
	serviceName := "urnetworkd.host.root.log.INFO.20260901-000000.909"
	writeTestingUploadLogFile(t, serviceDir, serviceName, "service line\n", modTime)
	if err := SetLogDir(serviceDir); err != nil {
		t.Fatalf("SetLogDir: %v", err)
	}

	appDir := t.TempDir()
	appInfoName := "URnetwork.host.user.log.INFO.20260901-000000.111"
	appErrorName := "URnetwork.host.user.log.ERROR.20260901-000000.111"
	appInfoPath := writeTestingUploadLogFile(t, appDir, appInfoName, "app line\n", modTime)
	appErrorPath := writeTestingUploadLogFile(t, appDir, appErrorName, "app error line\n", modTime)
	// what else is in the app's directory is not handed over, and never read
	writeTestingUploadLogFile(t, appDir, "app_prefs.json", "{\"not\":\"a log\"}\n", modTime)
	appFiles := openTestingUploadLogsFiles(t, appInfoPath, appErrorPath)

	uploadLogsFiles := NewUploadLogsFileList()
	uploadLogsFiles.Add(&UploadLogsFile{
		Source:         "app",
		Name:           appInfoName,
		FileDescriptor: testingFileDescriptor(appFiles[0]),
	})
	uploadLogsFiles.Add(&UploadLogsFile{
		Source:         "app",
		Name:           appErrorName,
		FileDescriptor: testingFileDescriptor(appFiles[1]),
	})

	postedZipChannel := make(chan []byte, 1)
	device.GetApi().setHttpPostStreamRaw(func(ctx context.Context, requestUrl string, body io.Reader, byJwt string) ([]byte, error) {
		zipBytes, err := io.ReadAll(body)
		if err != nil {
			return nil, err
		}
		postedZipChannel <- zipBytes
		return []byte("{}"), nil
	})

	if err := device.UploadLogsWithFiles(NewId().String(), uploadLogsFiles, nil); err != nil {
		t.Fatalf("UploadLogsWithFiles = %v", err)
	}
	var zipBytes []byte
	select {
	case zipBytes = <-postedZipChannel:
	case <-time.After(30 * time.Second):
		t.Fatal("the device did not post the upload")
	}
	entryNameContents := readTestingZip(t, zipBytes)

	wantEntryNameContents := map[string]string{
		serviceName:           "service line\n",
		"app/" + appInfoName:  "app line\n",
		"app/" + appErrorName: "app error line\n",
	}
	for entryName, wantContent := range wantEntryNameContents {
		if entryNameContents[entryName] != wantContent {
			t.Errorf("the upload has %q = %q, want %q (entries %v)", entryName, entryNameContents[entryName], wantContent, testingZipEntryNames(entryNameContents))
		}
	}
	for entryName := range entryNameContents {
		if strings.HasPrefix(entryName, "app/") {
			if _, ok := wantEntryNameContents[entryName]; !ok {
				t.Errorf("the upload has %q, which was not handed over", entryName)
			}
		} else if strings.Contains(entryName, "/") {
			t.Errorf("the upload has %q, outside the app's folder and this process's bare names", entryName)
		}
		if logSeverityOf(entryName) == "" {
			t.Errorf("the upload has %q, which is not a glog file", entryName)
		}
	}

	// borrowed: the caller's descriptors are still open, and still its own
	for i, appFile := range appFiles {
		if _, err := appFile.Stat(); err != nil {
			t.Errorf("the caller's descriptor %d was closed by the upload: %v", i, err)
		}
	}
}

// The upload reads each file from its start through its own duplicate, and
// leaves the caller's descriptor where it was: open, at the offset the caller
// had read it to.
func TestUploadLogsWithFilesBorrowTheDescriptors(t *testing.T) {
	restoreTestingLogDir(t)

	if err := SetLogDir(t.TempDir()); err != nil {
		t.Fatalf("SetLogDir: %v", err)
	}
	appName := "URnetwork.host.user.log.INFO.20260901-000000.222"
	appPath := writeTestingUploadLogFile(t, t.TempDir(), appName, "first|second\n", time.Now().Add(-time.Hour))
	appFile := openTestingUploadLogsFiles(t, appPath)[0]
	readBytes := make([]byte, len("first|"))
	if _, err := io.ReadFull(appFile, readBytes); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}

	zipPath, err := zipUploadLogs(uploadLogsMaxByteCount, []*UploadLogsFile{
		{
			Source:         "app",
			Name:           appName,
			FileDescriptor: testingFileDescriptor(appFile),
		},
	}, connect.DefaultLogger())
	if err != nil {
		t.Fatalf("zipUploadLogs = %v", err)
	}
	zipBytes, err := os.ReadFile(zipPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	entryNameContents := readTestingZip(t, zipBytes)
	if content := entryNameContents["app/"+appName]; content != "first|second\n" {
		t.Fatalf("the upload has %q for the app's file, want the whole file (entries %v)", content, testingZipEntryNames(entryNameContents))
	}

	offset, err := appFile.Seek(0, io.SeekCurrent)
	if err != nil {
		t.Fatalf("the caller's descriptor is not usable after the upload: %v", err)
	}
	if offset != int64(len("first|")) {
		t.Fatalf("the caller's descriptor is at %d after the upload, want %d where the caller left it", offset, len("first|"))
	}
	restBytes, err := io.ReadAll(appFile)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(restBytes) != "second\n" {
		t.Fatalf("the caller reads %q after the upload, want %q", restBytes, "second\n")
	}
}

// The name and the folder come from the process that wrote the file, and the
// descriptor from wherever the caller got it. The upload takes only a regular
// file with a glog name under a plain folder name, once per name, and leaves
// out everything else while the rest still goes: a path in either name, a name
// glog does not write, a folder of the wrong shape, a second file under one
// name, a directory, and a descriptor that is not one.
func TestUploadLogsWithFilesLeaveOutWhatIsNotAGlogFile(t *testing.T) {
	restoreTestingLogDir(t)

	if err := SetLogDir(t.TempDir()); err != nil {
		t.Fatalf("SetLogDir: %v", err)
	}
	appDir := t.TempDir()
	modTime := time.Now().Add(-time.Hour)
	appName := "URnetwork.host.user.log.INFO.20260901-000000.333"
	appPath := writeTestingUploadLogFile(t, appDir, appName, "app line\n", modTime)
	secretPath := writeTestingUploadLogFile(t, appDir, "jwt", "not a log\n", modTime)
	appFile := openTestingUploadLogsFiles(t, appPath)[0]
	secretFile := openTestingUploadLogsFiles(t, secretPath)[0]
	appDirFile := openTestingUploadLogsFiles(t, appDir)[0]

	uploadLogsFiles := []*UploadLogsFile{
		nil,
		{Source: "app", Name: appName, FileDescriptor: testingFileDescriptor(appFile)},
		// a second file under a name already taken
		{Source: "app", Name: appName, FileDescriptor: testingFileDescriptor(secretFile)},
		// names glog does not write, or with a path in them
		{Source: "app", Name: "jwt", FileDescriptor: testingFileDescriptor(secretFile)},
		{Source: "app", Name: "../jwt.log.INFO.1", FileDescriptor: testingFileDescriptor(secretFile)},
		{Source: "app", Name: "..\\jwt.log.INFO.2", FileDescriptor: testingFileDescriptor(secretFile)},
		{Source: "app", Name: ".jwt.log.INFO.3", FileDescriptor: testingFileDescriptor(secretFile)},
		{Source: "app", Name: "jwt\n.log.INFO.4", FileDescriptor: testingFileDescriptor(secretFile)},
		{Source: "app", Name: "C:jwt.log.INFO.5", FileDescriptor: testingFileDescriptor(secretFile)},
		// folders that are not one plain name
		{Source: "", Name: "a.log.INFO.6", FileDescriptor: testingFileDescriptor(secretFile)},
		{Source: "..", Name: "a.log.INFO.7", FileDescriptor: testingFileDescriptor(secretFile)},
		{Source: "app/..", Name: "a.log.INFO.8", FileDescriptor: testingFileDescriptor(secretFile)},
		{Source: "App", Name: "a.log.INFO.9", FileDescriptor: testingFileDescriptor(secretFile)},
		{Source: "1app", Name: "a.log.INFO.10", FileDescriptor: testingFileDescriptor(secretFile)},
		{Source: strings.Repeat("a", uploadLogsMaxSourceLength+1), Name: "a.log.INFO.11", FileDescriptor: testingFileDescriptor(secretFile)},
		// a glog name on what is not a regular file, or not a descriptor
		{Source: "dir", Name: "a.log.INFO.12", FileDescriptor: testingFileDescriptor(appDirFile)},
		{Source: "bad", Name: "a.log.INFO.13", FileDescriptor: -1},
		{Source: "bad", Name: "a.log.INFO.14", FileDescriptor: math.MaxInt64},
	}

	zipPath, err := zipUploadLogs(uploadLogsMaxByteCount, uploadLogsFiles, connect.DefaultLogger())
	if err != nil {
		t.Fatalf("zipUploadLogs = %v, want the rest of the upload", err)
	}
	zipBytes, err := os.ReadFile(zipPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	entryNameContents := readTestingZip(t, zipBytes)
	if content := entryNameContents["app/"+appName]; content != "app line\n" {
		t.Errorf("the upload has %q for the app's file, want %q (entries %v)", content, "app line\n", testingZipEntryNames(entryNameContents))
	}
	for entryName, content := range entryNameContents {
		if strings.Contains(entryName, "/") && entryName != "app/"+appName {
			t.Errorf("the upload has %q, which it should have left out", entryName)
		}
		if strings.Contains(content, "not a log") {
			t.Errorf("the upload has the bytes of a file that is not a log, as %q", entryName)
		}
	}
}

// The other processes' files count against the same cap as this process's
// own, in the same newest first order: what does not fit is left out, oldest
// first, whichever process wrote it, and the duplicates of the files left out
// are closed at once.
func TestUploadLogsWithFilesKeepTheNewestThatFitTheCap(t *testing.T) {
	restoreTestingLogDir(t)

	serviceDir := t.TempDir()
	// older than every app file
	writeTestingUploadLogFile(t, serviceDir, "urnetworkd.host.root.log.INFO.20260901-000000.444", strings.Repeat("s", 100), time.Now().Add(-2*time.Hour))
	if err := SetLogDir(serviceDir); err != nil {
		t.Fatalf("SetLogDir: %v", err)
	}

	// newer than anything this process writes during the test, so their order
	// is the order the cap is spent in
	appDir := t.TempDir()
	baseTime := time.Now().Add(time.Hour)
	oldestName := "URnetwork.host.user.log.INFO.20260901-000000.551"
	middleName := "URnetwork.host.user.log.INFO.20260901-000000.552"
	newestName := "URnetwork.host.user.log.INFO.20260901-000000.553"
	appFiles := openTestingUploadLogsFiles(t,
		writeTestingUploadLogFile(t, appDir, oldestName, strings.Repeat("o", 300), baseTime),
		writeTestingUploadLogFile(t, appDir, middleName, strings.Repeat("m", 200), baseTime.Add(time.Minute)),
		writeTestingUploadLogFile(t, appDir, newestName, strings.Repeat("n", 100), baseTime.Add(2*time.Minute)),
	)
	uploadLogsFiles := []*UploadLogsFile{}
	for i, name := range []string{oldestName, middleName, newestName} {
		uploadLogsFiles = append(uploadLogsFiles, &UploadLogsFile{
			Source:         "app",
			Name:           name,
			FileDescriptor: testingFileDescriptor(appFiles[i]),
		})
	}

	// the two newest app files, each with its entry, and nothing more
	maxByteCount := int64(200+100) + 2*uploadLogsEntryByteCount
	plan := newUploadLogsPlan(maxByteCount, uploadLogsFiles, connect.DefaultLogger())
	defer plan.close()

	wantSourceNames := []string{
		"app/" + newestName,
		"app/" + middleName,
	}
	if sourceNames := testingUploadLogSourceNames(plan.logFileInfos); !slices.Equal(sourceNames, wantSourceNames) {
		t.Fatalf("planned %v, want %v", sourceNames, wantSourceNames)
	}
	if len(plan.logFileInfoOpenFiles) != len(wantSourceNames) {
		t.Fatalf("the plan holds %d duplicates, want only the %d planned", len(plan.logFileInfoOpenFiles), len(wantSourceNames))
	}

	// planning again gives the same plan
	planAgain := newUploadLogsPlan(maxByteCount, uploadLogsFiles, connect.DefaultLogger())
	defer planAgain.close()
	if sourceNames := testingUploadLogSourceNames(planAgain.logFileInfos); !slices.Equal(sourceNames, wantSourceNames) {
		t.Fatalf("planned %v the second time, want %v", sourceNames, wantSourceNames)
	}
}

// Under a log root (ios, android) every process already has its folder, as its
// directory's name. A file handed over under one of those names would mix two
// processes in one folder, so it is left out, and a folder of its own is kept.
func TestUploadLogsWithFilesKeepTheFoldersOfTheLogRoot(t *testing.T) {
	restoreTestingLogDir(t)

	root := t.TempDir()
	modTime := time.Now().Add(-time.Hour)
	extensionName := "urnetwork.host.user.log.INFO.20260901-000000.661"
	writeTestingUploadLogFile(t, filepath.Join(root, "extension"), extensionName, "extension line\n", modTime)
	if err := SetLogDirForProcess(root, "app"); err != nil {
		t.Fatalf("SetLogDirForProcess: %v", err)
	}

	otherDir := t.TempDir()
	otherName := "helper.host.user.log.INFO.20260901-000000.662"
	otherFile := openTestingUploadLogsFiles(t, writeTestingUploadLogFile(t, otherDir, otherName, "helper line\n", modTime))[0]

	zipPath, err := zipUploadLogs(uploadLogsMaxByteCount, []*UploadLogsFile{
		{Source: "extension", Name: otherName, FileDescriptor: testingFileDescriptor(otherFile)},
		{Source: "app", Name: otherName, FileDescriptor: testingFileDescriptor(otherFile)},
		{Source: "helper", Name: otherName, FileDescriptor: testingFileDescriptor(otherFile)},
	}, connect.DefaultLogger())
	if err != nil {
		t.Fatalf("zipUploadLogs = %v", err)
	}
	zipBytes, err := os.ReadFile(zipPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	entryNameContents := readTestingZip(t, zipBytes)
	if content := entryNameContents["extension/"+extensionName]; content != "extension line\n" {
		t.Errorf("the upload lacks the extension's own file (entries %v)", testingZipEntryNames(entryNameContents))
	}
	if content := entryNameContents["helper/"+otherName]; content != "helper line\n" {
		t.Errorf("the upload lacks the file handed over under its own folder (entries %v)", testingZipEntryNames(entryNameContents))
	}
	for _, entryName := range []string{"extension/" + otherName, "app/" + otherName} {
		if _, ok := entryNameContents[entryName]; ok {
			t.Errorf("the upload has %q, a file handed over into a folder of the log root", entryName)
		}
	}
}

// The desktop apps hand the files over through the c abi as json
// (urnet_device_local_upload_logs_with_files): an array of objects keyed by
// the field names, as the generated c++ wrapper writes them.
func TestUploadLogsFileListCrossesAsJson(t *testing.T) {
	uploadLogsFiles := NewUploadLogsFileList()
	if err := json.Unmarshal([]byte(`[{"Source":"gui","Name":"urnetwork-gui.host.user.log.INFO.20260901-000000.1","FileDescriptor":7}]`), uploadLogsFiles); err != nil {
		t.Fatalf("Unmarshal = %v", err)
	}
	if uploadLogsFiles.Len() != 1 {
		t.Fatalf("read %d files, want 1", uploadLogsFiles.Len())
	}
	uploadLogsFile := uploadLogsFiles.Get(0)
	if uploadLogsFile.Source != "gui" || uploadLogsFile.Name != "urnetwork-gui.host.user.log.INFO.20260901-000000.1" || uploadLogsFile.FileDescriptor != 7 {
		t.Fatalf("read %+v", uploadLogsFile)
	}
	jsonBytes, err := json.Marshal(uploadLogsFiles)
	if err != nil {
		t.Fatalf("Marshal = %v", err)
	}
	if string(jsonBytes) != `[{"Source":"gui","Name":"urnetwork-gui.host.user.log.INFO.20260901-000000.1","FileDescriptor":7}]` {
		t.Fatalf("wrote %s", jsonBytes)
	}
}
