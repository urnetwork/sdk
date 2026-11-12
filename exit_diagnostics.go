package sdk

// The exit diagnostics file: why the app's earlier processes ended, one line
// per record, in a process's log directory beside its glog files.
//
// "The app closes by itself" (support 8190) can be a crash, a hang the system
// ended, a memory or cpu limit, or the user, and only the system knows which.
// On Apple platforms the app process writes what MetricKit reports of it here
// (exit_diagnostics_metrickit.go). The file is not a glog file, so glog's
// retention (clearOldLogs) never removes it; it keeps itself small instead,
// its newest whole lines within exitDiagnosticsMaxByteCount.
//
// It leaves the device only inside "send feedback with logs", which carries it
// beside the glog files of the process directory it sits in
// (appendLogFilesIn), planned before them so that newer logs never crowd it
// out of the upload's cap (selectUploadLogFiles), and in the diagnostic export
// the user makes. On ios the app writes it into Logs/app of the app group
// container, so the extension's upload, made from the same root while the
// tunnel is up, carries it as well as the app's own.
//
// This part is in every build: the extension writes no such file, but its
// upload carries the app's.

// The file's name in a process's log directory. It is not a glog name, so
// retention never takes it for a log file; the inventory lists it by this
// name.
const exitDiagnosticsFileName = "exit-diagnostics.txt"

// The file keeps its newest whole lines within this many bytes, two hundred
// lines or more.
const exitDiagnosticsMaxByteCount = 64 * 1024

// The severity the inventory gives the file, so that a picker that labels
// files by severity has one to show: its lines are informational.
const exitDiagnosticsSeverity = "INFO"

// Whether a log file is a process's exit diagnostics file.
func isExitDiagnosticsLogFile(logFileInfo *LogFileInfo) bool {
	return logFileInfo.Name == exitDiagnosticsFileName
}
