package report

import (
	"cmp"
	"slices"
	"strings"
)

// UnevictablePod is a pod that keeps its node from being drained, with every reason and what the node costs.
type UnevictablePod struct {
	Namespace, Pod, Node string
	Reasons              []string
	Monthly              float64 // the node it pins, per month; an estimate
}

// Unevictable is the Overview block (S26). Nil when no pod blocks.
type Unevictable struct {
	Pods    []UnevictablePod // dearest node first
	Nodes   int              // distinct nodes pinned
	Monthly float64          // their total price, each node counted once
}

// BuildUnevictable lists pods with a reason to stay put: a PDB with no disruptions allowed, safe-to-evict=false,
// local storage (hostPath, emptyDir) or no controller. DaemonSet and static pods never move, so they are skipped.
// Node prices come from n.Prices (the caller fills flat ones).
func BuildUnevictable(nodes []Node, pods []Pod, pdbs []PDB) *Unevictable {
	cost := map[string]float64{}
	for _, n := range nodes {
		cost[n.Name] = n.Prices.Monthly(n.Capacity)
	}
	u := &Unevictable{}
	pinned := map[string]bool{}
	for _, p := range pods {
		kind, _, _ := strings.Cut(p.Workload, "/")
		if p.Static || kind == "DaemonSet" {
			continue
		}
		var why []string
		if slices.ContainsFunc(pdbs, func(b PDB) bool {
			return b.Strict && b.Namespace == p.Namespace && slices.Contains(b.Workloads, p.Workload)
		}) {
			why = append(why, "PodDisruptionBudget allows 0 disruptions")
		}
		if p.EvictRead && p.NoEvict {
			why = append(why, "safe-to-evict is false")
		}
		// Cluster Autoscaler skips this rule for safe-to-evict "true"; an old agent (EvictRead false) cannot say, so it stays flagged.
		local := !(p.EvictRead && p.SafeEvict)
		if local && p.HostPath {
			why = append(why, "local storage (hostPath)")
		}
		if local && p.EmptyDir {
			why = append(why, "local storage (emptyDir)")
		}
		if kind == "Pod" {
			why = append(why, "no controller")
		}
		if len(why) == 0 {
			continue
		}
		u.Pods = append(u.Pods, UnevictablePod{p.Namespace, p.Name, p.Node, why, cost[p.Node]})
		if _, known := cost[p.Node]; known && !pinned[p.Node] {
			pinned[p.Node] = true
			u.Nodes++
			u.Monthly += cost[p.Node]
		}
	}
	if len(u.Pods) == 0 {
		return nil
	}
	slices.SortStableFunc(u.Pods, func(a, b UnevictablePod) int {
		return cmp.Or(cmp.Compare(b.Monthly, a.Monthly), cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Pod, b.Pod))
	})
	return u
}
