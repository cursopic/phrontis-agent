// Package report turns a cluster snapshot into monthly cost and rightsizing figures.
// It is pure (no Kubernetes client) so the math is unit-tested directly.
package report

import (
	"cmp"
	"maps"
	"math"
	"slices"
	"strings"
)

const (
	HoursPerMonth = 730
	cpuHeadroom   = 1.2
	memHeadroom   = 1.3 // memory gets more: running out is an OOM kill, not throttling
	minMilliCPU   = 10
	minMemBytes   = 32 << 20
	material      = 0.2 // skip changes smaller than 20% of the current request
	throttleAt    = 0.9 // CPU usage this close to the limit means the container is being throttled
)

// Prices are flat rates per vCPU-hour and per GiB-hour.
type Prices struct{ CPUHour, GiBHour float64 }

type Resources struct{ MilliCPU, MemBytes int64 }

func (p Prices) plus(q Prices) Prices { return Prices{p.CPUHour + q.CPUHour, p.GiBHour + q.GiBHour} }

// Split turns a whole-node hourly price into per-vCPU and per-GiB rates, keeping ref's CPU:GiB price ratio,
// so that Split(h, c, ref).Monthly(c) == h*HoursPerMonth. It returns the zero Prices when it cannot.
func Split(hourly float64, c Resources, ref Prices) Prices {
	den := float64(c.MilliCPU)/1000*ref.CPUHour + float64(c.MemBytes)/(1<<30)*ref.GiBHour
	if hourly <= 0 || den <= 0 {
		return Prices{}
	}
	k := hourly / den
	return Prices{ref.CPUHour * k, ref.GiBHour * k}
}

func (p Prices) Monthly(r Resources) float64 {
	return (float64(r.MilliCPU)/1000*p.CPUHour + float64(r.MemBytes)/(1<<30)*p.GiBHour) * HoursPerMonth
}

func Max(a, b Resources) Resources {
	return Resources{max(a.MilliCPU, b.MilliCPU), max(a.MemBytes, b.MemBytes)}
}
func add(a, b Resources) Resources {
	return Resources{a.MilliCPU + b.MilliCPU, a.MemBytes + b.MemBytes}
}

// excess is how far a exceeds b, per resource, floored at 0.
func excess(a, b Resources) Resources {
	return Resources{max(a.MilliCPU-b.MilliCPU, 0), max(a.MemBytes-b.MemBytes, 0)}
}

// share fits one resource of a node: requests are paid first, usage above requests (burst) shares what is left pro rata.
// It returns the factors applied to requests and to bursts, so that req*fr + burst*fb = min(req+burst, capacity).
func share(capacity, req, burst int64) (fr, fb float64) {
	switch {
	case req > capacity: // only with a stale snapshot: the scheduler keeps requests within the node
		return float64(capacity) / float64(req), 0
	case burst > capacity-req:
		return 1, float64(capacity-req) / float64(burst)
	}
	return 1, 1
}

type Node struct {
	Name     string
	Capacity Resources
	Prices   Prices // zero: use the fallback passed to Build
}

type Container struct {
	Name           string
	Request, Limit Resources // a zero Limit field means no limit
	Usage          Resources // highest sample seen (Window: p95 CPU, peak memory)
	HasUsage       bool      // false when metrics-server had no sample for it
	OOMKilled      bool      // its last restart was an OOM kill
	OOMFloor       int64     `json:"-"` // Window: OOMFloor of the request and limit it was killed with; 0: of Request and Limit
	// The memory limit in its ReplicaSet's, StatefulSet's or DaemonSet's pod template, or the LimitRange default: the webhook
	// may have raised Limit, but caps memory by this one (MemCap). 0: none, or not read (another owner kind, an older
	// agent), and Limit stands in.
	ManifestMemLimit int64 `json:",omitempty"`
}

type Pod struct {
	Namespace, Name, Workload, Node string
	Containers                      []Container
	Static                          bool     // mirror pod: it costs, but its requests cannot be changed via the API
	PVCs                            []string // PersistentVolumeClaims it mounts; absent in samples from older agents
}

// Line is one row of cost; Workload is empty on namespace rows.
type Line struct {
	Namespace, Workload string
	Pods                int
	Monthly             float64
}

type Rec struct {
	Namespace, Workload, Container string
	Pods                           int
	Request, Recommended, Limit    Resources
	MonthlySaving                  float64 // negative when the request must grow
	OOMKilled                      bool    // memory was floored at the last OOM kill
	Throttled                      bool    // CPU usage reaches the limit: raise the limit, the request alone will not help
	HPA                            *HPARef `json:",omitempty"` // an HPA scales this workload: changing a request can change its replica count
}

type Report struct {
	Nodes                 int
	Capacity              Resources
	Monthly, IdleMonthly  float64
	Namespaces, Workloads []Line
	Recs                  []Rec
	Sizes                 []Size `json:"-"` // served to the workload autoscaler (GET /v1/clusters/{id}/recommendations), not in the report
}

// Size is one container's recommended requests before the 20% filter that makes Recs: once the autoscaler has applied a
// rec, the running request equals it and Recs drops it, but the next rollout must still get it (spec 009).
type Size struct {
	Namespace, Workload, Container string
	Requests                       Resources
}

// Build charges each container its request plus its usage above the request (burst), fitted into its node (see share),
// so on every listed node charges + idle = node cost (spec 001). A pod on a node not in nodes is charged fallback, unfitted.
// It recommends requests for containers whose peak usage is far from what they ask for.
// shortcut: init containers and pod overhead are ignored; add them when the numbers prove to matter on a real cluster.
// Each node is priced at its own Prices, or at fallback when those are zero.
func Build(nodes []Node, pods []Pod, fallback Prices) Report {
	byNode := map[string]Prices{}
	for _, n := range nodes {
		byNode[n.Name] = orElse(n.Prices, fallback)
	}
	var r Report
	for _, n := range nodes {
		r.Nodes++
		r.Capacity = add(r.Capacity, n.Capacity)
		r.Monthly += byNode[n.Name].Monthly(n.Capacity)
	}

	type agg struct {
		pods           int
		req, lim, peak Resources
		oom            bool
		oomFloor       int64
		manLim         int64 // the highest manifest memory limit, for MemCap
		nowMonthly     float64
		prices         Prices // sum of the node prices of its pods
	}
	reqs, bursts := map[string]Resources{}, map[string]Resources{}
	for _, pod := range pods {
		for _, c := range pod.Containers {
			reqs[pod.Node] = add(reqs[pod.Node], c.Request)
			bursts[pod.Node] = add(bursts[pod.Node], excess(c.Usage, c.Request))
		}
	}
	type fit struct{ req, burst Prices } // a node's prices scaled by share
	fits := map[string]fit{}
	for _, n := range nodes {
		p, rq, b := byNode[n.Name], reqs[n.Name], bursts[n.Name]
		rc, bc := share(n.Capacity.MilliCPU, rq.MilliCPU, b.MilliCPU)
		rm, bm := share(n.Capacity.MemBytes, rq.MemBytes, b.MemBytes)
		fits[n.Name] = fit{Prices{p.CPUHour * rc, p.GiBHour * rm}, Prices{p.CPUHour * bc, p.GiBHour * bm}}
		r.IdleMonthly += p.Monthly(excess(n.Capacity, add(rq, b)))
	}
	nsLines := map[string]*Line{}
	wlLines := map[[2]string]*Line{}
	conts := map[ckey]*agg{}

	for _, pod := range pods {
		p, ok := byNode[pod.Node]
		f := fits[pod.Node]
		if !ok {
			p, f = fallback, fit{fallback, fallback}
		}
		var cost float64
		for _, c := range pod.Containers {
			a := Max(c.Request, c.Usage)
			cost += f.req.Monthly(c.Request) + f.burst.Monthly(excess(c.Usage, c.Request))
			if !c.HasUsage || pod.Static {
				continue // never size what we have not measured or cannot change
			}
			k := ckey{pod.Namespace, pod.Workload, c.Name}
			g := conts[k]
			if g == nil {
				g = &agg{}
				conts[k] = g
			}
			g.pods++
			g.req = Max(g.req, c.Request)
			g.lim = Max(g.lim, c.Limit)
			g.peak = Max(g.peak, c.Usage)
			g.manLim = max(g.manLim, cmp.Or(c.ManifestMemLimit, c.Limit.MemBytes))
			if c.OOMKilled { // the floor at the kill: a limit the autoscaler raised since must not raise the next rec
				g.oom, g.oomFloor = true, max(g.oomFloor, cmp.Or(c.OOMFloor, OOMFloor(c.Request.MemBytes, c.Limit.MemBytes)))
			}
			g.nowMonthly += p.Monthly(a) // unfitted: the saving compares list prices of today's and the recommended size
			g.prices = g.prices.plus(p)
		}
		line(nsLines, pod.Namespace, Line{Namespace: pod.Namespace}).add(cost)
		line(wlLines, [2]string{pod.Namespace, pod.Workload}, Line{Namespace: pod.Namespace, Workload: pod.Workload}).add(cost)
	}
	r.Namespaces = sorted(nsLines)
	r.Workloads = sorted(wlLines)

	for k, g := range conts {
		rec := Floor(Resources{int64(math.Ceil(float64(g.peak.MilliCPU) * cpuHeadroom)), ceilMi(float64(g.peak.MemBytes) * memHeadroom)})
		rec.MemBytes = max(rec.MemBytes, g.oomFloor)
		r.Sizes = append(r.Sizes, Size{k.ns, k.wl, k.c, rec}) // uncapped: Mutate applies MemCap and journals when it held
		rec.MemBytes, _ = MemCap(rec.MemBytes, g.manLim)      // so the dry-run shows what the webhook writes (P36c)
		throttled := g.lim.MilliCPU > 0 && float64(g.peak.MilliCPU) >= throttleAt*float64(g.lim.MilliCPU)
		if !Changed(g.req.MilliCPU, rec.MilliCPU) && !Changed(g.req.MemBytes, rec.MemBytes) && !throttled {
			continue
		}
		// rec >= every pod's usage, so after the change each pod is charged exactly rec.
		r.Recs = append(r.Recs, Rec{k.ns, k.wl, k.c, g.pods, g.req, rec, g.lim, g.nowMonthly - g.prices.Monthly(rec), g.oom, throttled, nil})
	}
	slices.SortFunc(r.Recs, func(a, b Rec) int {
		return cmp.Or(cmp.Compare(b.MonthlySaving, a.MonthlySaving), cmp.Compare(a.Namespace+a.Workload+a.Container, b.Namespace+b.Workload+b.Container))
	})
	slices.SortFunc(r.Sizes, func(a, b Size) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Workload, b.Workload), cmp.Compare(a.Container, b.Container))
	})
	return r
}

// ceilMi rounds bytes up to whole Mi, so the printed value is the real one.
func ceilMi(b float64) int64 { return int64(math.Ceil(b/(1<<20))) << 20 }

// Floor applies spec 002's minimums to a recommended request: CPU at least 10m, memory rounded up to Mi and at least 32Mi.
func Floor(r Resources) Resources {
	return Resources{max(r.MilliCPU, minMilliCPU), max(ceilMi(float64(r.MemBytes)), minMemBytes)}
}

// MemCap is the most memory the workload autoscaler gives a container whose manifest sets memory limit lim (0: none):
// twice that limit, rounded up to Mi (P36b), and never below Floor's 32Mi minimum. A lower mem is returned as is; capped
// says the cap held mem down.
func MemCap(mem, lim int64) (_ int64, capped bool) {
	if lim <= 0 || mem-lim <= lim { // no limit, or at most 2x it; written so it cannot overflow
		return mem, false
	}
	c := max(ceilMi(2*float64(lim)), minMemBytes)
	return min(mem, c), mem > c
}

// OOMFloor is the least memory request for a container OOM-killed with request req and limit lim (0: none):
// it was killed at its limit (or, with none, somewhere above its request), so never size below that.
func OOMFloor(req, lim int64) int64 { return max(req, ceilMi(float64(lim)*memHeadroom)) }

type ckey struct{ ns, wl, c string }

// Window folds samples taken over time into one snapshot: the latest sample's pods, with each container's usage
// replaced by the p95 CPU and the peak memory of that workload's container across every sample and replica.
// shortcut: keeps every CPU value in memory (8 bytes × samples × replicas); move to a bucketed histogram when a window outgrows RAM.
type Window struct {
	Samples int
	cpu     map[ckey][]int64
	mem     map[ckey]int64
	oom     map[ckey]int64 // OOM-killed in the window: the highest OOMFloor seen at a kill
	last    []Pod
	seen    map[[2]string]Pod // each Deployment's and StatefulSet's latest pod, for Seen
}

// Add takes one sample; call it oldest first.
func (w *Window) Add(pods []Pod) {
	if w.cpu == nil {
		w.cpu, w.mem, w.oom, w.seen = map[ckey][]int64{}, map[ckey]int64{}, map[ckey]int64{}, map[[2]string]Pod{}
	}
	w.Samples++
	w.last = pods
	for _, p := range pods {
		if kind, _, _ := strings.Cut(p.Workload, "/"); kind == "Deployment" || kind == "StatefulSet" {
			w.seen[[2]string{p.Namespace, p.Workload}] = p
		}
		for _, c := range p.Containers {
			k := ckey{p.Namespace, p.Workload, c.Name}
			if c.OOMKilled {
				w.oom[k] = max(w.oom[k], OOMFloor(c.Request.MemBytes, c.Limit.MemBytes))
			}
			if c.HasUsage {
				w.cpu[k] = append(w.cpu[k], c.Usage.MilliCPU)
				w.mem[k] = max(w.mem[k], c.Usage.MemBytes)
			}
		}
	}
}

// Pods returns the latest sample's pods with the folded usage.
func (w *Window) Pods() []Pod { return w.fold(w.last) }

// Seen is Pods plus the latest pod of each Deployment and StatefulSet in the window, so one scaled to 0 since keeps its
// size (and its OOM floor) for the workload autoscaler. Read only Sizes from it: a running workload is counted twice.
func (w *Window) Seen() []Pod {
	return w.fold(slices.AppendSeq(slices.Clone(w.last), maps.Values(w.seen)))
}

func (w *Window) fold(pods []Pod) []Pod {
	out := make([]Pod, len(pods))
	for i, p := range pods {
		p.Containers = slices.Clone(p.Containers)
		for j := range p.Containers {
			c := &p.Containers[j]
			k := ckey{p.Namespace, p.Workload, c.Name}
			c.OOMFloor, c.OOMKilled = w.oom[k]
			if v := w.cpu[k]; len(v) > 0 {
				slices.Sort(v)
				c.Usage, c.HasUsage = Resources{v[(len(v)*95+99)/100-1], w.mem[k]}, true // ceil(0.95n)-th smallest
			}
		}
		out[i] = p
	}
	return out
}

func orElse(p, fallback Prices) Prices {
	if p == (Prices{}) {
		return fallback
	}
	return p
}

// Changed reports whether rec is worth applying over the current request cur: cur is unset or rec is at least 20% away.
func Changed(cur, rec int64) bool {
	return cur == 0 || math.Abs(float64(rec-cur)) >= material*float64(cur)
}

func (l *Line) add(cost float64) { l.Pods++; l.Monthly += cost }

func line[K comparable](m map[K]*Line, k K, zero Line) *Line {
	if m[k] == nil {
		m[k] = &zero
	}
	return m[k]
}

func sorted[K comparable](m map[K]*Line) []Line {
	out := make([]Line, 0, len(m))
	for _, l := range m {
		out = append(out, *l)
	}
	slices.SortFunc(out, func(a, b Line) int {
		return cmp.Or(cmp.Compare(b.Monthly, a.Monthly), cmp.Compare(a.Namespace+a.Workload, b.Namespace+b.Workload))
	})
	return out
}

// Volume is one PersistentVolume. Namespace and Claim are set only while it is Bound to a claim.
// Type is the EBS volume type, or "" when it is not an EBS CSI volume or its StorageClass is gone.
type Volume struct {
	Name, Namespace, Claim string
	Bytes                  int64
	Type                   string
}

// StorageLine is one row of storage: a namespace's claimed volumes, or the unclaimed ones (Namespace "").
// Unpriced counts the volumes whose type has no price; their GiB are counted but add nothing to Monthly.
type StorageLine struct {
	Namespace string
	Volumes   int
	GiB       float64
	Monthly   float64
	Unpriced  int
}

// Storage is billed per provisioned GiB, separate from compute (it never enters Report.Monthly or IdleMonthly).
// Unclaimed holds PVs with no claim (Available, Released): provisioned and billed, used by nobody.
type Storage struct {
	Namespaces []StorageLine
	Unclaimed  StorageLine
	Total      StorageLine
}

// BuildStorage prices volumes at perGiBMonth[type]; a missing type leaves the volume unpriced.
func BuildStorage(vols []Volume, perGiBMonth map[string]float64) Storage {
	var s Storage
	by := map[string]*StorageLine{}
	for _, v := range vols {
		l := &s.Unclaimed
		if v.Namespace != "" {
			if by[v.Namespace] == nil {
				by[v.Namespace] = &StorageLine{Namespace: v.Namespace}
			}
			l = by[v.Namespace]
		}
		gib := float64(v.Bytes) / (1 << 30)
		price, ok := perGiBMonth[v.Type]
		for _, x := range []*StorageLine{l, &s.Total} {
			x.Volumes++
			x.GiB += gib
			if ok {
				x.Monthly += gib * price
			} else {
				x.Unpriced++
			}
		}
	}
	for _, l := range by {
		s.Namespaces = append(s.Namespaces, *l)
	}
	slices.SortFunc(s.Namespaces, func(a, b StorageLine) int {
		return cmp.Or(cmp.Compare(b.Monthly, a.Monthly), cmp.Compare(b.GiB, a.GiB), cmp.Compare(a.Namespace, b.Namespace))
	})
	return s
}

// LoadBalancer is one Service of type LoadBalancer with an assigned ingress. Kind is "classic", "nlb" or "unknown"
// (another controller or an unrecognised annotation: not priced).
type LoadBalancer struct {
	Name, Namespace, Kind string
}

// LBLine is one namespace's load balancers; Unpriced counts those whose kind has no hourly price.
type LBLine struct {
	Namespace string
	Count     int
	Monthly   float64
	Unpriced  int
}

// LoadBalancers is the hourly charge of load balancers, separate from compute (it never enters Report.Monthly).
// shortcut: LCU and data-processed charges are usage-based and not visible to a read-only agent, so they are not included.
type LoadBalancers struct {
	Namespaces []LBLine
	Total      LBLine
}

// BuildLoadBalancers prices each load balancer at hourly[kind] x HoursPerMonth; a missing kind leaves it unpriced.
func BuildLoadBalancers(lbs []LoadBalancer, hourly map[string]float64) LoadBalancers {
	var s LoadBalancers
	by := map[string]*LBLine{}
	for _, lb := range lbs {
		if by[lb.Namespace] == nil {
			by[lb.Namespace] = &LBLine{Namespace: lb.Namespace}
		}
		price, ok := hourly[lb.Kind]
		for _, x := range []*LBLine{by[lb.Namespace], &s.Total} {
			x.Count++
			if ok {
				x.Monthly += price * HoursPerMonth
			} else {
				x.Unpriced++
			}
		}
	}
	for _, l := range by {
		s.Namespaces = append(s.Namespaces, *l)
	}
	slices.SortFunc(s.Namespaces, func(a, b LBLine) int {
		return cmp.Or(cmp.Compare(b.Monthly, a.Monthly), cmp.Compare(b.Count, a.Count), cmp.Compare(a.Namespace, b.Namespace))
	})
	return s
}
