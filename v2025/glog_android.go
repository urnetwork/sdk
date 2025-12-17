//go:build android
// +build android

package sdk

func defaultLogDir(_ string) string {
	return "/data/data/com.bringyour.network/files"
}
