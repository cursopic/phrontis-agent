package report

import (
	"encoding/json"
	"strings"
	"testing"
)

// Spec 031: a sidecar is costed and sized, but Sizes (what the webhook applies) does not have it.
func TestBuildSidecar(t *testing.T) {
	p := Prices{CPUHour: 0.03, GiBHour: 0.004}
	app := ct("app", Resources{1000, 2 * gi}, Resources{100, 200 * mi}, true)
	mesh := ct("mesh", Resources{500, gi}, Resources{20, 50 * mi}, true)
	mesh.Sidecar, mesh.Injected = true, true
	plain := ct("mesh", Resources{500, gi}, Resources{20, 50 * mi}, true) // an old replica, from before mesh became a sidecar
	r := Build([]Node{{Name: "n1", Capacity: Resources{4000, 8 * gi}}}, []Pod{
		{Namespace: "shop", Name: "a", Workload: "Deployment/web", Node: "n1", Containers: []Container{app, mesh}},
		{Namespace: "shop", Name: "b", Workload: "Deployment/web", Node: "n1", Containers: []Container{app, plain}},
	}, p)
	near(t, "namespace", r.Namespaces[0].Monthly, p.Monthly(Resources{3000, 6 * gi}))
	near(t, "namespace+idle", r.Namespaces[0].Monthly+r.IdleMonthly, r.Monthly)
	if len(r.Sizes) != 1 || r.Sizes[0].Container != "app" {
		t.Errorf("sizes = %+v, want app only", r.Sizes)
	}
	if len(r.Recs) != 2 {
		t.Fatalf("recs = %+v", r.Recs)
	}
	for _, x := range r.Recs {
		if side := x.Container == "mesh"; x.Sidecar != side || x.Injected != side {
			t.Errorf("%s: sidecar %v injected %v", x.Container, x.Sidecar, x.Injected)
		}
	}
	// Without sidecars the report and the sample have no new key.
	for _, v := range []any{app, r.Recs[0]} {
		if x, ok := v.(Rec); ok && x.Sidecar {
			t.Fatalf("first rec is %s, want app", x.Container)
		}
		if b, _ := json.Marshal(v); strings.Contains(string(b), "Sidecar") || strings.Contains(string(b), "Injected") {
			t.Errorf("new key in %s", b)
		}
	}
}

// A busy sidecar, or one without usage data, does not change the abandoned verdict.
func TestAbandonedIgnoresSidecars(t *testing.T) {
	busy, nodata := ucont("mesh", 500, true), ucont("logs", 0, false)
	busy.Sidecar, nodata.Sidecar = true, true
	for name, cs := range map[string][]Container{"busy": {busy}, "no usage": {nodata}, "both": {busy, nodata}} {
		pods := fold(300, func(int) []Pod {
			return []Pod{apod("Deployment/idle", append([]Container{ucont("c", 1, true)}, cs...)...), apod("Deployment/busy", append([]Container{ucont("c", 50, true)}, cs...)...)}
		})
		a := BuildAbandoned(pods, lines)
		if a == nil || len(a.Workloads) != 1 || a.Workloads[0].Workload != "Deployment/idle" || a.Workloads[0].PeakCPU != 1 {
			t.Errorf("%s: %+v", name, a)
		}
	}
	// A pod of sidecars only has no main container to judge.
	if a := BuildAbandoned(fold(300, func(int) []Pod { return []Pod{apod("Deployment/idle", busy)} }), lines); a != nil && len(a.Workloads) > 0 {
		t.Errorf("sidecars only: %+v", a.Workloads)
	}
}
