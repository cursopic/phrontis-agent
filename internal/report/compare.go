package report

import "math"

// Totals are the monthly figures of one saved snapshot (USD estimate; flat-priced nodes make it a rough one).
type Totals struct{ Compute, Idle, Storage, LoadBalancers float64 }

// Delta is one cost line before and after the switch. Pct is 0 when Before is 0.
type Delta struct {
	Name                string
	Before, After, Diff float64
	Pct                 float64
}

// Compare lines up two snapshots: compute, idle (part of compute), storage, load balancers, and the total of the three billed lines.
func Compare(before, after Totals) []Delta {
	line := func(n string, b, a float64) Delta {
		d := Delta{Name: n, Before: b, After: a, Diff: a - b}
		if b != 0 {
			d.Pct = 100 * d.Diff / math.Abs(b)
		}
		return d
	}
	return []Delta{
		line("Compute", before.Compute, after.Compute),
		line("  of which idle", before.Idle, after.Idle),
		line("Storage", before.Storage, after.Storage),
		line("Load balancers", before.LoadBalancers, after.LoadBalancers),
		line("Total", before.Compute+before.Storage+before.LoadBalancers, after.Compute+after.Storage+after.LoadBalancers),
	}
}
