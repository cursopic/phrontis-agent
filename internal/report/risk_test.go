package report

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestRisks(t *testing.T) {
	pod := func(ns, wl string, cs ...Container) Pod { return Pod{Namespace: ns, Workload: wl, Containers: cs} }
	cpu := func(usage, limit int64) Container {
		return Container{Name: "app", HasUsage: true, Request: Resources{100, 1 << 20}, Limit: Resources{limit, 0}, Usage: Resources{usage, 1 << 20}}
	}
	static := pod("kube-system", "Node/etcd", Container{Name: "etcd", OOMKilled: true, HasUsage: true, Usage: Resources{MilliCPU: 100}, Limit: Resources{MilliCPU: 100}})
	static.Static = true
	hpa := func(name string, min, now, max int) HPA {
		return HPA{Namespace: "shop", Name: name, Target: "Deployment/web", MinReplicas: min, Replicas: now, MaxReplicas: max}
	}
	web := func(c, kind, detail string) Risk { return Risk{"shop", "Deployment/web", c, kind, detail} }
	const oom, hot = "OOM-killed", "CPU at its limit, likely throttled"
	for name, tc := range map[string]struct {
		pods []Pod
		hpas []HPA
		want []Risk
	}{
		"oom without usage": {pods: []Pod{pod("shop", "Deployment/web", Container{Name: "app", OOMKilled: true})}, want: []Risk{web("app", "oom", oom)}},
		"oom in one of two replicas": {pods: []Pod{pod("shop", "Deployment/web", cpu(10, 0)), pod("shop", "Deployment/web", Container{Name: "app", OOMKilled: true})},
			want: []Risk{web("app", "oom", oom)}},
		"cpu at 90% in every replica": {pods: []Pod{pod("shop", "Deployment/web", cpu(90, 100)), pod("shop", "Deployment/web", cpu(95, 100))}, want: []Risk{web("app", "cpu", hot)}},
		"cpu just below 90%":          {pods: []Pod{pod("shop", "Deployment/web", cpu(89, 100))}},
		"cpu without a limit":         {pods: []Pod{pod("shop", "Deployment/web", cpu(900, 0))}},
		"cpu limit without usage":     {pods: []Pod{pod("shop", "Deployment/web", Container{Name: "app", Limit: Resources{MilliCPU: 100}})}},
		"sidecar":                     {pods: []Pod{pod("shop", "Deployment/web", Container{Name: "mesh", Sidecar: true, OOMKilled: true})}, want: []Risk{web("mesh", "oom", oom)}},
		"static pod":                  {pods: []Pod{static}, want: []Risk{{"kube-system", "Node/etcd", "etcd", "cpu", hot}, {"kube-system", "Node/etcd", "etcd", "oom", oom}}},
		"hpa at its maximum":          {hpas: []HPA{hpa("web", 2, 10, 10)}, want: []Risk{web("", "hpa", "HPA web at its maximum, 10 replicas")}},
		"hpa above its maximum":       {hpas: []HPA{hpa("web", 2, 12, 10)}, want: []Risk{web("", "hpa", "HPA web at its maximum, 10 replicas")}},
		"hpa below its maximum":       {hpas: []HPA{hpa("web", 2, 9, 10)}},
		"hpa of a fixed size":         {hpas: []HPA{hpa("web", 10, 10, 10)}},
		"hpa from an old agent":       {hpas: []HPA{hpa("web", 2, 10, 0)}},
		"two hpas on one workload": {hpas: []HPA{hpa("b", 1, 3, 3), hpa("a", 1, 4, 4)},
			want: []Risk{web("", "hpa", "HPA a at its maximum, 4 replicas"), web("", "hpa", "HPA b at its maximum, 3 replicas")}},
		"order": {pods: []Pod{pod("shop", "Deployment/web", Container{Name: "b", OOMKilled: true}, Container{Name: "a", OOMKilled: true}),
			pod("shop", "Deployment/api", Container{Name: "z", OOMKilled: true}), pod("data", "StatefulSet/x", Container{Name: "z", OOMKilled: true})},
			hpas: []HPA{hpa("web", 1, 2, 2)},
			want: []Risk{{"data", "StatefulSet/x", "z", "oom", oom}, {"shop", "Deployment/api", "z", "oom", oom},
				web("", "hpa", "HPA web at its maximum, 2 replicas"), web("a", "oom", oom), web("b", "oom", oom)}},
	} {
		if got := Risks(tc.pods, tc.hpas); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %+v, want %+v", name, got, tc.want)
		}
	}
}

// The list and the recommendation agree on throttling, and a right size does not hide a risk.
func TestRisksAgreeWithRecs(t *testing.T) {
	c := Container{Name: "app", HasUsage: true, OOMKilled: true, Request: Resources{100, 256 << 20}, Limit: Resources{0, 512 << 20}, Usage: Resources{80, 200 << 20}}
	for _, tc := range []struct {
		limit     int64
		throttled bool
	}{{0, false}, {100, false}, {88, true}} { // 80 is 90.9% of 88
		c.Limit.MilliCPU = tc.limit
		pods := []Pod{{Namespace: "shop", Name: "web-1", Workload: "Deployment/web", Containers: []Container{c}}}
		recs, risks := Build(nil, pods, Prices{}).Recs, Risks(pods, nil)
		if got := slices.ContainsFunc(recs, func(r Rec) bool { return r.Throttled }); got != tc.throttled {
			t.Errorf("limit %d: Rec.Throttled %v", tc.limit, got)
		}
		if got := slices.ContainsFunc(risks, func(r Risk) bool { return r.Kind == "cpu" }); got != tc.throttled {
			t.Errorf("limit %d: cpu risk %v", tc.limit, got)
		}
		if !slices.ContainsFunc(risks, func(r Risk) bool { return r.Kind == "oom" }) {
			t.Errorf("limit %d: no oom risk", tc.limit)
		}
	}
	// No usage: no recommendation, still a risk.
	pods := []Pod{{Namespace: "shop", Workload: "Deployment/web", Containers: []Container{{Name: "app", OOMKilled: true}}}}
	if recs := Build(nil, pods, Prices{}).Recs; len(recs) > 0 || len(Risks(pods, nil)) != 1 {
		t.Errorf("recs %+v, risks %+v", recs, Risks(pods, nil))
	}
}

// A sample stored by an older agent has no MaxReplicas; a report without risks has no Risks key.
func TestRisksOldSampleAndJSON(t *testing.T) {
	var h HPA
	if err := json.Unmarshal([]byte(`{"Namespace":"shop","Name":"web","Target":"Deployment/web","MinReplicas":2,"Replicas":10}`), &h); err != nil {
		t.Fatal(err)
	}
	if got := Risks(nil, []HPA{h}); got != nil {
		t.Errorf("old sample: %+v", got)
	}
	b, err := json.Marshal(Report{Risks: Risks(nil, nil)})
	if err != nil || strings.Contains(string(b), "Risks") {
		t.Errorf("%s, %v", b, err)
	}
	h.MaxReplicas = 10
	var back HPA
	if b, err = json.Marshal(h); err == nil {
		err = json.Unmarshal(b, &back)
	}
	if err != nil || !reflect.DeepEqual(back, h) {
		t.Errorf("%+v, %v", back, err)
	}
}
