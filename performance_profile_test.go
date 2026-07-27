package sdk

import (
	"testing"

	"github.com/urnetwork/connect"
)

// TestToConnectPerformanceProfileWindowType pins the window type mapping:
// quality and speed fix a window; auto and unset map to the connect auto
// window type (the same as no profile), not a silently fixed quality window.
func TestToConnectPerformanceProfileWindowType(t *testing.T) {
	if toConnectPerformanceProfile(nil) != nil {
		t.Fatalf("nil profile must map to nil")
	}

	quality := toConnectPerformanceProfile(&PerformanceProfile{WindowType: WindowTypeQuality})
	if quality.WindowType != connect.WindowTypeQuality {
		t.Fatalf("quality window type = %v", quality.WindowType)
	}
	if _, _, ok := quality.FixedWindow(); !ok {
		t.Fatalf("quality profile must fix a window")
	}

	speed := toConnectPerformanceProfile(&PerformanceProfile{WindowType: WindowTypeSpeed})
	if speed.WindowType != connect.WindowTypeSpeed {
		t.Fatalf("speed window type = %v", speed.WindowType)
	}

	auto := toConnectPerformanceProfile(&PerformanceProfile{
		WindowType:  WindowTypeAuto,
		AllowDirect: true,
	})
	if auto.WindowType != connect.WindowTypeAuto {
		t.Fatalf("auto window type = %v", auto.WindowType)
	}
	if _, _, ok := auto.FixedWindow(); ok {
		t.Fatalf("auto profile must not fix a window")
	}
	// the orthogonal settings carry through under auto
	if !auto.AllowDirect {
		t.Fatalf("auto profile must carry allow direct")
	}
	pqe := toConnectPerformanceProfile(&PerformanceProfile{
		WindowType:            WindowTypeAuto,
		PostQuantumEncryption: true,
	})
	if !pqe.PostQuantumEncryption {
		t.Fatalf("auto profile must carry post quantum encryption")
	}

	// unset means auto, not quality
	unset := toConnectPerformanceProfile(&PerformanceProfile{})
	if unset.WindowType != connect.WindowTypeAuto {
		t.Fatalf("unset window type = %v", unset.WindowType)
	}
}

// TestPerformanceProfilesEqualAuto verifies the change detection around auto
// profiles: replacing a nil profile with an auto profile is a change (the
// orthogonal settings may differ), and identical auto profiles are equal.
func TestPerformanceProfilesEqualAuto(t *testing.T) {
	autoProfile := &PerformanceProfile{WindowType: WindowTypeAuto}

	if !performanceProfilesEqual(autoProfile, &PerformanceProfile{WindowType: WindowTypeAuto}) {
		t.Fatalf("identical auto profiles must be equal")
	}
	if performanceProfilesEqual(autoProfile, nil) {
		t.Fatalf("auto profile and nil are different values")
	}
	if performanceProfilesEqual(autoProfile, &PerformanceProfile{WindowType: WindowTypeQuality}) {
		t.Fatalf("auto and quality profiles must differ")
	}
	if performanceProfilesEqual(
		autoProfile,
		&PerformanceProfile{WindowType: WindowTypeAuto, AllowDirect: true},
	) {
		t.Fatalf("allow direct must be part of profile equality")
	}
	if performanceProfilesEqual(
		autoProfile,
		&PerformanceProfile{WindowType: WindowTypeAuto, PostQuantumEncryption: true},
	) {
		t.Fatalf("post quantum encryption must be part of profile equality")
	}
}
