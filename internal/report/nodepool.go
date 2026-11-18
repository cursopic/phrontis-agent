package report

import (
	"cmp"
	"slices"
	"strings"
)

// Offer is one instance type a node pool could use, priced at its on-demand list price (USD per hour).
type Offer struct {
	Type, Family string
	Capacity     Resources
	Hourly       float64
}

// NodePool is the simulated cheapest fleet for the cluster's pods, with the rightsizing recommendations applied.
type NodePool struct {
	Families  []string // cheapest first, up to 5 distinct: the instance-family requirement
	Type      string
	Nodes     int
	Monthly   float64  // the cheapest simulation: Nodes of Type at on-demand list price
	Current   float64  // Report.Monthly, what the nodes cost now
	SpotShare float64  // share of Monthly on nodes that only run spot-safe pods
	OnDemand  []string // "ns/Kind/name" workloads to pin to on-demand
	Unplaced  int      // pods no offer can fit
}

const (
	nodeReserve = 0.10 // kubelet/OS share of every node's CPU and memory
	maxFamilies = 5
)

// BuildNodePool packs the pods onto each offer's nodes by first-fit decreasing and returns the cheapest.
// Spot-safe pods (Deployments with >= 2 pods in this sample) and on-demand pods are packed apart, since a node is one or the other.
// shortcut: flat 10% reserve instead of the AMI's per-type kube-reserved formula; use the formula once sims are checked against real nodes.
// shortcut: static pods are skipped (they live on nodes Karpenter does not manage).
// Cost is O(pods x nodes x offers), fine at cluster scale.
func BuildNodePool(pods []Pod, recs []Rec, offers []Offer, current float64) NodePool {
	if len(offers) == 0 {
		return NodePool{}
	}
	rec := map[ckey]Resources{}
	for _, r := range recs {
		rec[ckey{r.Namespace, r.Workload, r.Container}] = r.Recommended
	}
	type item struct {
		key  [2]string
		d    Resources
		spot bool
	}
	replicas := map[[2]string]int{}
	daemon := map[[2]string]Resources{} // per DaemonSet: its largest pod
	var items []item
	for _, p := range pods {
		if p.Static {
			continue
		}
		var d Resources
		for _, c := range p.Containers {
			r := c.Request
			if v, ok := rec[ckey{p.Namespace, p.Workload, c.Name}]; ok {
				r = v
			}
			d = add(d, r)
		}
		k := [2]string{p.Namespace, p.Workload}
		if strings.HasPrefix(p.Workload, "DaemonSet/") {
			daemon[k] = Max(daemon[k], d)
			continue
		}
		replicas[k]++
		items = append(items, item{key: k, d: d})
	}
	for i, it := range items {
		items[i].spot = strings.HasPrefix(it.key[1], "Deployment/") && replicas[it.key] >= 2
	}
	var reserve Resources
	for _, d := range daemon {
		reserve = add(reserve, d)
	}

	type plan struct {
		o               Offer
		cost            float64
		nodes, spotNode int
	}
	alloc := func(o Offer) Resources {
		a := Resources{int64(float64(o.Capacity.MilliCPU) * (1 - nodeReserve)), int64(float64(o.Capacity.MemBytes) * (1 - nodeReserve))}
		return Resources{a.MilliCPU - reserve.MilliCPU, a.MemBytes - reserve.MemBytes}
	}
	fits := func(d, a Resources) bool { return d.MilliCPU <= a.MilliCPU && d.MemBytes <= a.MemBytes }
	// pack returns the number of nodes of allocatable a that hold ds by first-fit decreasing.
	pack := func(ds []Resources, a Resources) int {
		share := func(d Resources) float64 {
			return max(float64(d.MilliCPU)/float64(a.MilliCPU), float64(d.MemBytes)/float64(a.MemBytes))
		}
		slices.SortStableFunc(ds, func(x, y Resources) int { return cmp.Compare(share(y), share(x)) })
		var used []Resources
	next:
		for _, d := range ds {
			for i, u := range used {
				if fits(add(u, d), a) {
					used[i] = add(u, d)
					continue next
				}
			}
			used = append(used, d)
		}
		return len(used)
	}

	var out NodePool
	var plans []plan
	for _, o := range offers {
		if a := alloc(o); a.MilliCPU > 0 && a.MemBytes > 0 {
			plans = append(plans, plan{o: o})
		}
	}
	// A pod no offer can fit is counted and left out, so one giant pod does not veto every offer.
	var spotD, odD []Resources
	var pins []string // only workloads the simulation placed
	placed := 0
	for _, it := range items {
		if !slices.ContainsFunc(plans, func(p plan) bool { return fits(it.d, alloc(p.o)) }) {
			out.Unplaced++
			continue
		}
		placed++
		if it.spot {
			spotD = append(spotD, it.d)
		} else {
			odD = append(odD, it.d)
			if pin := it.key[0] + "/" + it.key[1]; !slices.Contains(pins, pin) {
				pins = append(pins, pin)
			}
		}
	}
	slices.Sort(pins)
	if placed == 0 {
		return NodePool{Unplaced: out.Unplaced}
	}
	var ok []plan
	for _, p := range plans {
		a := alloc(p.o)
		if slices.ContainsFunc(append(slices.Clone(spotD), odD...), func(d Resources) bool { return !fits(d, a) }) {
			continue // some pod fits no empty node of this type
		}
		p.spotNode = pack(slices.Clone(spotD), a)
		p.nodes = p.spotNode + pack(slices.Clone(odD), a)
		p.cost = float64(p.nodes) * p.o.Hourly * HoursPerMonth
		ok = append(ok, p)
	}
	if len(ok) == 0 {
		return NodePool{Unplaced: out.Unplaced}
	}
	slices.SortFunc(ok, func(a, b plan) int {
		return cmp.Or(cmp.Compare(a.cost, b.cost), cmp.Compare(a.nodes, b.nodes), cmp.Compare(a.o.Type, b.o.Type))
	})
	best := ok[0]
	out.Type, out.Nodes, out.Monthly, out.Current, out.OnDemand = best.o.Type, best.nodes, best.cost, current, pins
	if best.cost > 0 {
		out.SpotShare = float64(best.spotNode) * best.o.Hourly * HoursPerMonth / best.cost
	}
	for _, p := range ok {
		if len(out.Families) < maxFamilies && !slices.Contains(out.Families, p.o.Family) {
			out.Families = append(out.Families, p.o.Family)
		}
	}
	return out
}
