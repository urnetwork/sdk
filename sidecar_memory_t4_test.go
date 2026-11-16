//go:build !ios_extension && !js

package sdk

// §18.4's host budgets as assertions on T4's surrogate cells (SDK-M8-15; §22.9's Memory, size row; owner decisions 100,
// 215, 216; QD-620, QD-621): a cell's budget check fails a step that keeps past its budget, as a deliberate
// over-allocation shows, and passes one within it; and on the ios32 cell the forwarding compile's admission rule admits a
// 3,000-device team's compile and refuses a 5,000-device one with compile_over_budget at DESIGN-DETAILS 4.4's figures.
// The cells run on this host under the profiles' limits; the surrogate devices' runs are external (owner decision 100).

import (
	"strings"
	"testing"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest/t4cell"
)

// A budget assertion fails a step that keeps past its budget, by name, and passes one within it, so a cell's check is
// not vacuous.
func TestT4BudgetCatchesADeliberateOverAllocation(t *testing.T) {
	cell := t4cell.NewCell(t4cell.Ios32Profile())
	const budget = 1024 * 1024
	if _, err := cell.Measure("within", budget, t4cell.HoldsSteady, func() (any, error) {
		return make([]byte, 64*1024), nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := cell.CheckWithin(); err != nil {
		t.Fatalf("a step within its budget: %v", err)
	}
	row, err := cell.Measure("deliberate over-allocation", budget, t4cell.HoldsSteady, func() (any, error) {
		kept := make([]byte, 4*budget)
		for i := range kept {
			kept[i] = byte(i)
		}
		return kept, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if row.Within() || row.SteadyByteCount < 3*budget {
		t.Fatalf("the over-allocation measured %+v", row)
	}
	failure := cell.CheckWithin()
	if failure == nil || !strings.Contains(failure.Error(), "deliberate over-allocation") || strings.Contains(failure.Error(), "within:") {
		t.Fatalf("the check: %v", failure)
	}
	// a peak budget holds what a step held at once, even if it let it go
	peakCell := t4cell.NewCell(t4cell.Ios32Profile())
	peakRow, _ := peakCell.Measure("transient", budget, t4cell.HoldsPeak, func() (any, error) {
		transient := make([]byte, 4*budget)
		for i := range transient {
			transient[i] = byte(i)
		}
		return nil, nil
	})
	if peakRow.PeakByteCount < 3*budget || peakCell.CheckWithin() == nil {
		t.Errorf("a transient over-allocation's peak: %+v", peakRow)
	}
	t.Log("\n" + cell.Table() + peakCell.Table())
}

// DESIGN-DETAILS 4.4's figures for a team's compile input on an app device: a row of about 440 B a device (2.2 MB at
// 5,000, QD-622), and a config head of about 80 B a device's names (4.2.4's estimate). A test's literals.
const (
	testT4RowsByteCountPerDevice = 2_200_000 / 5_000
	testT4HeadByteCountPerDevice = 80
)

// On the ios32 cell the forwarding compile's admission rule (QD-620's 10 MiB cap under the 30 MiB ceiling) admits a
// 3,000-device team's compile at the tunnel's connected heap and refuses a 5,000-device one with compile_over_budget,
// whatever the heap, so the last verified state stays and nothing goes pending (QD-621; the Forwarder's own tests hold
// that). The cell's figure is the rule's estimate at 4.4's sizes; the measured compile peak is the review phase's.
func TestForwardingCompileAdmissionOnTheIos32Cell(t *testing.T) {
	profile := t4cell.Ios32Profile()
	settings := sidecar.DefaultForwarderSettings(sidecar.ForwarderHostApple)
	if settings.CompileMemoryMaxByteCount != profile.CompileMemoryMaxByteCount {
		t.Fatalf("the Apple class's cap %d is not the ios32 cell's %d", settings.CompileMemoryMaxByteCount, profile.CompileMemoryMaxByteCount)
	}
	sizeOf := func(deviceCount int64) sidecar.ForwarderCompileSize {
		return sidecar.ForwarderCompileSize{
			RowsByteCount: deviceCount * testT4RowsByteCountPerDevice,
			HeadByteCount: deviceCount * testT4HeadByteCountPerDevice,
		}
	}
	// the tunnel connected: 19.6 MiB of its 28 MiB target (question ios-leg-head-reader-size-over-budget's figure)
	const connectedHeapByteCount = 19_600 * 1024
	if admission := settings.CompileAdmission(sizeOf(3_000), connectedHeapByteCount); admission != sidecar.ForwarderCompileAdmitted {
		t.Errorf("a 3,000-device team: %v, estimate %d of %d", admission, settings.CompileEstimate(sizeOf(3_000)), settings.CompileMemoryMaxByteCount)
	}
	if admission := settings.CompileAdmission(sizeOf(5_000), 0); admission != sidecar.ForwarderCompileOverBudget {
		t.Errorf("a 5,000-device team: %v, estimate %d of %d", admission, settings.CompileEstimate(sizeOf(5_000)), settings.CompileMemoryMaxByteCount)
	}
	if sidecar.ForwarderAlarmCompileOverBudget != "compile_over_budget" {
		t.Errorf("the refusal's alarm reads %q", sidecar.ForwarderAlarmCompileOverBudget)
	}
	// on the Android cell the 5,000-device team compiles: its cap is 16 MiB, with no ceiling
	android := sidecar.DefaultForwarderSettings(sidecar.ForwarderHostAndroid)
	if android.CompileMemoryMaxByteCount != t4cell.AndroidProfile().CompileMemoryMaxByteCount ||
		android.CompileAdmission(sizeOf(5_000), connectedHeapByteCount) != sidecar.ForwarderCompileAdmitted {
		t.Errorf("Android's 5,000-device compile: estimate %d of %d", android.CompileEstimate(sizeOf(5_000)), android.CompileMemoryMaxByteCount)
	}
	t.Logf("ios32 estimates: 3,000 devices %d B, 5,000 devices %d B, cap %d B", settings.CompileEstimate(sizeOf(3_000)),
		settings.CompileEstimate(sizeOf(5_000)), settings.CompileMemoryMaxByteCount)
}
