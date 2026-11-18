package report

import (
	"math"
	"reflect"
	"testing"
)

func TestSpotSafety(t *testing.T) {
	dep := func(name string, n int, pvcs ...string) []Pod {
		var ps []Pod
		for i := 0; i < n; i++ {
			ps = append(ps, Pod{Namespace: "shop", Workload: name, PVCs: pvcs})
		}
		return ps
	}
	cat := func(ps ...[]Pod) (all []Pod) {
		for _, p := range ps {
			all = append(all, p...)
		}
		return
	}
	pdbs := []PDB{{Namespace: "shop", Name: "p", Workloads: []string{"Deployment/web", "Deployment/one", "StatefulSet/db", "Deployment/data"}},
		{Namespace: "other", Name: "p", Workloads: []string{"Deployment/nopdb"}}}
	tests := []struct {
		name string
		pods []Pod
		safe bool
		why  string
	}{
		{"deployment 2 replicas with PDB", dep("Deployment/web", 2), true, "2 replicas with a PodDisruptionBudget"},
		{"statefulset", dep("StatefulSet/db", 3), false, "StatefulSet keeps state and a stable identity"},
		{"one replica", dep("Deployment/one", 1), false, "runs a single replica, so a spot interruption stops it"},
		{"pvc", dep("Deployment/data", 3, "data"), false, "mounts the volume claim data"},
		{"no pdb", dep("Deployment/nopdb", 2), false, "2 replicas but no PodDisruptionBudget to keep some running"},
		{"pdb in another namespace", dep("Deployment/nopdb", 2), false, "2 replicas but no PodDisruptionBudget to keep some running"},
		{"daemonset", dep("DaemonSet/agent", 4), false, "DaemonSet is not a Deployment"},
		{"bare pod", dep("Pod/x", 1), false, "Pod is not a Deployment"},
	}
	for _, tc := range tests {
		got := SpotSafety(tc.pods, pdbs)
		if len(got) != 1 || got[0].Safe != tc.safe || got[0].Reason != tc.why {
			t.Errorf("%s: got %+v", tc.name, got)
		}
	}
	// the same PDB name matches only inside its own namespace; static pods are skipped
	got := SpotSafety(append(cat(dep("Deployment/web", 2)), Pod{Namespace: "other", Workload: "Deployment/nopdb"}, Pod{Namespace: "other", Workload: "Deployment/nopdb"}, Pod{Namespace: "kube", Workload: "Pod/s", Static: true}), pdbs)
	want := []SpotVerdict{{"other", "Deployment/nopdb", true, "2 replicas with a PodDisruptionBudget"}, {"shop", "Deployment/web", true, "2 replicas with a PodDisruptionBudget"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestSpotSavings(t *testing.T) {
	verdicts := []SpotVerdict{
		{Namespace: "a", Workload: "Deployment/web", Safe: true},
		{Namespace: "a", Workload: "Deployment/api", Safe: true},
		{Namespace: "a", Workload: "StatefulSet/db"}, // not safe: never listed
		{Namespace: "a", Workload: "Deployment/nocost", Safe: true},
	}
	lines := []Line{{Namespace: "a", Workload: "Deployment/web", Monthly: 100}, {Namespace: "a", Workload: "Deployment/api", Monthly: 40},
		{Namespace: "a", Workload: "StatefulSet/db", Monthly: 500}}
	pod := func(wl, node string) Pod { return Pod{Namespace: "a", Workload: wl, Node: node} }
	tests := []struct {
		name     string
		pods     []Pod
		discount map[string]float64
		want     []SpotLine
	}{
		{"two nodes, cost split by pod", []Pod{pod("Deployment/web", "n1"), pod("Deployment/web", "n2"), pod("Deployment/api", "n1"), pod("Deployment/api", "n1")},
			map[string]float64{"n1": 0.6, "n2": 0.2},
			[]SpotLine{{"a", "Deployment/web", 2, 100, 40}, {"a", "Deployment/api", 2, 40, 24}}},
		{"node without a spot price saves nothing", []Pod{pod("Deployment/web", "n1"), pod("Deployment/web", "flat")}, map[string]float64{"n1": 0.5},
			[]SpotLine{{"a", "Deployment/web", 2, 100, 25}}},
		{"no discounts: listed, saving 0", []Pod{pod("Deployment/api", "n1")}, nil, []SpotLine{{"a", "Deployment/api", 1, 40, 0}}},
		{"static pods are skipped", []Pod{{Namespace: "a", Workload: "Deployment/web", Node: "n1", Static: true}}, map[string]float64{"n1": 0.5}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SpotSavings(verdicts, lines, tt.pods, tt.discount)
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i, w := range tt.want {
				g := got[i]
				if g.Workload != w.Workload || g.Pods != w.Pods || g.Monthly != w.Monthly || math.Abs(g.Saving-w.Saving) > 1e-9 {
					t.Errorf("line %d: got %+v, want %+v", i, g, w)
				}
			}
		})
	}
}
