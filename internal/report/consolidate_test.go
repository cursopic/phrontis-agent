package report

import (
	"slices"
	"testing"
)

func cnode(name string) Node {
	return Node{Name: name, Capacity: Resources{4000, 16 << 30}, Prices: Prices{CPUHour: 0.02, GiBHour: 0.005}}
}

// cpod requests milli CPU and gi GiB; wl is "Kind/name".
func cpod(name, node, wl string, milli, gi int64) Pod {
	return Pod{Namespace: "ns", Name: name, Workload: wl, Node: node, EvictRead: true,
		Containers: []Container{{Name: "c", Request: Resources{milli, gi << 30}}}}
}

func verdict(t *testing.T, vs []NodeVerdict, node string) NodeVerdict {
	t.Helper()
	for _, v := range vs {
		if v.Node == node {
			return v
		}
	}
	t.Fatalf("no verdict for %s in %+v", node, vs)
	return NodeVerdict{}
}

func TestConsolidateEmptyNodeAndSaving(t *testing.T) {
	nodes := []Node{cnode("a"), cnode("b")}
	pods := []Pod{cpod("big", "b", "Deployment/big", 3000, 12), cpod("x", "a", "Deployment/x", 500, 1)}
	vs := Consolidate(nodes, pods, nil)
	if len(vs) != 1 {
		t.Fatalf("only a is a candidate (b is at 75%%): %+v", vs)
	}
	v := vs[0]
	if !v.Empty || !slices.Equal(v.To, []string{"b"}) {
		t.Errorf("a should go to b: %+v", v)
	}
	if want := (4*0.02 + 16*0.005) * 730; v.Monthly != want {
		t.Errorf("monthly %v, want %v", v.Monthly, want)
	}
}

func TestConsolidateBlockers(t *testing.T) {
	for _, c := range []struct {
		name   string
		mod    func(*Pod)
		reason string
	}{
		{"statefulset", func(p *Pod) { p.Workload = "StatefulSet/db" }, "runs StatefulSet/db"},
		{"claim", func(p *Pod) { p.PVCs = []string{"data-0"} }, "mounts claim data-0"},
		{"no controller", func(p *Pod) { p.Workload = "Pod/x" }, "runs pod x without a controller"},
		{"static", func(p *Pod) { p.Static = true }, "runs static pod x"},
		{"annotation", func(p *Pod) { p.NoEvict = true }, "pod has safe-to-evict=false"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := cpod("x", "a", "Deployment/x", 500, 1)
			c.mod(&p)
			v := verdict(t, Consolidate([]Node{cnode("a"), cnode("b")}, []Pod{p}, nil), "a")
			if v.Empty || v.Reason != c.reason {
				t.Errorf("got %+v, want reason %q", v, c.reason)
			}
		})
	}
}

func TestConsolidatePodsDoNotFit(t *testing.T) {
	// b is at 87% CPU, so only 500m is free there; a's pod needs 1000m.
	nodes := []Node{cnode("a"), cnode("b")}
	pods := []Pod{cpod("p", "a", "Deployment/p", 1000, 1), cpod("full", "b", "Deployment/full", 3500, 1)}
	v := verdict(t, Consolidate(nodes, pods, nil), "a")
	if v.Empty || v.Reason != "pod p does not fit on the other nodes" {
		t.Errorf("got %+v", v)
	}
}

func TestConsolidateTwoNodesCannotBothGo(t *testing.T) {
	nodes := []Node{cnode("a"), cnode("b")}
	pods := []Pod{cpod("p", "a", "Deployment/p", 100, 1), cpod("q", "b", "Deployment/q", 200, 1)}
	vs := Consolidate(nodes, pods, nil)
	if len(vs) != 2 || !vs[0].Empty || vs[0].Node != "a" || vs[1].Empty {
		t.Fatalf("a goes, b stays: %+v", vs)
	}
	if bc := BuildConsolidation(nodes, pods, nil); bc.Count != 1 {
		t.Errorf("count %d", bc.Count)
	}
}

func TestConsolidateDaemonSetNeitherMovesNorBlocks(t *testing.T) {
	ds := cpod("ds-a", "a", "DaemonSet/agent", 100, 1)
	ds.Static, ds.PVCs = true, []string{"x"} // would block if it counted
	v := verdict(t, Consolidate([]Node{cnode("a"), cnode("b")}, []Pod{ds, cpod("p", "a", "Deployment/p", 100, 1), cpod("q", "b", "Deployment/q", 2400, 1)}, nil), "a")
	if !v.Empty {
		t.Errorf("DaemonSet pod must not block: %+v", v)
	}
}

func TestConsolidateOldAgentAndSingleNode(t *testing.T) {
	p := cpod("p", "a", "Deployment/p", 100, 1)
	p.EvictRead = false
	if c := BuildConsolidation([]Node{cnode("a"), cnode("b")}, []Pod{p}, nil); !c.OldAgent || c.Count != 1 {
		t.Errorf("old agent: %+v", c)
	}
	if c := BuildConsolidation([]Node{cnode("a")}, []Pod{p}, nil); c != nil {
		t.Errorf("one node: no block, got %+v", c)
	}
}

func TestConsolidateSkipsCordonedAndZeroCapacityTargets(t *testing.T) {
	for name, mod := range map[string]func(*Node){
		"cordoned":      func(n *Node) { n.Unschedulable = true },
		"zero capacity": func(n *Node) { n.Capacity = Resources{} },
	} {
		a, b, c := cnode("a"), cnode("b"), cnode("c")
		mod(&b)
		// b holds a pod so it is not emptied first; zero requests fit even zero capacity,
		// so only the target filter keeps pods off b.
		pods := []Pod{cpod("pa", "a", "Deployment/web", 0, 0), cpod("pb", "b", "Deployment/db", 0, 0),
			cpod("pc", "c", "Deployment/api", 3000, 8)}
		for _, v := range Consolidate([]Node{a, b, c}, pods, nil) {
			for _, to := range v.To {
				if to == "b" {
					t.Errorf("%s: %s moved pods to b: %+v", name, v.Node, v)
				}
			}
		}
	}
}
