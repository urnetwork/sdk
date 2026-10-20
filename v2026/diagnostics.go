package sdk

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// logSeverities are the glog severity tags, in the order glog defines them.
// They appear in a log file name as the segment immediately after ".log."
// (glog/glog_file.go:124-140).
var logSeverities = []string{"INFO", "WARNING", "ERROR", "FATAL"}

// LogFileInfo describes one log file the bundle could include.
//
// gomobile does not support struct composition, so this is flat, and every
// field is a bindable scalar.
type LogFileInfo struct {
	// Name is the glog file name. glog embeds the host and pid, so in
	// practice it is unique across the whole export, but nothing enforces
	// that across per-process directories -- Source plus Name is what is
	// guaranteed unique, and it is the zip path an entry is written to.
	Name string
	// Path is the absolute path on disk.
	Path string
	// Source is the writing process: the per-process subdirectory name,
	// e.g. "app" or "extension".
	Source string
	// Severity is INFO, WARNING, ERROR or FATAL.
	Severity  string
	ByteCount int64
	// ModifiedMillis is unix millis, 0 when unknown.
	ModifiedMillis int64
}

type LogFileInfoList struct {
	exportedList[*LogFileInfo]
}

func NewLogFileInfoList() *LogFileInfoList {
	return &LogFileInfoList{
		exportedList: *newExportedList[*LogFileInfo](),
	}
}

// logSeverityOf returns the severity named in a glog file name, or "" when the
// name is not a glog log file.
func logSeverityOf(name string) string {
	for _, severity := range logSeverities {
		if strings.Contains(name, ".log."+severity) {
			return severity
		}
	}
	return ""
}

// LogInventory enumerates every log file under the recorded log root, across
// every process that has written there.
//
// Symlinks are skipped: glog maintains a <program>.<SEVERITY> symlink beside
// each real file, and following it would list the same bytes twice.
func LogInventory() *LogFileInfoList {
	inventory, _, _ := logInventory()
	return inventory
}

// logRootSourceName labels a failure to read the log root itself, which is not
// attributable to any one process directory.
const logRootSourceName = "log root"

// logInventory is LogInventory plus the directories it could not read, as
// "<source>: <reason>" entries, plus where each source was read from.
//
// The third result maps a source to the directory the enumeration read it
// from: the shared per-process root in the normal layout, where the files live
// in <root>/<source>, or, under the legacy single-directory configuration,
// that directory itself. The manifest reports it per source because the
// process that BUILDS the manifest is not always the process that reads the
// files -- on ios the manifest comes from the extension over the rpc while the
// archive is assembled in the app, from the app's container.
//
// The exported LogInventory drops them because its bound signature has nowhere
// to put them, but ExportDiagnosticBundle must not: an unreadable log root or
// per-process directory used to be omitted from the bundle with nothing
// recorded as missing, so a user whose Logs/extension directory had become
// unreadable got a zip with an empty NOT INCLUDED block and an ExportResult
// reporting zero missing sources, while a whole process's logs were absent.
// The rule this export holds to is that a source that cannot be read is
// recorded as missing, never silently dropped.
func logInventory() (*LogFileInfoList, []unreadableSource, map[string]string) {
	inventory := NewLogFileInfoList()
	unreadable := []unreadableSource{}
	sourceRoots := map[string]string{}

	root := GetLogRoot()
	if root == "" {
		// legacy single-directory configuration: report it as one source, read
		// from that directory and not from a root above it
		if dir := GetLogDir(); dir != "" {
			sourceRoots["app"] = dir
			if err := appendLogFilesIn(inventory, dir, "app"); err != nil {
				unreadable = append(unreadable, unreadableSource{"app", err.Error()})
			}
		}
		return inventory, unreadable, sourceRoots
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		sourceRoots[logRootSourceName] = root
		return inventory, append(unreadable, unreadableSource{logRootSourceName, err.Error()}), sourceRoots
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		sourceRoots[entry.Name()] = root
		if err := appendLogFilesIn(inventory, filepath.Join(root, entry.Name()), entry.Name()); err != nil {
			unreadable = append(unreadable, unreadableSource{entry.Name(), err.Error()})
		}
	}
	return inventory, unreadable, sourceRoots
}

// unreadableSource is a log source directory that could not be listed.
type unreadableSource struct {
	Source string
	Reason string
}

func appendLogFilesIn(inventory *LogFileInfoList, dir string, source string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		// Type()&ModeSymlink catches glog's severity symlinks without a stat
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		severity := logSeverityOf(entry.Name())
		if severity == "" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		inventory.Add(&LogFileInfo{
			Name:           entry.Name(),
			Path:           filepath.Join(dir, entry.Name()),
			Source:         source,
			Severity:       severity,
			ByteCount:      info.Size(),
			ModifiedMillis: info.ModTime().UnixMilli(),
		})
	}
	return nil
}

// ExportOptions selects what an exported bundle contains.
type ExportOptions struct {
	// Redact maps ip addresses and uuid-shaped ids to per-export tokens.
	Redact bool
	// IncludeManifest writes manifest.json. The manifest body is supplied by
	// the platform via SetManifestJson, because on ios the device-side state
	// lives in the extension and arrives over the rpc.
	IncludeManifest bool
	// IncludePlatformLogs writes platform log files (platform/NAME.txt) from
	// SetPlatformLog entries.
	IncludePlatformLogs bool
	// SelectedNames limits the export to these files. Empty means every file
	// -- so a picker offering "export the selected files" must refuse to run
	// on an empty selection rather than pass it through, or it exports
	// everything.
	//
	// An entry matches a LogFileInfo either by bare Name or by the
	// source-qualified form, Source, a slash and Name, which is what the
	// entry is called inside the zip.
	SelectedNames *StringList

	manifestJson  string
	platformLogs  *StringList
	platformNames *StringList
	missingNames  *StringList
	missingWhy    *StringList
}

func NewExportOptions() *ExportOptions {
	options := &ExportOptions{}
	options.initLists()
	return options
}

// initLists fills in any list field left nil, and is called by every method
// that reads or writes one, plus by the exporter itself.
//
// ExportOptions reaches the exporter as a zero value on paths that never run
// NewExportOptions: the c abi json-unmarshals into a bare
// &sdk.ExportOptions{} (cgo/exports_gen.go, urnet_export_diagnostic_bundle)
// and leaves every unexported list nil, and a language binding that emits a
// zero-value constructor alongside the NewExportOptions one does the same.
// The lists are embedded-struct pointers, so reading Len() off a nil one is a
// nil dereference, not a zero result -- on the c abi that panic was recovered
// into a NULL return with no error set, and through a gomobile seq bridge it
// is an app crash. A zero-value ExportOptions must behave exactly like a
// fresh one.
func (self *ExportOptions) initLists() {
	if self.SelectedNames == nil {
		self.SelectedNames = NewStringList()
	}
	if self.platformLogs == nil {
		self.platformLogs = NewStringList()
	}
	if self.platformNames == nil {
		self.platformNames = NewStringList()
	}
	if self.missingNames == nil {
		self.missingNames = NewStringList()
	}
	if self.missingWhy == nil {
		self.missingWhy = NewStringList()
	}
}

// SetManifestJson supplies the manifest body, normally
// Device.DiagnosticManifestJson().
func (self *ExportOptions) SetManifestJson(manifestJson string) {
	self.manifestJson = manifestJson
}

// AddPlatformLog adds one platform log entry, written to platform/<name>.
// Android passes its logcat dump here.
func (self *ExportOptions) AddPlatformLog(name string, content string) {
	self.initLists()
	self.platformNames.Add(name)
	self.platformLogs.Add(content)
}

// MissingSourceReason records a source that could not be read, so the bundle
// says so instead of silently omitting it.
func (self *ExportOptions) MissingSourceReason(source string, reason string) {
	self.initLists()
	self.missingNames.Add(source)
	self.missingWhy.Add(reason)
}

// selects reports whether one inventory entry is included in the export.
//
// An empty selection means every file. A selected entry matches either the
// bare Name or the source-qualified form: Name alone is not guaranteed unique
// across per-process directories, while the qualified form is the zip path,
// so a picker that qualifies its keys can address exactly one file rather
// than every file that happens to share a name.
func (self *ExportOptions) selects(info *LogFileInfo) bool {
	self.initLists()
	if self.SelectedNames.Len() == 0 {
		return true
	}
	return self.SelectedNames.Contains(info.Name) ||
		self.SelectedNames.Contains(info.Source+"/"+info.Name)
}

// ExportResult reports what was written.
type ExportResult struct {
	ByteCount int64
	FileCount int
	// MissingSources holds human-readable "<source>: <reason>" entries.
	MissingSources *StringList
}

// ExportDiagnosticBundle writes a zip of the selected logs to destPath.
//
// A source that cannot be read is reported in the result and in the bundle's
// README, never fatal: an ios build whose provisioning profile predates the
// app group must still export the logs it can reach. Only an unwritable
// destination is an error.
//
// This flushes THIS process's glog. When another process writes some of the
// logs -- on ios the extension writes logs/extension -- the caller must call
// Device.FlushGlog() first, or the newest lines of that process's output are
// still in its memory and are missing from the bundle.
func ExportDiagnosticBundle(destPath string, opts *ExportOptions) (*ExportResult, error) {
	if opts == nil {
		opts = NewExportOptions()
	}
	opts.initLists()

	// A redacted bundle that cannot back its per-export salt with real
	// randomness must not be produced at all -- so this is checked, and can
	// fail, before anything is written to destPath. A RAW export never
	// reaches this branch and is unaffected.
	var transform func(string) string
	if opts.Redact {
		redactor, err := newLogRedactor()
		if err != nil {
			return nil, fmt.Errorf("redacted export not produced: a secure random salt was unavailable: %w", err)
		}
		transform = redactor.redactLine
	}

	FlushGlog()

	result := &ExportResult{MissingSources: NewStringList()}
	sources := newExportSourceReport()
	declaredMissing := map[string]bool{}
	for i := 0; i < opts.missingNames.Len(); i += 1 {
		result.MissingSources.Add(opts.missingNames.Get(i) + ": " + opts.missingWhy.Get(i))
		sources.unavailable(opts.missingNames.Get(i), opts.missingWhy.Get(i))
		declaredMissing[opts.missingNames.Get(i)] = true
	}

	zipFile, err := os.Create(destPath)
	if err != nil {
		return nil, err
	}
	defer zipFile.Close()

	zipWriter := zip.NewWriter(zipFile)

	// os.Create has already truncated or created destPath, so from here on a
	// returned error must not leave a half-written zip behind for the platform
	// to hand to the user or to a share sheet. Close before removing: on
	// windows, where the desktop c abi artifacts run, an open file cannot be
	// unlinked.
	fail := func(err error) (*ExportResult, error) {
		zipWriter.Close()
		zipFile.Close()
		os.Remove(destPath)
		return nil, err
	}

	inventory, unreadable, sourceRoots := logInventory()
	// from here on every source entry names the directory its files were
	// actually read from, in THIS process
	sources.withRoots(sourceRoots)
	for _, entry := range unreadable {
		if declaredMissing[entry.Source] {
			// the platform already said why this source is missing, in words
			// meant for a person; two lines about one source in the summary
			// and the README read as two separate problems
			continue
		}
		result.MissingSources.Add(entry.Source + ": " + entry.Reason)
		sources.unavailable(entry.Source, entry.Reason)
	}
	for i := 0; i < inventory.Len(); i += 1 {
		info := inventory.Get(i)
		if !opts.selects(info) {
			continue
		}
		f, err := os.Open(info.Path)
		if err != nil {
			result.MissingSources.Add(info.Name + ": " + err.Error())
			sources.incomplete(info.Source, info.Name, err.Error())
			continue
		}
		fi, err := f.Stat()
		if err != nil {
			f.Close()
			result.MissingSources.Add(info.Name + ": " + err.Error())
			sources.incomplete(info.Source, info.Name, err.Error())
			continue
		}
		err = zipWriteEntry(zipWriter, "logs/"+info.Source+"/"+info.Name, f, fi, transform)
		f.Close()
		if err != nil {
			// Reading one log file is not allowed to end the export. An i/o
			// error partway through a file, or a line past the redaction
			// scanner's 4 MiB cap (a corrupt or non-newline-terminated file),
			// used to abort here and return with a truncated zip still on
			// disk. Only an unwritable destination is fatal;
			// everything else is reported. Whatever was copied stays in the
			// archive as a valid entry, and the file is named as incomplete
			// rather than counted as exported.
			result.MissingSources.Add(info.Name + ": incomplete, " + err.Error())
			sources.incomplete(info.Source, info.Name, err.Error())
			continue
		}
		sources.included(info.Source, info.ByteCount)
		result.FileCount += 1
	}

	if opts.IncludeManifest {
		manifestJson := opts.manifestJson
		if manifestJson == "" {
			// No platform ever called SetManifestJson -- e.g. Android exporting
			// while disconnected, where deviceManager.device is null. Build the
			// fallback through buildDiagnosticManifestJson rather than a
			// hand-written literal, so this path and the normal one share a
			// single source of truth for the manifest's shape and can't drift
			// apart on key names again.
			manifestJson = fallbackDiagnosticManifestJson()
		}
		manifestJson, err := addExportMetadata(manifestJson, opts.Redact, sources.all())
		if err != nil {
			// The platform handed over something that is not a json object, so
			// there is nothing to merge into. Say so, and write the sdk's own
			// manifest instead of dropping the export metadata with it: a
			// bundle whose manifest cannot say whether it was redacted is
			// precisely what this metadata exists to prevent.
			result.MissingSources.Add("manifest: " + err.Error())
			manifestJson, err = addExportMetadata(fallbackDiagnosticManifestJson(), opts.Redact, sources.all())
			if err != nil {
				return fail(err)
			}
		}
		if err := zipWriteEntry(zipWriter, "manifest.json", strings.NewReader(manifestJson), nil, transform); err != nil {
			return fail(err)
		}
	}

	if opts.IncludePlatformLogs {
		for i := 0; i < opts.platformNames.Len(); i += 1 {
			err := zipWriteEntry(
				zipWriter,
				"platform/"+opts.platformNames.Get(i),
				strings.NewReader(opts.platformLogs.Get(i)),
				nil,
				transform,
			)
			if err != nil {
				return fail(err)
			}
		}
	}

	// README.txt goes through transform like every other entry. Its NOT
	// INCLUDED block quotes os.Open/os.Stat error strings, which carry the
	// absolute path of the file that could not be read -- on ios that path
	// contains the app group container uuid, the very identifier manifest.json
	// masks two entries earlier. An unredacted README would have made a
	// redacted bundle both mask and leak the same value, under a README
	// asserting that uuid-shaped ids are replaced. Platform-supplied
	// MissingSourceReason text lands here too and is equally unfiltered at
	// source.
	if err := zipWriteEntry(zipWriter, "README.txt", strings.NewReader(exportReadme(opts, result)), nil, transform); err != nil {
		return fail(err)
	}

	if err := zipWriter.Close(); err != nil {
		return fail(err)
	}
	if info, err := zipFile.Stat(); err == nil {
		result.ByteCount = info.Size()
	}
	return result, nil
}

func exportReadme(opts *ExportOptions, result *ExportResult) string {
	opts.initLists()
	var b strings.Builder
	b.WriteString("URnetwork diagnostic bundle\n\n")
	if opts.Redact {
		// No literal address may appear in this prose: README.txt is written
		// through the same transform as every log file, so an example address
		// here would come back out as a token and read as nonsense.
		b.WriteString("Mode: REDACTED. ip addresses and uuid-shaped ids are replaced by\n")
		b.WriteString("per-export tokens, in every form the logs write them: dotted quad,\n")
		b.WriteString("ipv6 literal, and the bracketed list of decimal bytes that a go %v of\n")
		b.WriteString("an address prints. One address reads as one token throughout this\n")
		b.WriteString("bundle whichever form each line wrote it in, and differently in any\n")
		b.WriteString("other bundle. The mapping is not reversible and the salt is not\n")
		b.WriteString("included. One exception: the unspecified address, the all-zero\n")
		b.WriteString("placeholder a log writes where it has no address to report, is left\n")
		b.WriteString("as written. It names no host, and masking it would hide the\n")
		b.WriteString("difference between no address and one.\n\n")
	} else {
		b.WriteString("Mode: RAW. Nothing is masked. At raised log verbosity this can include\n")
		b.WriteString("the destination addresses and ports of your traffic, and your client id.\n\n")
	}
	// only what this bundle actually contains: manifest.json is written only
	// when IncludeManifest is set, and no ios bundle has ever had a platform
	// directory, since iOS has no platform log source. A README promising
	// files that are not there sends the reader looking for them.
	b.WriteString("logs/<process>/  glog files, one directory per writing process\n")
	if opts.IncludeManifest {
		b.WriteString("manifest.json    device and connection state, the export mode, and what each source contributed\n")
	}
	if opts.IncludePlatformLogs && 0 < opts.platformNames.Len() {
		b.WriteString("platform/        platform-side logs\n")
	}
	b.WriteString("\n")
	if 0 < result.MissingSources.Len() {
		b.WriteString("NOT INCLUDED:\n")
		for i := 0; i < result.MissingSources.Len(); i += 1 {
			b.WriteString("  - " + result.MissingSources.Get(i) + "\n")
		}
	}
	return b.String()
}

// exportModeRedacted and exportModeRaw are the two values of the manifest's
// export_mode field, the machine-readable form of what README.txt says in
// prose.
const (
	exportModeRedacted = "redacted"
	exportModeRaw      = "raw"
)

// manifestSourceAvailability is one entry of the manifest's per-source
// availability list:
// {"source": "extension", "available": false, "reason": "app group container
// unavailable"}.
type manifestSourceAvailability struct {
	Source    string `json:"source"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
	FileCount int    `json:"file_count"`
	ByteCount int64  `json:"byte_count"`
	// Incomplete names files that were listed but could not be copied whole,
	// so "available: true, file_count: 2" is never read as "everything this
	// source had is in the bundle" when it is not.
	Incomplete []string `json:"incomplete,omitempty"`
	// LogRoot is the directory the export read this source from, in the
	// process that assembled the archive: the per-process root that contains
	// <root>/<source>, or the log directory itself under the legacy
	// single-directory configuration.
	//
	// It is per source, and it is the path a reader should follow, because the
	// manifest's own log fields come from the process that BUILT the manifest
	// -- on ios the extension, whose container is not the one the files were
	// read from. Empty when this export never enumerated the source, which is
	// the case for one the platform declared missing with no directory of its
	// own under this process's root.
	LogRoot string `json:"log_root,omitempty"`
}

// exportSourceReport accumulates the per-source availability list as the
// export runs.
type exportSourceReport struct {
	order   []string
	entries map[string]*manifestSourceAvailability
	// roots is where the enumeration read each source from, once it has run
	roots map[string]string
}

func newExportSourceReport() *exportSourceReport {
	return &exportSourceReport{entries: map[string]*manifestSourceAvailability{}}
}

// withRoots supplies the directory each source was read from, and backfills
// the entries already made -- the sources the platform declared missing, which
// are recorded before the enumeration runs. A source the enumeration never saw
// keeps an empty root, which is the truthful answer for it: nothing read it
// from anywhere.
func (self *exportSourceReport) withRoots(roots map[string]string) {
	self.roots = roots
	for source, entry := range self.entries {
		if entry.LogRoot == "" {
			entry.LogRoot = roots[source]
		}
	}
}

func (self *exportSourceReport) entry(source string) *manifestSourceAvailability {
	entry, ok := self.entries[source]
	if !ok {
		entry = &manifestSourceAvailability{Source: source}
		self.entries[source] = entry
		self.order = append(self.order, source)
	}
	if entry.LogRoot == "" {
		entry.LogRoot = self.roots[source]
	}
	return entry
}

func (self *exportSourceReport) included(source string, byteCount int64) {
	entry := self.entry(source)
	entry.FileCount += 1
	entry.ByteCount += byteCount
	if entry.Reason == "" {
		entry.Available = true
	}
}

func (self *exportSourceReport) unavailable(source string, reason string) {
	entry := self.entry(source)
	entry.Available = false
	entry.Reason = reason
}

func (self *exportSourceReport) incomplete(source string, name string, reason string) {
	entry := self.entry(source)
	entry.Incomplete = append(entry.Incomplete, name+": "+reason)
}

func (self *exportSourceReport) all() []manifestSourceAvailability {
	all := make([]manifestSourceAvailability, 0, len(self.order))
	for _, source := range self.order {
		all = append(all, *self.entries[source])
	}
	return all
}

// addExportMetadata merges what the sdk knows about THIS export into the
// manifest body the platform supplied.
//
// manifest.json is the DiagnosticManifestJson output plus export metadata,
// and three of those fields are safety-relevant: the mode, so
// a reader can tell a redacted bundle from a raw one without parsing English
// out of README.txt; the per-source availability list, so a source that was
// unreachable is distinguishable from a source that had nothing to say; and
// the verbosity, because it decides what a reader can expect to find at all --
// at the default level the connect package writes none of its V(1) contract
// and transport lines, so their absence means nothing. Neither platform
// augments the manifest, so if the sdk does not add these they exist nowhere
// machine-readable in the bundle.
//
// The other fields a support engineer would want -- app version and build,
// os version, device model -- are deliberately not here. The sdk cannot know them, and
// inventing them would put wrong values in the one file support trusts;
// carrying them needs new api surface on both platforms to inject them.
func addExportMetadata(manifestJson string, redact bool, sources []manifestSourceAvailability) (string, error) {
	manifest := map[string]any{}
	if err := json.Unmarshal([]byte(manifestJson), &manifest); err != nil {
		return "", err
	}
	if redact {
		manifest["export_mode"] = exportModeRedacted
	} else {
		manifest["export_mode"] = exportModeRaw
	}
	// the exporting process's own log configuration. export_log_root is the
	// root this process read the archive's files from, which is NOT
	// necessarily the one the manifest body names: on ios the body is built in
	// the extension and the archive is assembled in the app, from a different
	// container. export_log_verbosity says what the app's own lines were
	// written at.
	manifest["export_log_root"] = GetLogRoot()
	manifest["export_log_verbosity"] = GetLogVerbosity()
	if sources == nil {
		sources = []manifestSourceAvailability{}
	}
	manifest["sources"] = sources
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// fallbackDiagnosticManifestJson is the manifest written when no platform
// supplied one, or when the one supplied could not be parsed.
func fallbackDiagnosticManifestJson() string {
	return buildDiagnosticManifestJson(diagnosticManifestInput{
		SdkVersion:      Version,
		DeviceAvailable: false,
	})
}

// diagnosticManifestInput is the plain-Go input to the manifest. It is not
// exported to gomobile: the bound surface is DiagnosticManifestJson() string.
type diagnosticManifestInput struct {
	SdkVersion      string
	ClientId        string
	InstanceId      string
	NetworkSpace    string
	ConnectEnabled  bool
	ProvideEnabled  bool
	DeviceAvailable bool
}

func buildDiagnosticManifestJson(input diagnosticManifestInput) string {
	manifest := map[string]any{
		"sdk_version":   input.SdkVersion,
		"client_id":     input.ClientId,
		"instance_id":   input.InstanceId,
		"network_space": input.NetworkSpace,
		// device_available is false when the manifest was built without a live
		// device -- on ios that means the rpc into the extension was down, so
		// the fields below are absent rather than genuinely false.
		"device_available": input.DeviceAvailable,
		"connect_enabled":  input.ConnectEnabled,
		"provide_enabled":  input.ProvideEnabled,
		// the log fields are prefixed manifest_ because they describe THIS
		// process -- the one building the manifest -- and nothing else. On ios
		// that is the extension, reached over the rpc, while the archive is
		// assembled in the app from a different container: a plain "log_root"
		// here named the extension's root above an archive of the app's files,
		// and a support engineer following it opened the wrong container. The
		// path the archive's files came from is in sources[].log_root, per
		// source.
		"manifest_log_root":      GetLogRoot(),
		"manifest_log_dir":       GetLogDir(),
		"manifest_log_verbosity": GetLogVerbosity(),
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return "{\"device_available\":false}"
	}
	return string(encoded)
}
