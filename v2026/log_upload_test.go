package sdk

// Tests of the log upload (log_upload.go): which files it sends, under which
// names, within which cap, and that the device's, the remote's and the api's
// uploads each send the logs of every process under the shared log root.

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/glog/v2026"
)

// The most the server stores of one feedback's log zip
// (server controller.LogFileMaxByteCount).
const testingServerLogFileMaxByteCount = int64(100 * 1024 * 1024)

// A log file description for the selection, at a path nothing reads.
func testingUploadLogFileInfo(source string, name string, byteCount int64, modifiedMillis int64) *LogFileInfo {
	return &LogFileInfo{
		Name:           name,
		Path:           filepath.Join("/unused", source, name),
		Source:         source,
		Severity:       logSeverityOf(name),
		ByteCount:      byteCount,
		ModifiedMillis: modifiedMillis,
	}
}

// Each file as <source>/<name>, in order.
func testingUploadLogSourceNames(logFileInfos []*LogFileInfo) []string {
	sourceNames := []string{}
	for _, logFileInfo := range logFileInfos {
		sourceNames = append(sourceNames, logFileInfo.Source+"/"+logFileInfo.Name)
	}
	return sourceNames
}

// Writes a log file with the content and modified time into dir, creating dir,
// and returns its path.
func writeTestingUploadLogFile(t *testing.T, dir string, name string, content string, modTime time.Time) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("MkdirAll(%q): %v", dir, err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatalf("Chtimes(%q): %v", path, err)
	}
	return path
}

// The content of each entry of a zip, by entry name.
func readTestingZip(t *testing.T, zipBytes []byte) map[string]string {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	entryNameContents := map[string]string{}
	for _, zipFile := range reader.File {
		entry, err := zipFile.Open()
		if err != nil {
			t.Fatalf("open %q: %v", zipFile.Name, err)
		}
		content, err := io.ReadAll(entry)
		entry.Close()
		if err != nil {
			t.Fatalf("read %q: %v", zipFile.Name, err)
		}
		entryNameContents[zipFile.Name] = string(content)
	}
	return entryNameContents
}

// The entry names, sorted, for failure messages.
func testingZipEntryNames(entryNameContents map[string]string) []string {
	entryNames := []string{}
	for entryName := range entryNameContents {
		entryNames = append(entryNames, entryName)
	}
	slices.Sort(entryNames)
	return entryNames
}

// The server drops a log zip over 100 MiB whole, and the files under a log
// root have no such bound: the app and the extension each keep up to 4 files
// of 16 MiB at a start and add one every time a file fills. The upload takes
// the newest files that fit the cap, and a file that does not fit does not
// stop an older, smaller one from being sent.
func TestSelectUploadLogFilesKeepsTheNewestThatFitTheCap(t *testing.T) {
	logFileInfos := []*LogFileInfo{
		testingUploadLogFileInfo("app", "urnetwork.h.u.log.INFO.e", 200, 1000),
		testingUploadLogFileInfo("extension", "urnetwork.h.u.log.INFO.c", 500, 3000),
		testingUploadLogFileInfo("extension", "urnetwork.h.u.log.INFO.a", 400, 5000),
		testingUploadLogFileInfo("app", "urnetwork.h.u.log.INFO.d", 200, 2000),
		testingUploadLogFileInfo("app", "urnetwork.h.u.log.INFO.b", 300, 4000),
	}
	givenLogFileInfos := slices.Clone(logFileInfos)

	selectedLogFileInfos := selectUploadLogFiles(logFileInfos, 1000, 0)

	wantSourceNames := []string{
		"extension/urnetwork.h.u.log.INFO.a",
		"app/urnetwork.h.u.log.INFO.b",
		// c (500) would make 1200; d (200) still fits after it
		"app/urnetwork.h.u.log.INFO.d",
		// e (200) would make 1100
	}
	if sourceNames := testingUploadLogSourceNames(selectedLogFileInfos); !slices.Equal(sourceNames, wantSourceNames) {
		t.Fatalf("selected %v, want %v", sourceNames, wantSourceNames)
	}
	selectedByteCount := int64(0)
	for _, logFileInfo := range selectedLogFileInfos {
		selectedByteCount += logFileInfo.ByteCount
	}
	if 1000 < selectedByteCount {
		t.Fatalf("selected %d bytes, over the 1000 byte cap", selectedByteCount)
	}
	if !slices.Equal(logFileInfos, givenLogFileInfos) {
		t.Fatal("the selection reordered the caller's slice")
	}

	// one file larger than the cap on its own is never sent
	bigLogFileInfos := []*LogFileInfo{
		testingUploadLogFileInfo("app", "urnetwork.h.u.log.INFO.big", 1001, 9000),
		testingUploadLogFileInfo("app", "urnetwork.h.u.log.INFO.small", 10, 1),
	}
	if sourceNames := testingUploadLogSourceNames(selectUploadLogFiles(bigLogFileInfos, 1000, 0)); !slices.Equal(sourceNames, []string{"app/urnetwork.h.u.log.INFO.small"}) {
		t.Fatalf("selected %v, want only the small file", sourceNames)
	}

	// each file's zip entry counts against the cap as well: two files of 400
	// bytes fit 1000 bytes, but not with 200 bytes of entry each
	pairLogFileInfos := []*LogFileInfo{
		testingUploadLogFileInfo("app", "urnetwork.h.u.log.INFO.newer", 400, 2),
		testingUploadLogFileInfo("extension", "urnetwork.h.u.log.INFO.older", 400, 1),
	}
	if sourceNames := testingUploadLogSourceNames(selectUploadLogFiles(pairLogFileInfos, 1000, 0)); len(sourceNames) != 2 {
		t.Fatalf("selected %v without entry costs, want both", sourceNames)
	}
	if sourceNames := testingUploadLogSourceNames(selectUploadLogFiles(pairLogFileInfos, 1000, 200)); !slices.Equal(sourceNames, []string{"app/urnetwork.h.u.log.INFO.newer"}) {
		t.Fatalf("selected %v with 200 bytes per entry, want only the newer file", sourceNames)
	}

	// files written at the same time are taken in a fixed order
	tiedLogFileInfos := []*LogFileInfo{
		testingUploadLogFileInfo("extension", "urnetwork.h.u.log.INFO.y", 1, 7),
		testingUploadLogFileInfo("app", "urnetwork.h.u.log.INFO.z", 1, 7),
		testingUploadLogFileInfo("app", "urnetwork.h.u.log.INFO.x", 1, 7),
	}
	wantTiedSourceNames := []string{
		"app/urnetwork.h.u.log.INFO.x",
		"app/urnetwork.h.u.log.INFO.z",
		"extension/urnetwork.h.u.log.INFO.y",
	}
	if sourceNames := testingUploadLogSourceNames(selectUploadLogFiles(tiedLogFileInfos, 1000, 0)); !slices.Equal(sourceNames, wantTiedSourceNames) {
		t.Fatalf("tied files selected as %v, want %v", sourceNames, wantTiedSourceNames)
	}
}

// On ios the app and the network extension log into Logs/app and
// Logs/extension of their shared app group container. The device's upload
// (the extension's, while the tunnel is up) used to zip only its own
// directory, so the app's logs never reached support. Either process's upload
// now holds every process's glog files under the root, each under its process
// name, and nothing else that sits there.
func TestUploadLogsHoldEveryProcessUnderTheLogRoot(t *testing.T) {
	restoreTestingLogDir(t)

	root := t.TempDir()
	modTime := time.Now().Add(-time.Hour)
	appName := "urnetwork.host.user.log.INFO.20260901-000000.101"
	extensionName := "urnetwork.host.user.log.INFO.20260901-000000.202"
	extensionErrorName := "urnetwork.host.user.log.ERROR.20260901-000000.202"
	writeTestingUploadLogFile(t, filepath.Join(root, "app"), appName, "app line\n", modTime)
	extensionPath := writeTestingUploadLogFile(t, filepath.Join(root, "extension"), extensionName, "extension line\n", modTime)
	writeTestingUploadLogFile(t, filepath.Join(root, "extension"), extensionErrorName, "extension error line\n", modTime)
	// what else may sit in the log directories is never uploaded
	writeTestingUploadLogFile(t, filepath.Join(root, "extension"), "notes.txt", "not a log\n", modTime)
	writeTestingUploadLogFile(t, filepath.Join(root, "extension"), "logs-earlier.zip", "an earlier upload\n", modTime)
	writeTestingUploadLogFile(t, filepath.Join(root, "extension", "nested"), "urnetwork.host.user.log.INFO.20260901-000000.303", "nested line\n", modTime)
	if err := os.Symlink(extensionPath, filepath.Join(root, "extension", "urnetwork.INFO")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	// this process is the extension, the one the app asks while the tunnel is up
	if err := SetLogDirForProcess(root, "extension"); err != nil {
		t.Fatalf("SetLogDirForProcess: %v", err)
	}

	zipPath, err := zipUploadLogs(uploadLogsMaxByteCount, connect.DefaultLogger())
	if err != nil {
		t.Fatalf("zipUploadLogs = %v", err)
	}
	if filepath.Dir(zipPath) != filepath.Join(root, "extension") {
		t.Fatalf("the zip was written to %q, want this process's log directory", zipPath)
	}
	zipBytes, err := os.ReadFile(zipPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	entryNameContents := readTestingZip(t, zipBytes)

	wantEntryNameContents := map[string]string{
		"app/" + appName:                  "app line\n",
		"extension/" + extensionName:      "extension line\n",
		"extension/" + extensionErrorName: "extension error line\n",
	}
	for entryName, wantContent := range wantEntryNameContents {
		if entryNameContents[entryName] != wantContent {
			t.Errorf("the upload has %q = %q, want %q (entries %v)", entryName, entryNameContents[entryName], wantContent, testingZipEntryNames(entryNameContents))
		}
	}
	for entryName := range entryNameContents {
		if !strings.HasPrefix(entryName, "app/") && !strings.HasPrefix(entryName, "extension/") {
			t.Errorf("the upload has %q, outside the process directories", entryName)
		}
		if logSeverityOf(entryName) == "" || strings.Contains(entryName, "nested") || strings.HasSuffix(entryName, "urnetwork.INFO") {
			t.Errorf("the upload has %q, which is not a glog file of a process directory", entryName)
		}
	}

	// the inventory lists what the upload sends
	inventorySourceNames := testingUploadLogSourceNames(UploadLogsInventory().getAll())
	for entryName := range wantEntryNameContents {
		if !slices.Contains(inventorySourceNames, entryName) {
			t.Errorf("UploadLogsInventory lacks %q: %v", entryName, inventorySourceNames)
		}
	}
	for _, sourceName := range inventorySourceNames {
		if _, ok := entryNameContents[sourceName]; !ok {
			t.Errorf("UploadLogsInventory lists %q, which the upload does not hold", sourceName)
		}
	}
}

// Windows and Linux log into one directory (SetLogDir). Their uploads keep
// the bare file names they always had.
func TestUploadLogsUnderALegacyLogDirectoryKeepBareNames(t *testing.T) {
	restoreTestingLogDir(t)

	dir := t.TempDir()
	name := "urnetwork.host.user.log.INFO.20260901-000000.404"
	writeTestingUploadLogFile(t, dir, name, "legacy line\n", time.Now().Add(-time.Hour))
	if err := SetLogDir(dir); err != nil {
		t.Fatalf("SetLogDir: %v", err)
	}

	zipPath, err := zipUploadLogs(uploadLogsMaxByteCount, connect.DefaultLogger())
	if err != nil {
		t.Fatalf("zipUploadLogs = %v", err)
	}
	zipBytes, err := os.ReadFile(zipPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	entryNameContents := readTestingZip(t, zipBytes)
	if entryNameContents[name] != "legacy line\n" {
		t.Fatalf("the upload has %q = %q, want the bare name (entries %v)", name, entryNameContents[name], testingZipEntryNames(entryNameContents))
	}
	for entryName := range entryNameContents {
		if strings.Contains(entryName, "/") {
			t.Errorf("the upload has %q, a path under a legacy log directory", entryName)
		}
	}
}

// The live log file of a running process keeps growing while it is zipped. The
// upload copies each file only up to the size it was planned at, so the zip
// stays under the cap, and a file that is gone by the time it is zipped (a
// process pruned or rotated it) is left out instead of failing the upload.
func TestUploadLogsZipOnlyThePlannedBytes(t *testing.T) {
	dir := t.TempDir()
	modTime := time.Now().Add(-time.Hour)
	growingPath := writeTestingUploadLogFile(t, dir, "urnetwork.host.user.log.INFO.20260901-000000.505", "planned part|grown since", modTime)

	plan := &uploadLogsPlan{
		logFileInfos: []*LogFileInfo{
			{
				Name:           filepath.Base(growingPath),
				Path:           growingPath,
				Source:         "app",
				Severity:       "INFO",
				ByteCount:      int64(len("planned part|")),
				ModifiedMillis: modTime.UnixMilli(),
			},
			{
				Name:           "urnetwork.host.user.log.INFO.20260901-000000.506",
				Path:           filepath.Join(dir, "gone"),
				Source:         "app",
				Severity:       "INFO",
				ByteCount:      10,
				ModifiedMillis: modTime.UnixMilli(),
			},
		},
		perProcess: true,
	}

	var zipBuffer bytes.Buffer
	fileCount, err := plan.writeZip(&zipBuffer, connect.DefaultLogger())
	if err != nil {
		t.Fatalf("writeZip = %v, want nil: a file that is gone leaves the rest of the upload", err)
	}
	if fileCount != 1 {
		t.Fatalf("zipped %d files, want 1", fileCount)
	}
	entryNameContents := readTestingZip(t, zipBuffer.Bytes())
	if content := entryNameContents["app/"+filepath.Base(growingPath)]; content != "planned part|" {
		t.Fatalf("the growing file was zipped as %q, want only the planned %q", content, "planned part|")
	}
	if len(entryNameContents) != 1 {
		t.Fatalf("the upload has %v, want only the file that is still there", testingZipEntryNames(entryNameContents))
	}
}

// The cap is below the server's 100 MiB with room for what a zip adds. Each
// file counts against the cap with its entry (uploadLogsEntryByteCount), so
// what is left is deflate's few bytes per block of bytes that do not compress
// and the zip's end records. A zip of incompressible logs with long names is
// measured against that, and the most a full upload can be fits the server's
// cap.
func TestUploadLogsZipStaysUnderTheServerCap(t *testing.T) {
	dir := t.TempDir()
	modTime := time.Now().Add(-time.Hour)
	randomBytes := make([]byte, 4*1024*1024)
	if _, err := rand.Read(randomBytes); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	logFileInfos := []*LogFileInfo{}
	rawByteCount := int64(0)
	for i, byteCount := range []int{len(randomBytes), 1024 * 1024, 1} {
		name := "urnetwork." + strings.Repeat("h", 200) + ".user.log.INFO.20260901-000000.60" + string(rune('0'+i))
		path := writeTestingUploadLogFile(t, filepath.Join(dir, "extension"), name, string(randomBytes[:byteCount]), modTime)
		logFileInfos = append(logFileInfos, &LogFileInfo{
			Name:           name,
			Path:           path,
			Source:         "extension",
			Severity:       "INFO",
			ByteCount:      int64(byteCount),
			ModifiedMillis: modTime.UnixMilli(),
		})
		rawByteCount += int64(byteCount)
	}

	plan := &uploadLogsPlan{
		logFileInfos: logFileInfos,
		perProcess:   true,
	}
	var zipBuffer bytes.Buffer
	if _, err := plan.writeZip(&zipBuffer, connect.DefaultLogger()); err != nil {
		t.Fatalf("writeZip = %v", err)
	}

	// what deflate may add to bytes that do not compress, at most 0.1%, and the
	// zip's end records (with the zip64 ones)
	deflateByteCount := func(rawByteCount int64) int64 {
		return rawByteCount / 1024
	}
	endByteCount := int64(22 + 56 + 20)

	plannedByteCount := rawByteCount + int64(len(logFileInfos))*uploadLogsEntryByteCount
	t.Logf("a zip of %d incompressible bytes in %d files is %d bytes", rawByteCount, len(logFileInfos), zipBuffer.Len())
	if allowedByteCount := plannedByteCount + deflateByteCount(rawByteCount) + endByteCount; allowedByteCount < int64(zipBuffer.Len()) {
		t.Fatalf("a zip of %d incompressible bytes in %d files is %d bytes, over the allowed %d", rawByteCount, len(logFileInfos), zipBuffer.Len(), allowedByteCount)
	}

	if worstByteCount := uploadLogsMaxByteCount + deflateByteCount(uploadLogsMaxByteCount) + endByteCount; testingServerLogFileMaxByteCount < worstByteCount {
		t.Fatalf("a full upload can be %d bytes, over the server's %d", worstByteCount, testingServerLogFileMaxByteCount)
	}
}

// One request the test api received.
type testingUploadRequest struct {
	method        string
	path          string
	authorization string
	body          []byte
}

// The app process uploads through its own api while the device cannot be
// asked: on ios while the tunnel, and with it the network extension, is down.
// It sends one zip to /log/<feedback id>/upload holding the extension's last
// logs from the shared root as well as the app's own, including a line the app
// wrote just before (the upload flushes glog), and removes the zip once the
// post is done.
func TestApiUploadLogsPostsOneZipOfEveryProcess(t *testing.T) {
	restoreTestingLogDir(t)

	root := t.TempDir()
	extensionName := "urnetwork.host.user.log.INFO.20260901-000000.707"
	writeTestingUploadLogFile(t, filepath.Join(root, "extension"), extensionName, "extension line from the last tunnel\n", time.Now().Add(-time.Hour))
	if err := SetLogDirForProcess(root, "app"); err != nil {
		t.Fatalf("SetLogDirForProcess: %v", err)
	}
	LogAppInfo("feedback", "app line before the upload")

	var stateLock sync.Mutex
	uploadRequests := []testingUploadRequest{}
	_, api := newTestApi(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		func() {
			stateLock.Lock()
			defer stateLock.Unlock()
			uploadRequests = append(uploadRequests, testingUploadRequest{
				method:        r.Method,
				path:          r.URL.Path,
				authorization: r.Header.Get("Authorization"),
				body:          body,
			})
		}()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{}"))
	}))
	api.SetByJwt("test-jwt")

	type uploadResult struct {
		result *UploadLogsResult
		err    error
	}
	uploadResultChannel := make(chan uploadResult, 1)
	feedbackId := NewId().String()
	err := api.UploadLogs(feedbackId, connect.NewApiCallback[*UploadLogsResult](func(result *UploadLogsResult, err error) {
		uploadResultChannel <- uploadResult{
			result: result,
			err:    err,
		}
	}))
	if err != nil {
		t.Fatalf("UploadLogs = %v", err)
	}
	select {
	case r := <-uploadResultChannel:
		if r.err != nil || r.result == nil || r.result.Error != nil {
			t.Fatalf("the upload ended with %+v, %v", r.result, r.err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the upload did not finish")
	}

	receivedUploadRequests := func() []testingUploadRequest {
		stateLock.Lock()
		defer stateLock.Unlock()
		return slices.Clone(uploadRequests)
	}()
	if len(receivedUploadRequests) != 1 {
		t.Fatalf("the api got %d requests, want one upload", len(receivedUploadRequests))
	}
	uploadRequest := receivedUploadRequests[0]
	if uploadRequest.method != "POST" || uploadRequest.path != "/log/"+feedbackId+"/upload" {
		t.Fatalf("the upload was %s %s", uploadRequest.method, uploadRequest.path)
	}
	if uploadRequest.authorization != "Bearer test-jwt" {
		t.Fatalf("the upload was authorized with %q", uploadRequest.authorization)
	}
	entryNameContents := readTestingZip(t, uploadRequest.body)
	if content := entryNameContents["extension/"+extensionName]; content != "extension line from the last tunnel\n" {
		t.Errorf("the upload lacks the extension's log (entries %v)", testingZipEntryNames(entryNameContents))
	}
	appLine := false
	for entryName, content := range entryNameContents {
		if strings.HasPrefix(entryName, "app/") && strings.Contains(content, "] [app][feedback] app line before the upload\n") {
			appLine = true
		}
	}
	if !appLine {
		t.Errorf("the upload lacks the app's own log line (entries %v)", testingZipEntryNames(entryNameContents))
	}

	zipPaths, err := filepath.Glob(filepath.Join(root, "*", "logs-*.zip"))
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	if len(zipPaths) != 0 {
		t.Fatalf("the upload left %v behind", zipPaths)
	}
}

// While the tunnel is up the app asks the device, in the network extension,
// to upload. That zip is built from the extension's view of the shared log
// root, so it holds the app's files too, but the app's newest lines are in the
// app's glog buffer, which the extension cannot flush. The remote flushes this
// process's glog before it asks, whether or not the rpc can carry the request.
//
// glog also flushes on its own every 30 seconds (and on a warning, which
// nothing here logs). One such flush could put one round's line on disk by
// itself, but not both rounds' lines, so two rounds fail without the remote's
// flush whenever it happens.
func TestDeviceRemoteUploadLogsFlushesThisProcessFirst(t *testing.T) {
	restoreTestingLogDir(t)

	deviceRemote := newTestDeviceRemoteWithNoService(t)

	dir := t.TempDir()
	if err := SetLogDir(dir); err != nil {
		t.Fatalf("SetLogDir: %v", err)
	}

	writtenLog := func() string {
		logPaths, err := filepath.Glob(filepath.Join(dir, "*.log.INFO.*"))
		if err != nil {
			t.Fatalf("Glob: %v", err)
		}
		written := ""
		for _, logPath := range logPaths {
			content, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			written += string(content)
		}
		return written
	}

	for round := 0; round < 2; round += 1 {
		marker := "app line buffered before upload " + NewId().String()
		glog.Info(marker)

		if err := deviceRemote.UploadLogs(NewId().String(), nil); err == nil {
			t.Fatal("UploadLogs = nil with no rpc service, want an error so the app uploads itself")
		}

		if !strings.Contains(writtenLog(), marker) {
			t.Fatalf("round %d: this process's buffered line was not on disk when the device was asked to upload", round)
		}
	}
}

// The network extension's device uploads with the device's api. Its zip holds
// the app's logs from the shared root as well as its own.
func TestDeviceLocalUploadLogsHoldTheAppsLogs(t *testing.T) {
	restoreTestingLogDir(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	device, err := testing_newViewControllerDevice(ctx)
	if err != nil {
		t.Fatalf("device: %v", err)
	}
	defer device.Close()

	root := t.TempDir()
	appName := "urnetwork.host.user.log.INFO.20260901-000000.808"
	writeTestingUploadLogFile(t, filepath.Join(root, "app"), appName, "app line while the tunnel is up\n", time.Now().Add(-time.Hour))
	if err := SetLogDirForProcess(root, "extension"); err != nil {
		t.Fatalf("SetLogDirForProcess: %v", err)
	}
	LogAppInfo("tunnel", "extension line before the upload")

	postedZipChannel := make(chan []byte, 1)
	device.GetApi().setHttpPostStreamRaw(func(ctx context.Context, requestUrl string, body io.Reader, byJwt string) ([]byte, error) {
		zipBytes, err := io.ReadAll(body)
		if err != nil {
			return nil, err
		}
		postedZipChannel <- zipBytes
		return []byte("{}"), nil
	})

	if err := device.UploadLogs(NewId().String(), nil); err != nil {
		t.Fatalf("UploadLogs = %v", err)
	}
	var zipBytes []byte
	select {
	case zipBytes = <-postedZipChannel:
	case <-time.After(30 * time.Second):
		t.Fatal("the device did not post the upload")
	}
	entryNameContents := readTestingZip(t, zipBytes)
	if content := entryNameContents["app/"+appName]; content != "app line while the tunnel is up\n" {
		t.Errorf("the extension's upload lacks the app's log (entries %v)", testingZipEntryNames(entryNameContents))
	}
	extensionLine := false
	for entryName, content := range entryNameContents {
		if strings.HasPrefix(entryName, "extension/") && strings.Contains(content, "] [app][tunnel] extension line before the upload\n") {
			extensionLine = true
		}
	}
	if !extensionLine {
		t.Errorf("the extension's upload lacks its own log line (entries %v)", testingZipEntryNames(entryNameContents))
	}
}

// The extension's rpc server serves one request at a time, and the remote
// holds its state lock across each call. An upload answered only after its zip,
// which reads up to the cap from disk, stalled every other call of the app's
// remote for that long. The server now starts the upload and answers at once,
// so the remote's call returns while the upload is held before its zip.
//
// The hold ends when the test releases it after the call returns, or after ten
// seconds (under the remote's 60 second call timeout) so that a server that
// answers after the zip fails instead of hanging. Which of the two ended it is
// the result.
func TestDeviceLocalRpcUploadLogsAnswersBeforeTheZip(t *testing.T) {
	restoreTestingLogDir(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deviceLocal, deviceRemote := testing_newSyncedDeviceLocalRemote(t, ctx)

	root := t.TempDir()
	if err := SetLogDirForProcess(root, "extension"); err != nil {
		t.Fatalf("SetLogDirForProcess: %v", err)
	}

	releaseChannel := make(chan struct{})
	releasedByTestChannel := make(chan bool, 1)
	deviceLocal.testingBeforeUploadLogs = func() {
		select {
		case <-releaseChannel:
			releasedByTestChannel <- true
		case <-time.After(10 * time.Second):
			releasedByTestChannel <- false
		}
	}
	postedChannel := make(chan struct{}, 1)
	deviceLocal.GetApi().setHttpPostStreamRaw(func(ctx context.Context, requestUrl string, body io.Reader, byJwt string) ([]byte, error) {
		postedChannel <- struct{}{}
		return []byte("{}"), nil
	})

	if err := deviceRemote.UploadLogs(NewId().String(), nil); err != nil {
		t.Fatalf("UploadLogs = %v", err)
	}
	close(releaseChannel)

	select {
	case releasedByTest := <-releasedByTestChannel:
		if !releasedByTest {
			t.Fatal("the remote's call returned only after the upload's hold ran out: the server answered after the zip")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the upload never started")
	}
	select {
	case <-postedChannel:
	case <-time.After(30 * time.Second):
		t.Fatal("the upload was not posted")
	}
}
