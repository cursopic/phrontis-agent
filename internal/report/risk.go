package report

import (
	"cmp"
	"fmt"
	"slices"
)

// Risk is one reason a workload can fail or run slowly, whether or not its size must change (spec 033).
type Risk struct {
	Namespace, Workload string
	Container           string // empty for "hpa"
	Kind                string // "oom", "cpu" or "hpa"
	Detail              string
}

// throttled says the CPU usage is so close to the limit (0: none) that the container is likely throttled.
func throttled(usage, limit int64) bool {
	return limit > 0 && float64(usage) >= throttleAt*float64(limit)
}

// Risks lists the containers that were OOM-killed (usage not needed) or reach their CPU limit, and the HPAs at their
// maximum. Static pods count too. An HPA with equal minimum and maximum has a fixed size on purpose, and one from an older
// agent has no MaxReplicas: neither is a risk. One row per workload's container and kind, and one per HPA, sorted.
func Risks(pods []Pod, hpas []HPA) []Risk {
	type agg struct {
		oom      bool
		cpu, lim int64
	}
	byCont := map[ckey]agg{}
	for _, p := range pods {
		for _, c := range p.Containers {
			k := ckey{p.Namespace, p.Workload, c.Name}
			g := byCont[k]
			g.oom = g.oom || c.OOMKilled
			if c.HasUsage { // as in Build, so a rec and its risk agree
				g.cpu, g.lim = max(g.cpu, c.Usage.MilliCPU), max(g.lim, c.Limit.MilliCPU)
			}
			byCont[k] = g
		}
	}
	var out []Risk
	for k, g := range byCont {
		if g.oom {
			out = append(out, Risk{k.ns, k.wl, k.c, "oom", "OOM-killed"})
		}
		if throttled(g.cpu, g.lim) {
			out = append(out, Risk{k.ns, k.wl, k.c, "cpu", "CPU at its limit, likely throttled"})
		}
	}
	for _, h := range hpas {
		if h.MaxReplicas > 0 && h.Replicas >= h.MaxReplicas && h.MinReplicas < h.MaxReplicas {
			out = append(out, Risk{h.Namespace, h.Target, "", "hpa", fmt.Sprintf("HPA %s at its maximum, %d replicas", h.Name, h.MaxReplicas)})
		}
	}
	slices.SortFunc(out, func(a, b Risk) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Workload, b.Workload), cmp.Compare(a.Container, b.Container),
			cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Detail, b.Detail))
	})
	return out
}
