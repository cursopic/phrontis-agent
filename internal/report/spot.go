package report

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// PDB is a PodDisruptionBudget, already resolved by the agent to the workloads whose pods its selector matches.
type PDB struct {
	Namespace, Name string
	Workloads       []string // "Kind/name", as in Pod.Workload
}

// SpotVerdict says whether a workload may run on spot nodes, and why (Reason is for the report, in plain words).
type SpotVerdict struct {
	Namespace, Workload string
	Safe                bool
	Reason              string
}

// SpotSafety judges each workload in pods (spec 011): only a Deployment with 2+ pods, no PVC and a PDB is safe.
// Replicas are the pods seen in this sample. Static pods are skipped: they cannot be moved. Sorted by namespace, workload.
func SpotSafety(pods []Pod, pdbs []PDB) []SpotVerdict {
	type wl struct{ ns, name string }
	replicas, claims := map[wl]int{}, map[wl]string{}
	for _, p := range pods {
		if p.Static {
			continue
		}
		k := wl{p.Namespace, p.Workload}
		replicas[k]++
		if len(p.PVCs) > 0 {
			claims[k] = p.PVCs[0]
		}
	}
	var out []SpotVerdict
	for k, n := range replicas {
		v := SpotVerdict{Namespace: k.ns, Workload: k.name}
		switch {
		case strings.HasPrefix(k.name, "StatefulSet/"):
			v.Reason = "StatefulSet keeps state and a stable identity"
		case !strings.HasPrefix(k.name, "Deployment/"):
			v.Reason = fmt.Sprintf("%s is not a Deployment", k.name[:strings.Index(k.name+"/", "/")])
		case claims[k] != "":
			v.Reason = fmt.Sprintf("mounts the volume claim %s", claims[k])
		case n < 2:
			v.Reason = "runs a single replica, so a spot interruption stops it"
		case !slices.ContainsFunc(pdbs, func(b PDB) bool { return b.Namespace == k.ns && slices.Contains(b.Workloads, k.name) }):
			v.Reason = fmt.Sprintf("%d replicas but no PodDisruptionBudget to keep some running", n)
		default:
			v.Safe, v.Reason = true, fmt.Sprintf("%d replicas with a PodDisruptionBudget", n)
		}
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b SpotVerdict) int {
		return strings.Compare(a.Namespace+"/"+a.Workload, b.Namespace+"/"+b.Workload)
	})
	return out
}

// SpotLine is one spot-safe workload: what it costs per month now and what spot would save.
type SpotLine struct {
	Namespace, Workload string
	Pods                int
	Monthly, Saving     float64
}

// SpotSavings prices the safe workloads of verdicts (spec 011). discount is the share of its price a node saves on
// spot (0..1), by node name; a pod on a node without an entry saves nothing. A workload's cost is split evenly over its
// pods, so this is an estimate. Biggest saving first.
func SpotSavings(verdicts []SpotVerdict, workloads []Line, pods []Pod, discount map[string]float64) []SpotLine {
	type wl struct{ ns, name string }
	n, off := map[wl]int{}, map[wl]float64{}
	for _, p := range pods {
		if !p.Static {
			k := wl{p.Namespace, p.Workload}
			n[k]++
			off[k] += discount[p.Node]
		}
	}
	cost := map[wl]float64{}
	for _, l := range workloads {
		cost[wl{l.Namespace, l.Workload}] = l.Monthly
	}
	var out []SpotLine
	for _, v := range verdicts {
		k := wl{v.Namespace, v.Workload}
		if _, ok := cost[k]; v.Safe && ok && n[k] > 0 {
			out = append(out, SpotLine{v.Namespace, v.Workload, n[k], cost[k], cost[k] * off[k] / float64(n[k])})
		}
	}
	slices.SortStableFunc(out, func(a, b SpotLine) int { return cmp.Compare(b.Saving, a.Saving) })
	return out
}
