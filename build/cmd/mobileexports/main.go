// Command mobileexports rejects unexpected omissions from generated gomobile
// source. Gobind records unsupported declarations only as `// skipped` lines,
// so every Go-only surface must be an explicit policy decision here.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var mobileLifecycleJoinIds = map[string]bool{
	"Api.CloseAndWait":             true,
	"AsyncLocalState.CloseAndWait": true,
	"DeviceLocal.CloseAndWait":     true,
}

// Api-only controllers exist for signed-in hosts before their device plane
// attaches. Native apps open the device-owned controllers instead, so these
// context and Api parameters are deliberately outside the gomobile surface.
// Keep both gobind records explicit: it emits one on the return type and one
// on the package facade for each unsupported constructor.
var mobileApiOnlyControllerIds = map[string]bool{
	"AccountPreferencesViewController.NewAccountPreferencesViewControllerWithApi": true,
	"DevicesViewController.NewDevicesViewControllerWithApi":                       true,
	"FeedbackViewController.NewFeedbackViewControllerWithApi":                     true,
	"LocationsViewController.NewLocationsViewControllerWithApi":                   true,
	"NetworkUserViewController.NewNetworkUserViewControllerWithApi":               true,
	"ReferralCodeViewController.NewReferralCodeViewControllerWithApi":             true,
	"PointsLeaderboardViewController.NewPointsLeaderboardViewControllerWithApi":   true,
	"NewAccountPreferencesViewControllerWithApi":                                  true,
	"NewDevicesViewControllerWithApi":                                             true,
	"NewFeedbackViewControllerWithApi":                                            true,
	"NewLocationsViewControllerWithApi":                                           true,
	"NewNetworkUserViewControllerWithApi":                                         true,
	"NewReferralCodeViewControllerWithApi":                                        true,
	"NewPointsLeaderboardViewControllerWithApi":                                   true,
}

// The exact lifecycle joins take context.Context and are for Go owners. Mobile
// callbacks retain the non-joining Close methods so they cannot self-join.
func allowedMobileOmission(identifier string) bool {
	if mobileLifecycleJoinIds[identifier] || mobileApiOnlyControllerIds[identifier] {
		return true
	}
	parts := strings.SplitN(identifier, ".", 2)
	typeName := parts[0]
	memberName := ""
	if len(parts) == 2 {
		memberName = parts[1]
	}

	if typeName == "DeviceLocalRpc" || typeName == "DeviceProviderIdentities" ||
		strings.HasPrefix(typeName, "DeviceRemote") ||
		strings.HasSuffix(typeName, "Rpc") || strings.Contains(typeName, "DeviceRpcDialer") ||
		strings.Contains(typeName, "DeviceRpcListener") {
		return true
	}
	if strings.Contains(identifier, "NewPlatformDeviceLocal") ||
		strings.Contains(identifier, "NewPlatformNetworkSpace") ||
		strings.Contains(identifier, "Proxy") {
		return true
	}
	if typeName == "DeviceLocal" {
		if memberName == "AddReceivePacketCallback" || memberName == "AddReceivePacketsCallback" ||
			memberName == "SendPacketsNoCopy" || memberName == "Ctx" ||
			strings.HasPrefix(memberName, "SetClientSecurityPolicyGenerator") ||
			strings.HasPrefix(memberName, "SetProviderSecurityPolicyGenerator") {
			return true
		}
	}
	if typeName == "DeviceLocalSettings" || strings.Contains(identifier, "SetUpgradeMuxSettings") {
		return true
	}
	if identifier == "Testing_NewNetworkSpaceWithUrls" || identifier == "CollapseHostNames" ||
		strings.HasPrefix(identifier, "Sim") || strings.HasPrefix(identifier, "NewSim") {
		return true
	}
	if strings.Contains(identifier, "SyncWithContext") || memberName == "NewApi" ||
		memberName == "NewNetworkSpaceWithUrls" || identifier == "NewApi" ||
		identifier == "NewNetworkSpaceWithUrls" {
		return true
	}
	if strings.HasPrefix(identifier, "SnPoolClaimResult") ||
		strings.HasPrefix(identifier, "SnPoolClaimArgs") ||
		strings.HasPrefix(identifier, "SnEpochResult") || identifier == "VerifyKeysResult.Keys" {
		return true
	}
	return false
}

// Parses the stable kind and qualified identifier emitted by gobind. A line
// with the marker but an unknown shape remains a candidate so validation fails.
func skippedIdentifier(line string) (string, bool) {
	const marker = "// skipped "
	markerIndex := strings.Index(line, marker)
	if markerIndex < 0 {
		return "", false
	}
	fields := strings.Fields(line[markerIndex+len(marker):])
	if len(fields) < 2 {
		return "", true
	}
	switch fields[0] {
	case "constructor", "field", "function", "method":
		return fields[1], true
	default:
		return "", true
	}
}

// Walks generated Java only; the directory also contains the binary source JAR.
func validateMobileExports(root string) error {
	unexpected := []string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".java" {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()

		relativePath, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		scanner := bufio.NewScanner(file)
		lineNumber := 0
		for scanner.Scan() {
			lineNumber++
			line := scanner.Text()
			identifier, ok := skippedIdentifier(line)
			if ok && !allowedMobileOmission(identifier) {
				unexpected = append(unexpected, fmt.Sprintf("%s:%d:%s", relativePath, lineNumber, line))
			}
		}
		return scanner.Err()
	})
	if err != nil {
		return err
	}
	if len(unexpected) == 0 {
		return nil
	}
	sort.Strings(unexpected)
	return errors.New(strings.Join(unexpected, "\n"))
}

// Reports every unexpected omission together and exits nonzero.
func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: mobileexports <generated-source-directory>")
		os.Exit(2)
	}
	if err := validateMobileExports(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "Some types could not be exported:")
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
