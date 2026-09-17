package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSubprotocolRpcInternalsStayOutsideMobileBindings(t *testing.T) {
	for _, language := range []string{"java", "objc"} {
		t.Run(language, func(t *testing.T) {
			outputDirectory := t.TempDir()
			args := []string{
				"run", "golang.org/x/mobile/cmd/gobind",
				"-lang=" + language,
				"-tags=sdk_mobile_bind",
				"-outdir=" + outputDirectory,
			}
			if language == "java" {
				args = append(args, "-javapkg=com.bringyour")
			}
			args = append(args, "github.com/urnetwork/sdk")
			command := exec.Command("go", args...)
			command.Dir = "../.."
			command.Env = append(
				os.Environ(),
				"GODEBUG=gotypesalias=0",
				"GOEXPERIMENT=greenteagc",
				"GOPROXY=off",
				"GOSUMDB=off",
				"GOWORK=off",
			)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("generate %s bindings: %v\n%s", language, err, output)
			}
			if language == "java" {
				if err := validateMobileExports(outputDirectory); err != nil {
					t.Fatalf("validate generated Java: %v", err)
				}
			}

			forbidden := []string{
				"DeviceSubprotocolRequest",
				"DeviceSubprotocolResponse",
				"GetPeerClientKeyPinStore",
				"RemoteSubprotocol",
			}
			err := filepath.WalkDir(outputDirectory, func(path string, entry os.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if entry.IsDir() {
					return nil
				}
				contents, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				for _, name := range forbidden {
					if strings.Contains(string(contents), name) {
						t.Errorf("%s binding leaked RPC implementation type %s in %s", language, name, path)
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

// The pin store is security-critical Go wiring, not an app API. Allowlisting
// its unsupported connect interface would silently omit the getter again.
func TestMobilePinStoreOmissionIsRejected(t *testing.T) {
	root := t.TempDir()
	source := "// skipped method LocalState.GetPeerClientKeyPinStore with unsupported parameter or return types\n"
	if err := os.WriteFile(filepath.Join(root, "LocalState.java"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateMobileExports(root); err == nil {
		t.Fatal("the internal peer client key pin store was accepted as a mobile omission")
	}
}

func TestMobileLifecycleJoinOmissionsAreExplicit(t *testing.T) {
	root := t.TempDir()
	source := strings.Join([]string{
		"// skipped method Api.CloseAndWait with unsupported parameter or return types",
		"// skipped method AsyncLocalState.CloseAndWait with unsupported parameter or return types",
		"// skipped method DeviceLocal.CloseAndWait with unsupported parameter or return types",
	}, "\n")
	if err := os.WriteFile(filepath.Join(root, "Lifecycle.java"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateMobileExports(root); err != nil {
		t.Fatalf("Go-only lifecycle joins were not accepted: %v", err)
	}
}

func TestMobileLifecycleJoinPolicyRejectsAdjacentApiLoss(t *testing.T) {
	root := t.TempDir()
	source := "// skipped method NetworkSpace.CloseAndWait with unsupported parameter or return types\n"
	if err := os.WriteFile(filepath.Join(root, "NetworkSpace.java"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	err := validateMobileExports(root)
	if err == nil {
		t.Fatal("an unreviewed lifecycle omission was accepted")
	}
	if !strings.Contains(err.Error(), "NetworkSpace.CloseAndWait") {
		t.Fatalf("unexpected omission was not identified: %v", err)
	}
}

func TestMobileLifecycleJoinPolicyDoesNotPrefixMatch(t *testing.T) {
	root := t.TempDir()
	source := "// skipped method DeviceLocal.CloseAndWaitForLeak with unsupported parameter or return types\n"
	if err := os.WriteFile(filepath.Join(root, "DeviceLocal.java"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateMobileExports(root); err == nil {
		t.Fatal("a similarly named but unreviewed omission was accepted")
	}
}

func TestMobileApiOnlyControllerOmissionsAreExplicit(t *testing.T) {
	root := t.TempDir()
	source := strings.Join([]string{
		"// skipped constructor AccountPreferencesViewController.NewAccountPreferencesViewControllerWithApi with unsupported parameter or return types",
		"// skipped constructor DevicesViewController.NewDevicesViewControllerWithApi with unsupported parameter or return types",
		"// skipped constructor FeedbackViewController.NewFeedbackViewControllerWithApi with unsupported parameter or return types",
		"// skipped constructor LocationsViewController.NewLocationsViewControllerWithApi with unsupported parameter or return types",
		"// skipped constructor NetworkUserViewController.NewNetworkUserViewControllerWithApi with unsupported parameter or return types",
		"// skipped constructor ReferralCodeViewController.NewReferralCodeViewControllerWithApi with unsupported parameter or return types",
		"// skipped function NewAccountPreferencesViewControllerWithApi with unsupported parameter or return types",
		"// skipped function NewDevicesViewControllerWithApi with unsupported parameter or return types",
		"// skipped function NewFeedbackViewControllerWithApi with unsupported parameter or return types",
		"// skipped function NewLocationsViewControllerWithApi with unsupported parameter or return types",
		"// skipped function NewNetworkUserViewControllerWithApi with unsupported parameter or return types",
		"// skipped function NewReferralCodeViewControllerWithApi with unsupported parameter or return types",
	}, "\n")
	if err := os.WriteFile(filepath.Join(root, "LocationsViewController.java"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateMobileExports(root); err != nil {
		t.Fatalf("browser-only api controllers were not accepted: %v", err)
	}
}

func TestMobileApiOnlyControllerPolicyDoesNotPrefixMatch(t *testing.T) {
	root := t.TempDir()
	source := "// skipped function NewLocationsViewControllerWithApiAndLeak with unsupported parameter or return types\n"
	if err := os.WriteFile(filepath.Join(root, "Sdk.java"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateMobileExports(root); err == nil {
		t.Fatal("a similarly named but unreviewed api omission was accepted")
	}
}

func TestMobileExportPolicyRejectsMalformedSkippedRecord(t *testing.T) {
	root := t.TempDir()
	source := "// skipped unexpectedly malformed output\n"
	if err := os.WriteFile(filepath.Join(root, "Broken.java"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateMobileExports(root); err == nil {
		t.Fatal("a malformed skipped record was ignored")
	}
}

func TestMobileExtenderStringSliceOmissionsAreExplicit(t *testing.T) {
	root := t.TempDir()
	source := strings.Join([]string{
		"// skipped function ExtenderHosts with unsupported parameter or return types",
		"// skipped function ExtenderRootPublicKeys with unsupported parameter or return types",
		"// skipped field NetworkSpaceValues.ExtenderHosts with unsupported type",
		"// skipped field NetworkSpaceValues.ExtenderRootPublicKeys with unsupported type",
	}, "\n")
	if err := os.WriteFile(filepath.Join(root, "NetworkSpaceValues.java"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateMobileExports(root); err != nil {
		t.Fatalf("the go-side extender string slices were not accepted: %v", err)
	}
}

func TestMobileExtenderStringSlicePolicyDoesNotPrefixMatch(t *testing.T) {
	root := t.TempDir()
	source := "// skipped field NetworkSpaceValues.ExtenderHostsAndMore with unsupported type\n"
	if err := os.WriteFile(filepath.Join(root, "NetworkSpaceValues.java"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateMobileExports(root); err == nil {
		t.Fatal("a similarly named but unreviewed extender omission was accepted")
	}
}

// These internal types are absent under sdk_mobile_bind. A skipped record is
// a source-boundary regression, not a permitted unsupported app declaration.
func TestMobileSubprotocolRpcOmissionsAreRejected(t *testing.T) {
	for _, source := range []string{
		"// skipped field DeviceSubprotocolRequest.ID with unsupported type",
		"// skipped field DeviceSubprotocolRequest.ClientID with unsupported type",
		"// skipped field DeviceSubprotocolRequest.InstanceID with unsupported type",
		"// skipped field DeviceSubprotocolRequest.Destination with unsupported type",
		"// skipped field DeviceSubprotocolResponse.Source with unsupported type",
		"// skipped field DeviceSubprotocolResponse.Protocols with unsupported type",
		"// skipped method RemoteSubprotocol.Query with unsupported parameter or return types",
		"// skipped method RemoteSubprotocol.Receive with unsupported parameter or return types",
		"// skipped method RemoteSubprotocol.Send with unsupported parameter or return types",
	} {
		t.Run(strings.Fields(source)[3], func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "Subprotocol.java"), []byte(source+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := validateMobileExports(root); err == nil {
				t.Fatal("an internal subprotocol declaration was accepted as a mobile omission")
			}
		})
	}
}

func TestMobileSubprotocolPolicyRejectsAdjacentApiLoss(t *testing.T) {
	for _, record := range []struct{ kind, identifier string }{
		{"field", "DeviceSubprotocolRequest.IDAndMore"},
		{"field", "DeviceSubprotocolResponse.ProtocolsAndMore"},
		{"method", "RemoteSubprotocol.SendAndMore"},
		{"field", "DeviceSubprotocolRequestExtra.ID"},
		{"field", "DeviceSubprotocolResponseExtra.Source"},
		{"method", "RemoteSubprotocolExtra.Receive"},
		{"field", "DeviceSubprotocolRequest.Data"},
		{"field", "DeviceSubprotocolResponse.Data"},
		{"method", "RemoteSubprotocol.Close"},
		{"method", "DeviceLocal.EnableSubprotocol"},
		{"method", "DeviceLocal.DisableSubprotocol"},
		{"method", "DeviceLocal.EnabledSubprotocols"},
		{"method", "DeviceLocal.SendSubprotocolBytes"},
		{"method", "DeviceLocal.QuerySubprotocols"},
		{"method", "DeviceLocal.SubprotocolReceivedCount"},
		{"method", "DeviceLocal.SubprotocolStats"},
		{"method", "SubprotocolListener.SubprotocolMessage"},
		{"method", "SubprotocolsQueryCallback.Result"},
		{"field", "SubprotocolStats.Sent"},
	} {
		t.Run(record.identifier, func(t *testing.T) {
			root := t.TempDir()
			source := "// skipped " + record.kind + " " + record.identifier + " with unsupported type\n"
			if err := os.WriteFile(filepath.Join(root, "Subprotocol.java"), []byte(source), 0o600); err != nil {
				t.Fatal(err)
			}
			err := validateMobileExports(root)
			if err == nil {
				t.Fatal("an unreviewed subprotocol omission was accepted")
			}
			if !strings.Contains(err.Error(), record.identifier) {
				t.Fatalf("unexpected omission was not identified: %v", err)
			}
		})
	}
}

func TestMobilePeerIdentityStorePolicyRejectsAdjacentApiLoss(t *testing.T) {
	for _, identifier := range []string{
		"LocalState.GetPeerClientKeyPinStoreAndMore",
		"LocalStateExtra.GetPeerClientKeyPinStore",
		"AsyncLocalState.GetPeerClientKeyPinStore",
		"LocalState.SetPeerClientKeyPinStore",
		"LocalState.GetDeviceLocalKeyMaterial",
		"LocalState.GetProvideSecretKeys",
	} {
		t.Run(identifier, func(t *testing.T) {
			root := t.TempDir()
			source := "// skipped method " + identifier + " with unsupported parameter or return types\n"
			if err := os.WriteFile(filepath.Join(root, "LocalState.java"), []byte(source), 0o600); err != nil {
				t.Fatal(err)
			}
			err := validateMobileExports(root)
			if err == nil {
				t.Fatal("an unreviewed local-state omission was accepted")
			}
			if !strings.Contains(err.Error(), identifier) {
				t.Fatalf("unexpected omission was not identified: %v", err)
			}
		})
	}
}

// Native dial signatures remain deliberate omissions on concrete Devices.
func TestMobileSocketNativeOmissionsAreExplicit(t *testing.T) {
	root := t.TempDir()
	lines := []string{}
	for _, typeName := range []string{"DeviceLocal", "DeviceRemote"} {
		for _, method := range []string{"Dial", "DialContext", "DialTls", "DialTlsContext"} {
			lines = append(lines, "// skipped method "+typeName+"."+method+" with unsupported parameter or return types")
		}
	}
	if err := os.WriteFile(filepath.Join(root, "NativeSocket.java"), []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateMobileExports(root); err != nil {
		t.Fatalf("native-only concrete dial methods were not accepted: %v", err)
	}
}

// Portable entry points and connection controls cannot inherit Go-only skips.
func TestMobileSocketPortableOmissionsAreRejected(t *testing.T) {
	for _, identifier := range []string{
		"Device.OpenSocket", "DeviceLocal.OpenSocket", "DeviceRemote.OpenSocket",
		"Socket.Read", "Socket.Write", "Socket.Close", "Socket.SetDeadlineMillis",
		"Socket.SetReadDeadlineMillis", "Socket.SetWriteDeadlineMillis",
		"Socket.GetLocalAddr", "Socket.GetRemoteAddr",
		"SocketRead.Data", "SocketRead.Eof", "SocketTLSOptions.SetNextProtos",
	} {
		root := t.TempDir()
		kind := "method"
		if strings.HasPrefix(identifier, "SocketRead.") {
			kind = "field"
		}
		line := "// skipped " + kind + " " + identifier + " with unsupported parameter or return types\n"
		if err := os.WriteFile(filepath.Join(root, "PortableSocket.java"), []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := validateMobileExports(root); err == nil {
			t.Errorf("portable socket omission %s was accepted", identifier)
		}
	}
}
