package report

import (
	"cmp"
	"math"
	"slices"
	"strings"
	"time"
)

const (
	idleAvgMilli  = 5  // a workload is abandoned when every container averages under this many millicores...
	idlePeakMilli = 10 // ...and never reaches this many
	// AbandonedDays is the history the story asks for; a shorter window is used as is and shown (S27).
	AbandonedDays = 14
	// A workload needs this much history of its own before it can be called abandoned: a young one with a few zero samples is not.
	abandonedMinSamples = 12
	abandonedMinSpan    = 24 * time.Hour
)

// AbandonedLine is one workload that sat idle for the whole window. Monthly is what its pods cost (same figure as Report.Workloads).
type AbandonedLine struct {
	Namespace, Workload string
	Pods                int
	PeakCPU             int64   // highest CPU sample of any container, millicores
	SpanHours           float64 // how long its own samples reach, first to last
	Short               bool    // Span is shorter than AbandonedDays
	Monthly             float64
}

// Abandoned lists workloads with near-zero CPU; each line carries the span of its own samples, so a reader sees how much history backs it.
// shortcut: CPU only; the agent collects no traffic signal, so "no traffic" is not claimed.
type Abandoned struct {
	Workloads []AbandonedLine
	Monthly   float64
	Short     bool // some line has less than AbandonedDays of history
}

// BuildAbandoned judges the pods of a Window (others have no mean and give nothing). Static pods and DaemonSets are skipped:
// the first cannot be removed through the API, the second is meant to run on every node whatever the load.
// A workload with a container that has no usage data is never listed. Native sidecars do not count: a busy mesh proxy
// does not make a workload used.
func BuildAbandoned(pods []Pod, workloads []Line) *Abandoned {
	type agg struct {
		pods int
		peak int64
		span time.Duration // the shortest of its containers
		ok   bool
	}
	by := map[[2]string]*agg{}
	for _, p := range pods {
		if kind, _, _ := strings.Cut(p.Workload, "/"); p.Static || kind == "DaemonSet" || kind == "Pod" {
			continue
		}
		k := [2]string{p.Namespace, p.Workload}
		a := by[k]
		if a == nil {
			a = &agg{ok: true, span: math.MaxInt64}
			by[k] = a
		}
		a.pods++
		n := 0
		for _, c := range p.Containers {
			if c.Sidecar {
				continue
			}
			n++
			sp := c.CPULast.Sub(c.CPUFirst)
			a.ok = a.ok && c.HasUsage && c.CPUN > 0 && c.CPUN >= abandonedMinSamples && sp >= abandonedMinSpan && c.CPUAvg < idleAvgMilli && c.CPUPeak < idlePeakMilli
			a.span = min(a.span, sp)
			a.peak = max(a.peak, c.CPUPeak)
		}
		a.ok = a.ok && n > 0
	}
	cost := map[[2]string]float64{}
	for _, l := range workloads {
		cost[[2]string{l.Namespace, l.Workload}] = l.Monthly
	}
	out := &Abandoned{}
	for k, a := range by {
		if a.ok {
			short := a.span < AbandonedDays*24*time.Hour
			out.Short = out.Short || short
			out.Workloads = append(out.Workloads, AbandonedLine{k[0], k[1], a.pods, a.peak, a.span.Hours(), short, cost[k]})
			out.Monthly += cost[k]
		}
	}
	if len(out.Workloads) == 0 {
		return nil
	}
	slices.SortFunc(out.Workloads, func(a, b AbandonedLine) int {
		return cmp.Or(cmp.Compare(b.Monthly, a.Monthly), cmp.Compare(a.Namespace+a.Workload, b.Namespace+b.Workload))
	})
	return out
}
