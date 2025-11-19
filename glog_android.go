//go:build android
// +build android

package sdk

// On Android, use the app's files directory (usually set by the host app).
// By default, os.UserHomeDir() returns the app's sandbox root, but it's best practice
// to let the host app provide the exact path via a setter.
// For now, use "files/urnetwork" inside the home directory.
func defaultLogDir(home string) string {
	// // Try cwd first (often your app's sandbox), then a subdir.
	// if cwd, err := os.Getwd(); err == nil && cwd != "" {
	// 	return filepath.Join(cwd, "urnetwork_logs")
	// }
	// // Fallback to home if available and not /sdcard
	// if home, _ := os.UserHomeDir(); home != "" && home != "/sdcard" && home != "/" {
	// 	return filepath.Join(home, "files", "urnetwork_logs")
	// }
	// // Final fallback: no default → glog will still try TempDir, but this is unlikely if above succeeds.
	// return ""
	//
	return "/data/data/com.bringyour.network/files"
}
