package report

import (
	"slices"
	"testing"
)

func TestUnevictableReasons(t *testing.T) {
	nodes := []Node{cnode("a"), cnode("b")}
	mk := func(name, wl string, f func(*Pod)) Pod {
		p := cpod(name, "a", wl, 100, 1)
		f(&p)
		return p
	}
	pods := []Pod{
		mk("pdb", "Deployment/pdb", func(*Pod) {}),
		mk("ann", "Deployment/ann", func(p *Pod) { p.NoEvict = true }),
		mk("hp", "Deployment/hp", func(p *Pod) { p.HostPath = true }),
		mk("ed", "Deployment/ed", func(p *Pod) { p.EmptyDir = true }),
		mk("own", "Pod/own", func(*Pod) {}),
		mk("free", "Deployment/free", func(*Pod) {}),
		mk("ds", "DaemonSet/ds", func(p *Pod) { p.HostPath = true }),
		mk("noread", "Deployment/noread", func(p *Pod) { p.NoEvict, p.EvictRead = true, false }),
	}
	pdbs := []PDB{{Namespace: "ns", Name: "x", Workloads: []string{"Deployment/pdb"}, Strict: true}, {Namespace: "ns", Name: "y", Workloads: []string{"Deployment/free"}}}
	u := BuildUnevictable(nodes, pods, pdbs)
	want := map[string]string{"pdb": "PodDisruptionBudget allows 0 disruptions", "ann": "safe-to-evict is false", "hp": "local storage (hostPath)", "ed": "local storage (emptyDir)", "own": "no controller"}
	if len(u.Pods) != len(want) {
		t.Fatalf("want %d pods, got %+v", len(want), u.Pods)
	}
	for _, p := range u.Pods {
		if !slices.Equal(p.Reasons, []string{want[p.Pod]}) {
			t.Errorf("%s: %v", p.Pod, p.Reasons)
		}
		if m := (4*0.02 + 16*0.005) * 730; p.Monthly != m {
			t.Errorf("%s pins %v, want %v", p.Pod, p.Monthly, m)
		}
	}
	if u.Nodes != 1 || u.Monthly != u.Pods[0].Monthly {
		t.Errorf("one node counted once: %+v", u)
	}
}

func TestUnevictableNoneAndTwoReasons(t *testing.T) {
	if u := BuildUnevictable([]Node{cnode("a")}, []Pod{cpod("p", "a", "Deployment/p", 100, 1)}, nil); u != nil {
		t.Errorf("a pod with no reason: %+v", u)
	}
	p := cpod("p", "a", "Pod/p", 100, 1)
	p.HostPath = true
	if u := BuildUnevictable([]Node{cnode("a")}, []Pod{p}, nil); len(u.Pods[0].Reasons) != 2 {
		t.Errorf("both reasons: %+v", u)
	}
}

func TestUnevictableSafeToEvictAndMissingNode(t *testing.T) {
	ok := cpod("ok", "a", "Deployment/ok", 100, 1)
	ok.EmptyDir, ok.HostPath, ok.SafeEvict = true, true, true
	old := ok // an old agent cannot say: still flagged
	old.Name, old.EvictRead = "old", false
	lost := cpod("lost", "gone", "Pod/lost", 100, 1)
	u := BuildUnevictable([]Node{cnode("a")}, []Pod{ok, old, lost}, nil)
	if len(u.Pods) != 2 || u.Pods[0].Pod != "old" && u.Pods[1].Pod != "old" {
		t.Fatalf("safe-to-evict=true lifts local storage: %+v", u.Pods)
	}
	if u.Nodes != 1 || u.Monthly != (4*0.02+16*0.005)*730 {
		t.Errorf("the unknown node costs 0 and is not a pinned node: %+v", u)
	}
}
