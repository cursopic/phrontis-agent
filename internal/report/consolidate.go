package report

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// NoEvictAnnotation stops the Cluster Autoscaler from removing a node that runs the pod.
const NoEvictAnnotation = "cluster-autoscaler.kubernetes.io/safe-to-evict"

// NodeVerdict is one candidate node (requests under 50% of CPU and of memory) and whether its pods fit elsewhere (spec 019).
type NodeVerdict struct {
	Node     string
	CPU, Mem float64  // requests as a share of capacity, 0..1
	Monthly  float64  // the node's price; an estimate
	Empty    bool     // its pods fit on other nodes
	To       []string `json:",omitempty"` // Empty: the nodes its pods go to
	Reason   string   `json:",omitempty"` // not Empty: what keeps it
}

// Consolidation is the Nodes-screen block. Nil with fewer than 2 nodes.
type Consolidation struct {
	Verdicts []NodeVerdict // candidates, least loaded first
	Count    int           // how many can be emptied
	Saving   float64       // per month; an estimate that ignores affinity, taints and node selectors
	OldAgent bool          // no pod carries the eviction annotation field, so safe-to-evict was not checked
}

// BuildConsolidation runs Consolidate and totals it. Node prices come from n.Prices (the caller fills flat ones).
func BuildConsolidation(nodes []Node, pods []Pod, pdbs []PDB) *Consolidation {
	if len(nodes) < 2 {
		return nil
	}
	c := &Consolidation{Verdicts: Consolidate(nodes, pods, pdbs), OldAgent: len(pods) > 0 && !slices.ContainsFunc(pods, func(p Pod) bool { return p.EvictRead })}
	for _, v := range c.Verdicts {
		if v.Empty {
			c.Count++
			c.Saving += v.Monthly
		}
	}
	return c
}

func podRequest(p Pod) Resources {
	var r Resources
	for _, c := range p.Containers {
		r.MilliCPU += c.Request.MilliCPU
		r.MemBytes += c.Request.MemBytes
	}
	return r
}

// blocker says why a pod keeps its node, or "" when it can move. DaemonSet pods never reach it.
func blocker(p Pod) string {
	kind, name, _ := strings.Cut(p.Workload, "/")
	switch {
	case p.Static:
		return "runs static pod " + p.Name
	case kind == "StatefulSet":
		return "runs StatefulSet/" + name
	case kind == "Pod":
		return "runs pod " + p.Name + " without a controller"
	case len(p.PVCs) > 0:
		return "mounts claim " + p.PVCs[0]
	case p.NoEvict:
		return "pod has safe-to-evict=false"
	}
	return ""
}

// Consolidate says which lightly loaded nodes could be emptied (spec 019). A candidate has requests under 50% of CPU and
// of memory. Candidates go least loaded first; its movable pods are placed first-fit-decreasing on the other nodes within
// capacity. A node that is emptied takes no more pods, and a node that took pods is not emptied later. Pods of a
// DaemonSet stay and block nothing.
// shortcut: PDBs are unused (a zero-headroom PDB can still stop a drain), and affinity, taints and node selectors are
// ignored, so a saving can be too high; upgrade when a customer asks.
func Consolidate(nodes []Node, pods []Pod, _ []PDB) []NodeVerdict {
	if len(nodes) < 2 {
		return nil
	}
	type state struct {
		Node
		used    Resources
		pods    []Pod
		emptied bool
		took    string // the first node whose pods came here
	}
	st := map[string]*state{}
	for _, n := range nodes {
		st[n.Name] = &state{Node: n}
	}
	add := func(s *state, p Pod) {
		r := podRequest(p)
		s.used.MilliCPU += r.MilliCPU
		s.used.MemBytes += r.MemBytes
		s.pods = append(s.pods, p)
	}
	for _, p := range pods {
		if s := st[p.Node]; s != nil {
			add(s, p)
		}
	}
	load := func(s *state) (cpu, mem float64) {
		return float64(s.used.MilliCPU) / float64(max(s.Capacity.MilliCPU, 1)), float64(s.used.MemBytes) / float64(max(s.Capacity.MemBytes, 1))
	}
	var cands []*state
	for _, n := range nodes {
		if s := st[n.Name]; s.Capacity.MilliCPU > 0 && s.Capacity.MemBytes > 0 {
			if c, m := load(s); c < 0.5 && m < 0.5 {
				cands = append(cands, s)
			}
		}
	}
	slices.SortStableFunc(cands, func(a, b *state) int {
		ac, am := load(a)
		bc, bm := load(b)
		return cmp.Or(cmp.Compare(ac+am, bc+bm), cmp.Compare(a.Name, b.Name))
	})
	var out []NodeVerdict
	for _, s := range cands {
		c, m := load(s)
		v := NodeVerdict{Node: s.Name, CPU: c, Mem: m, Monthly: s.Prices.Monthly(s.Capacity)}
		var move []Pod
		for _, p := range s.pods {
			if kind, _, _ := strings.Cut(p.Workload, "/"); kind == "DaemonSet" {
				continue
			}
			if v.Reason = blocker(p); v.Reason != "" {
				break
			}
			move = append(move, p)
		}
		if v.Reason == "" && s.took != "" {
			v.Reason = "takes pods from " + s.took
		}
		if v.Reason == "" {
			slices.SortStableFunc(move, func(a, b Pod) int {
				ar, br := podRequest(a), podRequest(b)
				return cmp.Or(cmp.Compare(br.MilliCPU, ar.MilliCPU), cmp.Compare(br.MemBytes, ar.MemBytes), cmp.Compare(a.Name, b.Name))
			})
			free := map[string]Resources{}
			for _, n := range nodes {
				if o := st[n.Name]; o != s && !o.emptied && !o.Unschedulable && o.Capacity.MilliCPU > 0 && o.Capacity.MemBytes > 0 {
					free[n.Name] = Resources{o.Capacity.MilliCPU - o.used.MilliCPU, o.Capacity.MemBytes - o.used.MemBytes}
				}
			}
			placed := map[string][]Pod{}
		pods:
			for _, p := range move {
				r := podRequest(p)
				for _, n := range nodes { // first fit, in node order
					if f, ok := free[n.Name]; ok && f.MilliCPU >= r.MilliCPU && f.MemBytes >= r.MemBytes {
						free[n.Name] = Resources{f.MilliCPU - r.MilliCPU, f.MemBytes - r.MemBytes}
						placed[n.Name] = append(placed[n.Name], p)
						continue pods
					}
				}
				v.Reason = fmt.Sprintf("pod %s does not fit on the other nodes", p.Name)
				break
			}
			if v.Reason == "" {
				v.Empty, s.emptied = true, true
				for name, ps := range placed {
					for _, p := range ps {
						add(st[name], p)
					}
					if st[name].took == "" {
						st[name].took = s.Name
					}
					v.To = append(v.To, name)
				}
				slices.Sort(v.To)
			}
		}
		out = append(out, v)
	}
	return out
}
