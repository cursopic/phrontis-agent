package report

import (
	"math"
	"testing"
	"time"
)

func apod(wl string, cs ...Container) Pod {
	return Pod{Namespace: "ns", Name: wl + "-1", Workload: wl, Node: "a", Containers: cs}
}

func ucont(name string, milli int64, has bool) Container {
	c := Container{Name: name, Request: Resources{100, 64 << 20}}
	if has {
		c.HasUsage, c.Usage = true, Resources{milli, 1 << 20}
	}
	return c
}

// fold runs n samples (5 min apart) through a Window; pods(i) gives sample i's pods.
func fold(n int, pods func(i int) []Pod) []Pod {
	return foldAt(n, func(i int) time.Time { return t0.Add(time.Duration(i) * 5 * time.Minute) }, pods)
}

var t0 = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

func foldAt(n int, at func(i int) time.Time, pods func(i int) []Pod) []Pod {
	var w Window
	for i := 0; i < n; i++ {
		w.Add(at(i), pods(i))
	}
	return w.Pods()
}

var lines = []Line{{Namespace: "ns", Workload: "Deployment/idle", Monthly: 7}, {Namespace: "ns", Workload: "Deployment/busy", Monthly: 9}}

func TestAbandonedIdleListedBusyAndNoDataNot(t *testing.T) {
	pods := fold(300, func(i int) []Pod { // 25 h
		return []Pod{apod("Deployment/idle", ucont("c", int64(i%3), true)), apod("Deployment/busy", ucont("c", 50, true)),
			apod("Deployment/nodata", ucont("c", 0, false))}
	})
	a := BuildAbandoned(pods, lines)
	if a == nil || len(a.Workloads) != 1 || a.Workloads[0].Workload != "Deployment/idle" {
		t.Fatalf("only the idle workload is listed: %+v", a)
	}
	if l := a.Workloads[0]; l.PeakCPU != 2 || l.Monthly != 7 || !l.Short || !a.Short || l.SpanHours < 24.9 || l.SpanHours > 25.1 {
		t.Errorf("line %+v", l)
	}
}

func TestAbandonedNeedsOwnHistory(t *testing.T) {
	// "young" appears only in the last 3 of 300 samples: a short run of zeros is no evidence.
	pods := fold(300, func(i int) []Pod {
		ps := []Pod{apod("Deployment/idle", ucont("c", 0, true))}
		if i >= 297 {
			ps = append(ps, apod("Deployment/young", ucont("c", 0, true)))
		}
		return ps
	})
	a := BuildAbandoned(pods, append(lines, Line{Namespace: "ns", Workload: "Deployment/young", Monthly: 1}))
	if a == nil || len(a.Workloads) != 1 || a.Workloads[0].Workload != "Deployment/idle" {
		t.Fatalf("young must not be listed: %+v", a)
	}
	// Enough samples but under 24 h of span.
	pods = fold(200, func(int) []Pod { return []Pod{apod("Deployment/idle", ucont("c", 0, true))} })
	if BuildAbandoned(pods, lines) != nil {
		t.Error("16 h of history is too short")
	}
}

func TestAbandonedSingleSampleListsNothing(t *testing.T) {
	pods := fold(1, func(int) []Pod { return []Pod{apod("Deployment/idle", ucont("c", 0, true))} })
	if BuildAbandoned(pods, lines) != nil {
		t.Error("one sample is no window")
	}
}

func TestAbandonedThresholds(t *testing.T) {
	// Mean exactly 5m (alternating 0 and 10 is peak 10, so use 5 flat: mean 5, peak 5) is not idle; mean 4 / peak 9 is.
	for _, c := range []struct {
		name string
		vals []int64 // repeated
		want bool
	}{
		{"mean 4 peak 9", []int64{4, 4, 4, 4, 4, 4, 4, 4, 4, 9}, true},
		{"mean exactly 5", []int64{5}, false},
		{"peak exactly 10", []int64{0, 10}, false},
		{"peak 9 mean under 5", []int64{0, 9}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			pods := fold(300, func(i int) []Pod {
				return []Pod{apod("Deployment/idle", ucont("c", c.vals[i%len(c.vals)], true))}
			})
			if got := BuildAbandoned(pods, lines) != nil; got != c.want {
				t.Errorf("listed=%v want %v", got, c.want)
			}
		})
	}
}

func TestAbandonedOneContainerWithoutData(t *testing.T) {
	pods := fold(300, func(int) []Pod {
		return []Pod{apod("Deployment/idle", ucont("app", 0, true), ucont("sidecar", 0, false))}
	})
	if BuildAbandoned(pods, lines) != nil {
		t.Error("a container with no data blocks the workload")
	}
}

func TestAbandonedCountsRealSamples(t *testing.T) {
	// Samples 1-k and 295-300 span 25 h, but only k+6 samples back it: 12 pass, 11 do not.
	for _, c := range []struct {
		first int
		want  bool
	}{{6, true}, {5, false}} {
		pods := fold(300, func(i int) []Pod {
			if i >= c.first && i < 294 {
				return nil
			}
			return []Pod{apod("Deployment/idle", ucont("c", 0, true))}
		})
		if got := BuildAbandoned(pods, lines) != nil; got != c.want {
			t.Errorf("first run of %d samples: listed = %v, want %v", c.first, got, c.want)
		}
	}
}

// A workload's span is the time between its first and last sample, not its share of the sample numbers.
func TestAbandonedSpanIsRealTime(t *testing.T) {
	for _, c := range []struct {
		name string
		gap  time.Duration
		want float64 // span hours; 0: not listed
	}{{"gap inside its samples", 23 * time.Hour, 24 + 10./12}, {"no gap", 5 * time.Minute, 0}} {
		pods := foldAt(24, func(i int) time.Time {
			at := t0.Add(time.Duration(i) * 5 * time.Minute)
			if i >= 12 {
				at = at.Add(c.gap - 5*time.Minute)
			}
			return at
		}, func(int) []Pod { return []Pod{apod("Deployment/idle", ucont("c", 0, true))} })
		a := BuildAbandoned(pods, lines)
		if (a != nil) != (c.want > 0) || a != nil && math.Abs(a.Workloads[0].SpanHours-c.want) > 0.01 {
			t.Errorf("%s: %+v, want span %v", c.name, a, c.want)
		}
	}
	// The first 12 samples are 2 h apart and the other 240 are 5 min apart; the workload is only in those 240.
	pods := foldAt(252, func(i int) time.Time {
		if i < 12 {
			return t0.Add(time.Duration(i) * 2 * time.Hour)
		}
		return t0.Add(24*time.Hour + time.Duration(i-12)*5*time.Minute)
	}, func(i int) []Pod {
		if i < 12 {
			return nil
		}
		return []Pod{apod("Deployment/idle", ucont("c", 0, true))}
	})
	if a := BuildAbandoned(pods, lines); a != nil {
		t.Errorf("20 h of own samples in a 44 h window (by sample number 42 h): %+v, want none", a)
	}
}
