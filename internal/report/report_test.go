package report

import (
	"math"
	"slices"
	"testing"
	"time"
)

const mi, gi = 1 << 20, 1 << 30

func near(t *testing.T, what string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-6 {
		t.Errorf("%s = %.6f, want %.6f", what, got, want)
	}
}

func TestBuild(t *testing.T) {
	p := Prices{CPUHour: 0.03, GiBHour: 0.004}
	nodes := []Node{{Name: "n1", Capacity: Resources{4000, 8 * gi}}}
	pods := []Pod{
		// over-requested deployment, two replicas
		{"shop", "web-a", "Deployment/web", "n1", []Container{ct("app", Resources{1000, 2 * gi}, Resources{100, 200 * mi}, true)}, false, nil, false, false, false, false, false, false, nil, false, false},
		{"shop", "web-b", "Deployment/web", "n1", []Container{ct("app", Resources{1000, 2 * gi}, Resources{120, 300 * mi}, true)}, false, nil, false, false, false, false, false, false, nil, false, false},
		// no requests at all: charged by usage, told to set them
		{"ops", "cron", "Pod/cron", "n1", []Container{ct("job", Resources{}, Resources{500, 100 * mi}, true)}, false, nil, false, false, false, false, false, false, nil, false, false},
		// unmeasured: charged by request, never sized
		{"ops", "new", "Pod/new", "n1", []Container{ct("app", Resources{200, 64 * mi}, Resources{}, false)}, false, nil, false, false, false, false, false, false, nil, false, false},
		// static pod: costed, never sized
		{Namespace: "kube-system", Name: "etcd", Workload: "Pod/etcd", Node: "n1", Static: true, Containers: []Container{ct("etcd", Resources{100, 0}, Resources{20, 50 * mi}, true)}},
	}
	r := Build(nodes, pods, p)

	near(t, "cluster", r.Monthly, (4*0.03+8*0.004)*730)
	alloc := Resources{2000 + 500 + 200 + 100, 4*gi + 100*mi + 64*mi + 50*mi}
	near(t, "idle", r.IdleMonthly, p.Monthly(Resources{4000 - alloc.MilliCPU, 8*gi - alloc.MemBytes}))
	near(t, "allocated+idle", r.Namespaces[0].Monthly+r.Namespaces[1].Monthly+r.Namespaces[2].Monthly+r.IdleMonthly, r.Monthly)
	if r.Namespaces[0].Namespace != "shop" || r.Workloads[0].Workload != "Deployment/web" || r.Workloads[0].Pods != 2 {
		t.Fatalf("lines not sorted by cost: %+v %+v", r.Namespaces, r.Workloads)
	}

	if len(r.Recs) != 2 {
		t.Fatalf("want 2 recs (web, cron), got %+v", r.Recs)
	}
	web := r.Recs[0]
	wantRec := Resources{144, 390 * mi} // ceil(120m*1.2), ceil(300Mi*1.3)
	if web.Workload != "Deployment/web" || web.Pods != 2 || web.Recommended != wantRec {
		t.Fatalf("web rec = %+v", web)
	}
	if web.Pod != "web-a" || web.CPUUsage != 120 || web.MemUsage != 300*mi {
		t.Errorf("web pod, usage = %q %d %d", web.Pod, web.CPUUsage, web.MemUsage)
	}
	near(t, "web cost", web.MonthlyCost, 2*p.Monthly(Resources{1000, 2 * gi}))
	near(t, "web saving", web.MonthlySaving, 2*p.Monthly(Resources{1000, 2 * gi})-2*p.Monthly(wantRec))

	cron := r.Recs[1]
	if cron.Workload != "Pod/cron" || cron.Recommended != (Resources{600, 130 * mi}) || cron.MonthlySaving >= 0 {
		t.Fatalf("cron rec = %+v (raising a request costs money, saving must be negative)", cron)
	}
}

// S51: each node's idle goes to its pods by cost; an empty node's idle and an unlisted node's pods stay out.
func TestShareIdle(t *testing.T) {
	p := Prices{CPUHour: 0.03, GiBHour: 0.004}
	nodes := []Node{
		{Name: "n1", Capacity: Resources{4000, 8 * gi}},
		{Name: "n2", Capacity: Resources{2000, 4 * gi}, Prices: Prices{CPUHour: 0.06, GiBHour: 0.008}},
		{Name: "empty", Capacity: Resources{1000, 2 * gi}},
	}
	pod := func(ns, node string, cpu int64) Pod {
		return Pod{Namespace: ns, Name: ns, Workload: "Pod/" + ns, Node: node, Containers: []Container{ct("app", Resources{cpu, 0}, Resources{}, false)}}
	}
	r := Build(nodes, []Pod{pod("a", "n1", 1000), pod("b", "n1", 3000), pod("b", "n2", 500), pod("gone", "nx", 100), pod("zero", "empty", 0)}, p)
	if r.IdleUnshared != 0 || r.Namespaces[0].IdleShare != 0 {
		t.Fatal("shared without ShareIdle")
	}
	r.ShareIdle()
	m1, n2 := p.Monthly(Resources{0, 8 * gi}), nodes[1].Prices.Monthly(Resources{1500, 4 * gi})
	empty := p.Monthly(nodes[2].Capacity)
	near(t, "unshared", r.IdleUnshared, empty)
	want := map[string]float64{"a": m1 / 4, "b": m1*3/4 + n2, "gone": 0, "zero": 0}
	for _, ls := range [][]Line{r.Namespaces, r.Workloads} {
		total := r.IdleUnshared
		for _, l := range ls {
			near(t, l.Namespace+l.Workload, l.IdleShare, want[l.Namespace])
			if l.Namespace != "gone" {
				total += l.Monthly + l.IdleShare
			}
		}
		near(t, "lines + unshared", total, r.Monthly)
	}
}

// S50: usage in percent of the request, per resource, over the containers that have both.
func TestUsePct(t *testing.T) {
	pod := func(wl string, cs ...Container) Pod {
		return Pod{Namespace: "ns", Workload: wl, Node: "n1", Containers: cs}
	}
	r := Build(nil, []Pod{
		pod("web", ct("app", Resources{1000, 1 * gi}, Resources{100, 512 * mi}, true)),
		pod("web", ct("app", Resources{1000, 1 * gi}, Resources{300, 512 * mi}, true),
			ct("new", Resources{5000, 8 * gi}, Resources{}, false), // unmeasured: in neither sum
			ct("free", Resources{}, Resources{900, 8 * gi}, true)), // no request: in neither sum
		pod("cpu", ct("app", Resources{100, 0}, Resources{200, 1 * gi}, true)), // over its request; no memory request
		pod("none", ct("app", Resources{}, Resources{200, 1 * gi}, true)),
	}, Prices{CPUHour: 0.03, GiBHour: 0.004})
	got := map[string]Line{}
	for _, l := range slices.Concat(r.Namespaces, r.Workloads) {
		got[l.Workload] = l
	}
	for wl, want := range map[string][2]float64{"web": {20, 50}, "cpu": {200, -1}, "none": {-1, -1}, "": {100 * 600. / 2100, 50}} {
		for i, p := range []*float64{got[wl].CPUPct, got[wl].MemPct} {
			if (p == nil) != (want[i] < 0) {
				t.Errorf("%q %d: nil is %v, want %v", wl, i, p == nil, want[i] < 0)
			} else if p != nil {
				near(t, wl, *p, want[i])
			}
		}
	}
}

// Spec 001: namespaces + idle = cluster cost, also when usage above requests overcommits a node.
// Requests are paid in full; the capacity they leave is shared by bursts pro rata; requests over capacity are scaled down.
func TestOvercommittedNodeKeepsTotal(t *testing.T) {
	p := Prices{CPUHour: 0.03, GiBHour: 0.004}
	cpu := func(milli float64) float64 { return milli / 1000 * p.CPUHour * HoursPerMonth }
	nodes := []Node{
		{Name: "n1", Capacity: Resources{2000, 4 * gi}}, // requests fill the CPU: burst gets nothing
		{Name: "n2", Capacity: Resources{1000, 1 * gi}}, // 400m left for 900m of burst: each burst gets 4/9
		{Name: "n3", Capacity: Resources{1000, 1 * gi}}, // requests 2000m on 1000m (stale snapshot): halved
	}
	pod := func(name, node string, req, use Resources) Pod {
		return Pod{"ns", name, "Pod/" + name, node, []Container{ct("app", req, use, true)}, false, nil, false, false, false, false, false, false, nil, false, false}
	}
	pods := []Pod{
		pod("big", "n1", Resources{1500, 1 * gi}, Resources{100, 200 * mi}),
		pod("burst", "n1", Resources{500, 1 * gi}, Resources{1000, 300 * mi}),
		pod("c", "n2", Resources{400, 0}, Resources{700, 0}),
		pod("d", "n2", Resources{200, 0}, Resources{800, 0}),
		pod("e", "n3", Resources{1500, 0}, Resources{0, 0}),
		pod("f", "n3", Resources{500, 0}, Resources{900, 0}),
	}
	r := Build(nodes, pods, p)

	near(t, "namespaces+idle", r.Namespaces[0].Monthly+r.IdleMonthly, r.Monthly)
	near(t, "idle", r.IdleMonthly, p.Monthly(Resources{0, 2*gi + 1*gi + 1*gi}))
	want := map[string]float64{
		"Pod/big":   cpu(1500) + p.Monthly(Resources{0, 1 * gi}),
		"Pod/burst": cpu(500) + p.Monthly(Resources{0, 1 * gi}),
		"Pod/c":     cpu(400 + 300*4.0/9),
		"Pod/d":     cpu(200 + 600*4.0/9),
		"Pod/e":     cpu(750),
		"Pod/f":     cpu(250),
	}
	for _, l := range r.Workloads {
		near(t, l.Workload, l.Monthly, want[l.Workload])
	}
}

func TestSmallGapIsNotARecommendation(t *testing.T) {
	pods := []Pod{{"a", "p", "Pod/p", "n", []Container{ct("c", Resources{120, 130 * mi}, Resources{100, 100 * mi}, true)}, false, nil, false, false, false, false, false, false, nil, false, false}}
	r := Build(nil, pods, Prices{1, 1})
	if len(r.Recs) != 0 {
		t.Fatalf("120m/130Mi vs recommended 120m/130Mi is no change, got %+v", r.Recs)
	}
	// The autoscaler still gets it: a container already at its rec must keep it on the next rollout.
	if want := []Size{{"a", "Pod/p", "c", Resources{120, 130 * mi}}}; !slices.Equal(r.Sizes, want) {
		t.Fatalf("Sizes = %+v, want %+v", r.Sizes, want)
	}
}

func TestSplit(t *testing.T) {
	ref := Prices{CPUHour: 0.03, GiBHour: 0.004}
	c := Resources{2000, 8 * gi}
	s := Split(0.115, c, ref)
	near(t, "monthly", s.Monthly(c), 0.115*HoursPerMonth)
	near(t, "ratio", s.CPUHour/s.GiBHour, ref.CPUHour/ref.GiBHour)
	if Split(0, c, ref) != (Prices{}) || Split(0.1, Resources{}, ref) != (Prices{}) || Split(0.1, c, Prices{}) != (Prices{}) {
		t.Error("want zero Prices when hourly or the denominator is not positive")
	}
}

func TestBuildPerNodePrices(t *testing.T) {
	flat := Prices{CPUHour: 0.03, GiBHour: 0.004}
	cheap, dear := Prices{0.01, 0.001}, Prices{0.05, 0.005}
	c := Resources{4000, 8 * gi}
	nodes := []Node{{Name: "a", Capacity: c, Prices: cheap}, {Name: "b", Capacity: c, Prices: dear}, {Name: "c", Capacity: c}} // c has no price: flat
	// Two replicas of one workload, on a and b; usage 100m/100Mi each.
	mk := func(name, node string) Pod {
		return Pod{"ns", name, "Deployment/w", node, []Container{ct("app", Resources{1000, 2 * gi}, Resources{100, 100 * mi}, true)}, false, nil, false, false, false, false, false, false, nil, false, false}
	}
	r := Build(nodes, []Pod{mk("p1", "a"), mk("p2", "b"), mk("p3", "gone")}, flat)

	near(t, "cluster", r.Monthly, cheap.Monthly(c)+dear.Monthly(c)+flat.Monthly(c))
	req := Resources{1000, 2 * gi}
	near(t, "idle", r.IdleMonthly, cheap.Monthly(Resources{3000, 6 * gi})+dear.Monthly(Resources{3000, 6 * gi})+flat.Monthly(c))
	near(t, "workload", r.Workloads[0].Monthly, cheap.Monthly(req)+dear.Monthly(req)+flat.Monthly(req))

	rec := Resources{120, 130 * mi}
	near(t, "saving", r.Recs[0].MonthlySaving,
		cheap.Monthly(req)+dear.Monthly(req)+flat.Monthly(req)-cheap.Monthly(rec)-dear.Monthly(rec)-flat.Monthly(rec))
}

// A pod on a node missing from the list pays flat prices for its request and its burst, unfitted.
func TestUnlistedNodeBurstIsFlat(t *testing.T) {
	flat := Prices{CPUHour: 0.03, GiBHour: 0.004}
	req, use := Resources{100, 64 * mi}, Resources{900, 512 * mi}
	r := Build(nil, []Pod{{"ns", "p", "Pod/p", "gone", []Container{ct("app", req, use, true)}, false, nil, false, false, false, false, false, false, nil, false, false}}, flat)
	near(t, "workload", r.Workloads[0].Monthly, flat.Monthly(use))
}

func ct(name string, req, use Resources, has bool) Container {
	return Container{Name: name, Request: req, Usage: use, HasUsage: has}
}

func TestOOMFloorAndThrottle(t *testing.T) {
	oom := Container{Name: "app", Request: Resources{500, 256 * mi}, Limit: Resources{0, 512 * mi}, Usage: Resources{100, 100 * mi}, HasUsage: true, OOMKilled: true}
	// request = limit = 180m and usage just under it: 204m is a 13% change, so only the throttle makes it a rec
	busy := Container{Name: "app", Request: Resources{180, 130 * mi}, Limit: Resources{180, 0}, Usage: Resources{170, 100 * mi}, HasUsage: true}
	r := Build(nil, []Pod{
		{Namespace: "a", Name: "o", Workload: "Pod/o", Containers: []Container{oom}},
		{Namespace: "a", Name: "b", Workload: "Pod/b", Containers: []Container{busy}},
	}, Prices{1, 1})
	if len(r.Recs) != 2 {
		t.Fatalf("want 2 recs, got %+v", r.Recs)
	}
	for _, x := range r.Recs {
		switch x.Workload {
		case "Pod/o": // killed at 512Mi: never below 512Mi*1.3, although usage says 130Mi
			if x.Recommended != (Resources{120, 666 * mi}) || !x.OOMKilled || x.Throttled || x.Limit != oom.Limit {
				t.Errorf("oom rec = %+v", x)
			}
		case "Pod/b":
			if x.Recommended != (Resources{204, 130 * mi}) || !x.Throttled || x.OOMKilled {
				t.Errorf("throttled rec = %+v", x)
			}
		}
	}
}

func TestWindow(t *testing.T) {
	var w Window
	for i := int64(1); i <= 20; i++ {
		c := ct("app", Resources{1000, gi}, Resources{i * 10, (100 - i) * mi}, true)
		c.OOMKilled = i == 3 // seen once, kept for the whole window
		w.Add(time.Time{}, []Pod{{Namespace: "ns", Name: "p", Workload: "Deployment/w", Containers: []Container{c}}})
	}
	last := []Pod{{Namespace: "ns", Name: "p2", Workload: "Deployment/w", Containers: []Container{ct("app", Resources{1000, gi}, Resources{}, false)}}}
	w.Add(time.Time{}, last) // the newest replica has no metrics yet: it still gets the workload's window
	got := w.Pods()
	c := got[0].Containers[0]
	if w.Samples != 21 || len(got) != 1 || got[0].Name != "p2" || c.Usage != (Resources{190, 99 * mi}) || !c.HasUsage || !c.OOMKilled {
		t.Fatalf("samples=%d pods=%+v (want p95 = 19th of 20 = 190m, peak 99Mi, OOM kept)", w.Samples, got)
	}
	if last[0].Containers[0].HasUsage {
		t.Fatal("Pods must not write into the sample it was given")
	}
}

// The autoscaler raises a new pod's limit to its rec (autoscale.Mutate), so the OOM floor must come from the limit the
// container was killed at: floored on each new pod's limit, the rec grew 666, 866, 1126Mi... until pods went Pending.
func TestOOMFloorIsTakenAtTheKill(t *testing.T) {
	var w Window
	pod := func(c Container) []Pod {
		return []Pod{{Namespace: "a", Name: "p", Workload: "Deployment/w", Containers: []Container{c}}}
	}
	c := Container{Name: "app", Request: Resources{100, 256 * mi}, Limit: Resources{0, 512 * mi}, Usage: Resources{100, 400 * mi}, HasUsage: true, OOMKilled: true}
	w.Add(time.Time{}, pod(c)) // one kill at 512Mi, usage 400Mi
	for i := range 6 {
		rec := Build(nil, w.Pods(), Prices{}).Sizes[0].Requests
		if rec != (Resources{120, 666 * mi}) {
			t.Fatalf("cycle %d: rec = %dMi, want a stable 666Mi (512Mi*1.3)", i, rec.MemBytes/mi)
		}
		c.Request, c.Limit.MemBytes, c.OOMKilled = rec, max(512*mi, rec.MemBytes), false // the next pod, as Mutate sizes it
		w.Add(time.Time{}, pod(c))
	}
}

func TestMemCap(t *testing.T) {
	for _, c := range []struct {
		mem, lim, want int64
		capped         bool
	}{
		{2 * gi, 512 * mi, gi, true},
		{gi, 512 * mi, gi, false},       // exactly 2x
		{4 * gi, 0, 4 * gi, false},      // no limit, no cap
		{2 * gi, 500e6, 954 * mi, true}, // "500M": 2x is 1e9 bytes, rounded up to Mi like every value
		{954 * mi, 500e6, 954 * mi, false},
		{math.MaxInt64, math.MaxInt64, math.MaxInt64, false}, // no overflow
		{64 * mi, 10 * mi, 32 * mi, true},                    // under 16Mi: never below the 32Mi minimum (#147)
		{32 * mi, 10 * mi, 32 * mi, false},
	} {
		if got, capped := MemCap(c.mem, c.lim); got != c.want || capped != c.capped {
			t.Errorf("MemCap(%d, %d) = %d, %v; want %d, %v", c.mem, c.lim, got, capped, c.want, c.capped)
		}
	}
}

// P36c: the manifest's limit is 512Mi, the webhook raised the pod to 1Gi and it was OOM-killed there, at the cap.
// The dry-run shows the 1Gi the webhook writes, at no extra cost, not OOMFloor(1Gi, 1Gi) = 1332Mi; Sizes stay uncapped
// so Mutate still reports the cap for the journal. Without a limit nothing changes.
func TestDryRunKeepsTheMemoryCap(t *testing.T) {
	for _, c := range []struct {
		name                 string
		manifest, limit, rec int64
		size                 int64
	}{
		{"manifest limit 512Mi", 512 * mi, gi, gi, 1332 * mi},
		{"older sample: the pod's limit stands in", 0, 512 * mi, gi, 1332 * mi},
		{"no limit: as before", 0, 0, 1332 * mi, 1332 * mi},
	} {
		ctr := Container{Name: "app", Request: Resources{500, gi}, Limit: Resources{0, c.limit}, Usage: Resources{100, gi},
			HasUsage: true, OOMKilled: true, ManifestMemLimit: c.manifest}
		r := Build(nil, []Pod{{Namespace: "a", Name: "p", Workload: "Deployment/w", Containers: []Container{ctr}}}, Prices{GiBHour: 1})
		if len(r.Recs) != 1 || r.Recs[0].Recommended.MemBytes != c.rec || r.Sizes[0].Requests.MemBytes != c.size {
			t.Fatalf("%s: recs %+v sizes %+v, want rec %dMi, size %dMi", c.name, r.Recs, r.Sizes, c.rec/mi, c.size/mi)
		}
		if raise := r.Recs[0].MonthlySaving < 0; raise != (c.rec > gi) {
			t.Errorf("%s: saving %.2f", c.name, r.Recs[0].MonthlySaving)
		}
	}
}

// A Deployment scaled to 0 keeps its size for the autoscaler; a Job does not, and the report still shows only today's pods.
func TestSeenKeepsScaledToZero(t *testing.T) {
	var w Window
	oom := ct("app", Resources{100, 256 * mi}, Resources{100, 100 * mi}, true)
	oom.Limit.MemBytes, oom.OOMKilled = 512*mi, true
	w.Add(time.Time{}, []Pod{
		{Namespace: "a", Name: "d", Workload: "Deployment/d", Containers: []Container{oom}},
		{Namespace: "a", Name: "j", Workload: "Job/j", Containers: []Container{ct("run", Resources{100, 256 * mi}, Resources{100, 100 * mi}, true)}},
	})
	w.Add(time.Time{}, nil) // both gone
	want := []Size{{"a", "Deployment/d", "app", Resources{120, 666 * mi}}}
	if got := Build(nil, w.Seen(), Prices{}).Sizes; !slices.Equal(got, want) || len(w.Pods()) != 0 {
		t.Fatalf("sizes = %+v, pods = %+v; want %+v and no pods", got, w.Pods(), want)
	}
}

func TestBuildStorage(t *testing.T) {
	const g = 1 << 30
	day1 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	vols := []Volume{
		{"a", "shop", "data", 100 * g, "gp3", "", time.Time{}},
		{"b", "shop", "logs", 50 * g, "gp3", "", time.Time{}},
		{"c", "db", "pg", 10 * g, "io2", "", time.Time{}},
		{"d", "shop", "tmp", 5 * g, "", "", time.Time{}}, // not EBS: counted, unpriced
		{"e", "", "", 20 * g, "gp3", "fast", day1},       // Released: unclaimed
		{"f", "", "", 1 * g, "mystery", "", time.Time{}}, // a type with no price
	}
	s := BuildStorage(vols, map[string]float64{"gp3": 0.1, "io2": 0.2})
	near := func(got, want float64) bool { return math.Abs(got-want) < 1e-9 }
	if len(s.Namespaces) != 2 || s.Namespaces[0].Namespace != "shop" || s.Namespaces[1].Namespace != "db" {
		t.Fatalf("namespaces = %+v", s.Namespaces)
	}
	if sh := s.Namespaces[0]; sh.Volumes != 3 || sh.GiB != 155 || !near(sh.Monthly, 15) || sh.Unpriced != 1 {
		t.Errorf("shop = %+v", sh)
	}
	if db := s.Namespaces[1]; !near(db.Monthly, 2) || db.Unpriced != 0 {
		t.Errorf("db = %+v", db)
	}
	if u := s.Unclaimed; u.Volumes != 2 || u.GiB != 21 || !near(u.Monthly, 2) || u.Unpriced != 1 {
		t.Errorf("unclaimed = %+v", u)
	}
	if tot := s.Total; tot.Volumes != 6 || tot.GiB != 186 || !near(tot.Monthly, 19) || tot.Unpriced != 2 {
		t.Errorf("total = %+v", tot)
	}
	if e := BuildStorage(nil, nil); e.Total.Volumes != 0 || len(e.Namespaces) != 0 {
		t.Errorf("empty = %+v", e)
	}
}

func TestBuildLoadBalancers(t *testing.T) {
	lbs := []LoadBalancer{{"a", "web", "classic"}, {"b", "web", "nlb"}, {"c", "web", "unknown"}, {"d", "db", "nlb"}}
	s := BuildLoadBalancers(lbs, map[string]float64{"classic": 0.03, "nlb": 0.027})
	// web: (0.03+0.027)*730 = 41.61, one unpriced; db: 19.71.
	if len(s.Namespaces) != 2 || s.Namespaces[0].Namespace != "web" || s.Namespaces[0].Count != 3 || s.Namespaces[0].Unpriced != 1 ||
		math.Abs(s.Namespaces[0].Monthly-41.61) > 1e-9 || math.Abs(s.Total.Monthly-61.32) > 1e-9 || s.Total.Count != 4 || s.Total.Unpriced != 1 {
		t.Fatalf("got %+v", s)
	}
	if u := BuildLoadBalancers(lbs, nil); u.Total.Monthly != 0 || u.Total.Unpriced != 4 {
		t.Fatalf("no prices = %+v", u)
	}
}

func TestFloorNeverRaisesTinyRequest(t *testing.T) {
	// 5m / 30Mi request, 1m / 10Mi usage: the 10m / 32Mi floor is above the request, so it must not become a rec (S36).
	pods := []Pod{{"a", "p", "Pod/p", "n", []Container{ct("c", Resources{5, 30 * mi}, Resources{1, 10 * mi}, true)}, false, nil, false, false, false, false, false, false, nil, false, false}}
	r := Build(nil, pods, Prices{1, 1})
	if len(r.Recs) != 0 {
		t.Fatalf("floor alone must not raise a request, got %+v", r.Recs)
	}
	if want := (Resources{5, 30 * mi}); r.Sizes[0].Requests != want {
		t.Fatalf("size = %+v, want %+v", r.Sizes[0].Requests, want)
	}
	// Real usage above the request still raises it.
	pods[0].Containers[0] = ct("c", Resources{5, 30 * mi}, Resources{50, 100 * mi}, true)
	if r = Build(nil, pods, Prices{1, 1}); len(r.Recs) != 1 || r.Recs[0].Recommended.MilliCPU < 50 {
		t.Fatalf("usage above request must still be recommended, got %+v", r.Recs)
	}
}

func TestFloorSavingNeverNegativePerReplica(t *testing.T) {
	// Replica a requests 500m / 256Mi, replica b 5m / 30Mi; both use 1m / 10Mi. The rec is 10m / 32Mi (the floor).
	// b must not show a cost for the floor: its saving is 0, a's is the full cut (S36b).
	mk := func(name string, req Resources) Pod {
		return Pod{"ns", name, "Deployment/w", "n", []Container{ct("c", req, Resources{1, 10 * mi}, true)}, false, nil, false, false, false, false, false, false, nil, false, false}
	}
	pr := Prices{1, 1}
	r := Build(nil, []Pod{mk("a", Resources{500, 256 * mi}), mk("b", Resources{5, 30 * mi})}, pr)
	if len(r.Recs) != 1 {
		t.Fatalf("recs = %+v", r.Recs)
	}
	rec := r.Recs[0]
	want := pr.Monthly(Resources{500, 256 * mi}) - pr.Monthly(Resources{10, 32 * mi})
	if rec.Recommended != (Resources{10, 32 * mi}) || math.Abs(rec.MonthlySaving-want) > 1e-9 || rec.CPUSaving < 0 {
		t.Fatalf("rec = %+v, want saving %v", rec, want)
	}
	// Real usage above a replica's request is still recommended, and its growth still counts.
	up := mk("c", Resources{5, 30 * mi})
	up.Containers[0].Usage = Resources{50, 100 * mi}
	if r = Build(nil, []Pod{up}, pr); len(r.Recs) != 1 || r.Recs[0].MonthlySaving >= 0 {
		t.Fatalf("upward rec must keep a negative saving, got %+v", r.Recs)
	}
}
