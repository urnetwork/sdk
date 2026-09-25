//go:build js

package main

import (
	"syscall/js"

	"github.com/urnetwork/sdk"
)

// jsLicenses projects sdk.LicenseInfoList as an array of plain objects
// (types.ts LicenseInfo)
func jsLicenses(licenses *sdk.LicenseInfoList) js.Value {
	out := []any{}
	if licenses != nil {
		for i := 0; i < licenses.Len(); i += 1 {
			license := licenses.Get(i)
			out = append(out, map[string]any{
				"name":      license.Name,
				"version":   license.Version,
				"kind":      license.Kind,
				"origin":    license.Origin,
				"url":       license.Url,
				"spdx":      license.Spdx,
				"copyright": license.Copyright,
				"notice":    license.Notice,
				"text":      license.Text,
			})
		}
	}
	return js.ValueOf(out)
}

func jsGetLicenses(this js.Value, args []js.Value) any {
	return jsLicenses(sdk.GetLicenses(stringArg(args, 0)))
}
