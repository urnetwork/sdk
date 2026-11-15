//go:build !ios

package sdk

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

type mobileH1ProfileWindows struct {
	Quality, Speed int
}

type mobileH1ProfileReadiness struct {
	Healthy, Present mobileH1ProfileWindows
	Clients          int64
	Duplicate        bool
	StableMillis     int64
}

func mobileH1ProfileRequiredWindows(target ByteCount) mobileH1ProfileWindows {
	settings := connect.DefaultMultiClientSettings()
	applyMobileLowMemoryMultiClientSettingsForPlatform(settings, target, true)
	return mobileH1ProfileWindows{
		Quality: settings.WindowSizes[connect.WindowTypeQuality].WindowSizeMin,
		Speed:   settings.WindowSizes[connect.WindowTypeSpeed].WindowSizeMin,
	}
}

func mobileH1ProfileReadinessFromExits(exits []*connect.ExitInfo, clients int64) mobileH1ProfileReadiness {
	out := mobileH1ProfileReadiness{Clients: clients}
	seen := make(map[connect.Id]bool, len(exits))
	for _, exit := range exits {
		if exit == nil {
			out.Duplicate = true // malformed observations cannot satisfy readiness
			continue
		}
		if seen[exit.ClientId] {
			out.Duplicate = true
		}
		seen[exit.ClientId] = true
		var present, healthy *int
		switch exit.WindowType {
		case connect.WindowTypeQuality:
			present, healthy = &out.Present.Quality, &out.Healthy.Quality
		case connect.WindowTypeSpeed:
			present, healthy = &out.Present.Speed, &out.Healthy.Speed
		default:
			out.Duplicate = true
			continue
		}
		*present++
		if !exit.Done && !exit.Warning && !exit.Quarantined {
			*healthy++
		}
	}
	return out
}

func mobileH1ProfileReady(required mobileH1ProfileWindows, observed mobileH1ProfileReadiness) bool {
	return 0 < required.Quality && 0 < required.Speed && !observed.Duplicate &&
		observed.Healthy == required && observed.Present == required &&
		observed.Clients == int64(required.Quality+required.Speed)
}

// Only this gate invokes measured traffic. A timeout carries the last observed
// counts and never permits a partial or briefly-ready topology to run.
func runMobileH1ProfileWhenReady(
	ctx context.Context,
	required mobileH1ProfileWindows,
	stableFor time.Duration,
	read func() mobileH1ProfileReadiness,
	now func() time.Time,
	wait func(context.Context) error,
	run func(mobileH1ProfileReadiness) error,
) (mobileH1ProfileReadiness, error) {
	var since time.Time
	var observed mobileH1ProfileReadiness
	for {
		if err := ctx.Err(); err != nil {
			return observed, fmt.Errorf("profile readiness canceled before measured traffic: %w", err)
		}
		observed = read()
		if mobileH1ProfileReady(required, observed) {
			if since.IsZero() {
				since = now()
			}
			observed.StableMillis = now().Sub(since).Milliseconds()
			if now().Sub(since) >= stableFor {
				return observed, run(observed)
			}
		} else {
			since = time.Time{}
		}
		if err := wait(ctx); err != nil {
			return observed, fmt.Errorf("profile windows never became stable: required=%+v last=%+v: %w", required, observed, err)
		}
	}
}

func TestMobileH1ProfileReadinessRequiresBothScaledWindows(t *testing.T) {
	for _, tc := range []struct {
		target ByteCount
		want   mobileH1ProfileWindows
	}{{20, mobileH1ProfileWindows{4, 1}}, {28, mobileH1ProfileWindows{4, 1}},
		{32, mobileH1ProfileWindows{5, 1}}, {64, mobileH1ProfileWindows{10, 2}}} {
		t.Run(fmt.Sprint(tc.target), func(t *testing.T) {
			required := mobileH1ProfileRequiredWindows(tc.target << 20)
			if required != tc.want {
				t.Fatalf("configured window minima = %+v, want %+v", required, tc.want)
			}
			ready := mobileH1ProfileReadiness{Healthy: required, Present: required,
				Clients: int64(required.Quality + required.Speed)}
			if !mobileH1ProfileReady(required, ready) {
				t.Fatal("fully formed profile was not ready")
			}
			for _, field := range []string{"quality", "speed", "warning", "forming", "duplicate", "census"} {
				partial := ready
				switch field {
				case "quality":
					partial.Healthy.Quality--
					partial.Present.Quality--
				case "speed":
					partial.Healthy.Speed--
					partial.Present.Speed--
				case "warning":
					partial.Healthy.Quality--
				case "forming":
					partial.Present.Quality++
				case "duplicate":
					partial.Duplicate = true
				case "census":
					partial.Clients++
				}
				if mobileH1ProfileReady(required, partial) {
					t.Errorf("%s observation allowed measured traffic: %+v", field, partial)
				}
			}
		})
	}
}

func TestMobileH1ProfileReadinessCountsHealthyExitsPerWindow(t *testing.T) {
	exits := []*connect.ExitInfo{
		{ClientId: connect.NewId(), WindowType: connect.WindowTypeQuality},
		{ClientId: connect.NewId(), WindowType: connect.WindowTypeQuality, Warning: true},
		{ClientId: connect.NewId(), WindowType: connect.WindowTypeQuality, Quarantined: true},
		{ClientId: connect.NewId(), WindowType: connect.WindowTypeSpeed},
		{ClientId: connect.NewId(), WindowType: connect.WindowTypeSpeed, Done: true},
	}
	got := mobileH1ProfileReadinessFromExits(exits, 5)
	if got.Healthy != (mobileH1ProfileWindows{1, 1}) || got.Present != (mobileH1ProfileWindows{3, 2}) || got.Duplicate {
		t.Fatalf("unhealthy exits counted as ready: %+v", got)
	}
	if !mobileH1ProfileReadinessFromExits(append(exits, exits[0]), 6).Duplicate ||
		!mobileH1ProfileReadinessFromExits(append(exits, nil), 6).Duplicate {
		t.Fatal("duplicate or absent exit observation was accepted")
	}
}

func TestMobileH1ProfileMeasuredTrafficWaitsForStableTopology(t *testing.T) {
	required := mobileH1ProfileWindows{10, 2}
	complete := mobileH1ProfileReadiness{Healthy: required, Present: required, Clients: 12}
	partial := mobileH1ProfileReadiness{Healthy: mobileH1ProfileWindows{4, 1}, Present: mobileH1ProfileWindows{4, 1}, Clients: 5}
	for _, timeout := range []bool{false, true} {
		t.Run(fmt.Sprint(timeout), func(t *testing.T) {
			now := time.Unix(1, 0)
			poll, calls := 0, 0
			_, err := runMobileH1ProfileWhenReady(context.Background(), required, time.Second,
				func() mobileH1ProfileReadiness {
					// A 0.9-second ready interval must not allow traffic; a later
					// incomplete observation resets the entire stability hold.
					if timeout || poll < 2 || poll == 11 {
						return partial
					}
					return complete
				}, func() time.Time { return now }, func(context.Context) error {
					poll++
					now = now.Add(100 * time.Millisecond)
					if poll == 30 {
						return context.DeadlineExceeded
					}
					return nil
				}, func(ready mobileH1ProfileReadiness) error {
					calls++
					if poll != 22 || ready.StableMillis != 1000 {
						t.Errorf("traffic ran before a complete stability hold: poll=%d ready=%+v", poll, ready)
					}
					return nil
				})
			if timeout && (err == nil || calls != 0) || !timeout && (err != nil || calls != 1) {
				t.Fatalf("timeout=%t measured calls=%d error=%v", timeout, calls, err)
			}
		})
	}
}

func TestMobileH1ProfileCanceledReadinessNeverStartsTraffic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	required := mobileH1ProfileWindows{4, 1}
	_, err := runMobileH1ProfileWhenReady(ctx, required, 0,
		func() mobileH1ProfileReadiness {
			return mobileH1ProfileReadiness{Healthy: required, Present: required, Clients: 5}
		}, time.Now, func(context.Context) error { return nil },
		func(mobileH1ProfileReadiness) error { t.Fatal("canceled gate ran traffic"); return nil })
	if err == nil {
		t.Fatal("canceled readiness returned no failure")
	}
}
