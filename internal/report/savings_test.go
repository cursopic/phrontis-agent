package report

import (
	"testing"
	"time"
)

// series gives one point per hour from `from`, hours long, all at cost c.
func series(from time.Time, hours int, c float64) (out []Point) {
	for i := 0; i < hours; i++ {
		out = append(out, Point{from.Add(time.Duration(i) * time.Hour), c})
	}
	return out
}

func TestRealizedSaving(t *testing.T) {
	at := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	before := series(at.Add(-48*time.Hour), 48, 100)
	for _, c := range []struct {
		name  string
		pts   []Point
		state string
		saved float64
	}{
		{"saved", append(append([]Point{}, before...), series(at.Add(2*time.Hour), 30, 60)...), "saved", 40},
		{"cost went up", append(append([]Point{}, before...), series(at.Add(2*time.Hour), 30, 130)...), "increase", -30},
		{"unchanged", append(append([]Point{}, before...), series(at.Add(2*time.Hour), 30, 100)...), "flat", 0},
		{"after span under 24h", append(append([]Point{}, before...), series(at.Add(2*time.Hour), 20, 60)...), "pending", 0},
		{"nothing before", series(at.Add(2*time.Hour), 30, 60), "nobaseline", 0},
		// the settle hour (at .. at+1h) is a mix of old and new pods and is left out
		{"settle hour ignored", append(append(append([]Point{}, before...), Point{at.Add(30 * time.Minute), 999}), series(at.Add(time.Hour), 26, 60)...), "saved", 40},
	} {
		got := RealizedSaving(at, time.Time{}, c.pts)
		if got.Status != c.state || got.Saving != c.saved {
			t.Errorf("%s: got %+v, want %s %v", c.name, got, c.state, c.saved)
		}
	}
}

func TestRealizedSavingStopsAtNextChange(t *testing.T) {
	at := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	pts := append(append(series(at.Add(-48*time.Hour), 48, 100), series(at.Add(2*time.Hour), 30, 60)...), series(at.Add(40*time.Hour), 30, 200)...)
	if got := RealizedSaving(at, time.Time{}, pts); got.After == 60 {
		t.Fatalf("without a bound the later change leaks in: %+v", got)
	}
	if got := RealizedSaving(at, at.Add(36*time.Hour), pts); got.Status != "saved" || got.After != 60 {
		t.Errorf("bounded: %+v, want saved after 60", got)
	}
	for _, next := range []time.Duration{20 * time.Hour, time.Minute} { // too little data before the next change, or none
		if got := RealizedSaving(at, at.Add(next), pts); got != (Realized{Status: "superseded"}) {
			t.Errorf("next change after %s: %+v, want superseded", next, got)
		}
	}
	if got := RealizedSaving(at, at.Add(time.Minute), pts[48:]); got.Status != "nobaseline" {
		t.Errorf("no cost before: %+v, want nobaseline", got)
	}
}
