package sdk

import "testing"

func TestAndroidUsesIosExtensionGCPacing(t *testing.T) {
	if got, want := gcPercentForPlatform("android"), gcPercentForPlatform("ios"); got != want {
		t.Fatalf("Android GOGC = %d, iOS GOGC = %d", got, want)
	}
	if got := gcPercentForPlatform("ios"); got != 25 {
		t.Fatalf("mobile GOGC = %d, want 25", got)
	}
	if got := gcPercentForPlatform("darwin"); got != 100 {
		t.Fatalf("desktop GOGC = %d, want 100", got)
	}
	if got := gcPercentForPlatform("linux"); got != 100 {
		t.Fatalf("server GOGC = %d, want 100", got)
	}
}
