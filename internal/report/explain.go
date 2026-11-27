package report

import (
	"fmt"
	"strings"
	"time"
)

// ExplainAll sets Explanation on every rec. Call it after MarkHPA. windowed: usage is p95 CPU / peak memory over window
// (stored samples); otherwise it is the peak of the live samples.
func ExplainAll(recs []Rec, windowed bool, window time.Duration) {
	for i := range recs {
		recs[i].Explanation = Explain(recs[i], windowed, window)
	}
}

// Explain says in one sentence how r's recommended values came out. It is deterministic and uses only what Build knows.
func Explain(r Rec, windowed bool, window time.Duration) string {
	basis := "Sized on the peak CPU and memory of the live samples"
	if windowed {
		basis = "Sized on p95 CPU and peak memory over " + spanText(window)
	}
	parts := []string{fmt.Sprintf("%s, plus %.0f%% CPU and %.0f%% memory headroom", basis, (cpuHeadroom-1)*100, (memHeadroom-1)*100)}
	// Build never raises a request below the minimum (S36): a value at the minimum is a cut, or an unset request.
	atMin := func(rec, min, req int64) (held, set bool) { return rec == min && req > min, rec == min && req == 0 }
	cpuHeld, cpuSet := atMin(r.Recommended.MilliCPU, minMilliCPU, r.Request.MilliCPU)
	memHeld, memSet := atMin(r.Recommended.MemBytes, minMemBytes, r.Request.MemBytes)
	if cpuHeld || memHeld {
		parts = append(parts, fmt.Sprintf("a value below the minimum size (%dm CPU, %dMi memory) is held at it, not sized lower", minMilliCPU, minMemBytes>>20))
	}
	if cpuSet || memSet {
		parts = append(parts, fmt.Sprintf("an unset request is set to the minimum size (%dm CPU, %dMi memory)", minMilliCPU, minMemBytes>>20))
	}
	if r.OOMKilled {
		parts = append(parts, "memory is held above the last OOM kill")
	}
	if r.Throttled {
		parts = append(parts, fmt.Sprintf("CPU usage is within %.0f%% of its limit, so the container is likely throttled and the limit must rise too", (1-throttleAt)*100))
	}
	if h := r.HPA; h != nil {
		parts = append(parts, "HPA "+h.Name+" scales this workload, so a request change can move its replica count")
	}
	return strings.Join(parts, "; ") + "."
}

// spanText prints a window as people read it: whole hours under 2 days, else whole days ("7 days", never "166h0m0s").
func spanText(d time.Duration) string {
	if d < 48*time.Hour {
		h := d.Round(time.Hour) / time.Hour
		if h <= 1 { // a window under 90 minutes reads "1 hour", never "0 hours"
			return "1 hour"
		}
		return fmt.Sprintf("%d hours", h)
	}
	return fmt.Sprintf("%d days", d.Round(24*time.Hour)/(24*time.Hour))
}
