package report

import (
	"reflect"
	"slices"
	"testing"
)

// np builds a pod of one container "c" whose request is cpu millicores and 1 Mi of memory (memory never binds here).
func np(wl string, cpu int64) Pod {
	return Pod{Namespace: "ns", Name: wl, Workload: wl, Containers: []Container{{Name: "c", Request: Resources{cpu, 1 << 20}}}}
}

// off is an offer with cpu millicores and 100 GiB; 10% reserve leaves 90% of the CPU.
func off(typ, fam string, cpu int64, hourly float64) Offer {
	return Offer{typ, fam, Resources{cpu, 100 * gi}, hourly}
}

func TestNodePoolFirstFitDecreasing(t *testing.T) {
	// 10-core nodes leave 9000m: {5,4} {4,3,1} {3} = 3 nodes (20 cores cannot fit in 2).
	var pods []Pod
	for i, c := range []int64{1000, 3000, 4000, 5000, 3000, 4000} {
		pods = append(pods, np("StatefulSet/s"+string(rune('a'+i)), c))
	}
	got := BuildNodePool(pods, nil, []Offer{off("m.big", "m", 10000, 1)}, 123)
	if got.Nodes != 3 || got.Type != "m.big" || got.Monthly != 3*HoursPerMonth || got.Current != 123 || got.SpotShare != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestNodePoolDaemonSetReserve(t *testing.T) {
	pods := []Pod{np("StatefulSet/a", 4500), np("StatefulSet/b", 4500)}
	o := []Offer{off("m.big", "m", 10000, 1)}
	if n := BuildNodePool(pods, nil, o, 0).Nodes; n != 1 {
		t.Fatalf("no daemonset: %d nodes", n)
	}
	// The reserve is the largest pod per DaemonSet, not the sum of its pods; static pods are ignored.
	ds := np("DaemonSet/log", 1000)
	small := np("DaemonSet/log", 200)
	static := np("Pod/kube", 9000)
	static.Static = true
	if n := BuildNodePool(append(pods, ds, small, static), nil, o, 0).Nodes; n != 2 {
		t.Fatalf("with daemonset reserve: %d nodes, want 2", n)
	}
}

func TestNodePoolAppliesRecs(t *testing.T) {
	pods := []Pod{np("StatefulSet/a", 8000), np("StatefulSet/b", 8000)}
	o := []Offer{off("m.big", "m", 10000, 1)}
	if n := BuildNodePool(pods, nil, o, 0).Nodes; n != 2 {
		t.Fatalf("before: %d nodes", n)
	}
	recs := []Rec{{Namespace: "ns", Workload: "StatefulSet/a", Container: "c", Recommended: Resources{4000, 1 << 20}},
		{Namespace: "ns", Workload: "StatefulSet/b", Container: "c", Recommended: Resources{4000, 1 << 20}}}
	if n := BuildNodePool(pods, recs, o, 0).Nodes; n != 1 {
		t.Fatalf("after recs: %d nodes, want 1", n)
	}
	// A rec for another container or workload changes nothing.
	other := []Rec{{Namespace: "ns", Workload: "StatefulSet/a", Container: "x", Recommended: Resources{1, 1}}}
	if n := BuildNodePool(pods, other, o, 0).Nodes; n != 2 {
		t.Fatalf("unrelated rec: %d nodes", n)
	}
}

func TestNodePoolSpotSplit(t *testing.T) {
	pods := []Pod{np("Deployment/a", 1000), np("Deployment/a", 1000), np("Deployment/b", 1000), np("StatefulSet/c", 1000), np("Job/d", 1000)}
	got := BuildNodePool(pods, nil, []Offer{off("m.big", "m", 10000, 1)}, 0)
	// The spot pods (a x2) and the on-demand pods (b, c, d) never share a node.
	if got.Nodes != 2 || got.SpotShare != 0.5 {
		t.Fatalf("got %+v", got)
	}
	if want := []string{"ns/Deployment/b", "ns/Job/d", "ns/StatefulSet/c"}; !slices.Equal(got.OnDemand, want) {
		t.Fatalf("OnDemand = %v, want %v", got.OnDemand, want)
	}
	// A DaemonSet is neither packed nor pinned.
	if got := BuildNodePool(append(pods, np("DaemonSet/x", 100)), nil, []Offer{off("m.big", "m", 10000, 1)}, 0); len(got.OnDemand) != 3 {
		t.Fatalf("daemonset pinned: %v", got.OnDemand)
	}
}

func TestNodePoolSkipsOfferThatCannotFitAPod(t *testing.T) {
	// The cheap small type cannot hold the 5-core pod, so the dearer big type wins.
	pods := []Pod{np("StatefulSet/a", 5000), np("StatefulSet/b", 500)}
	got := BuildNodePool(pods, nil, []Offer{off("c.small", "c", 4000, 0.01), off("m.big", "m", 10000, 1)}, 0)
	if got.Type != "m.big" || got.Nodes != 1 || !slices.Equal(got.Families, []string{"m"}) {
		t.Fatalf("got %+v", got)
	}
}

func TestNodePoolUnplaced(t *testing.T) {
	pods := []Pod{np("StatefulSet/huge", 50000), np("StatefulSet/a", 1000)}
	got := BuildNodePool(pods, nil, []Offer{off("m.big", "m", 10000, 1)}, 0)
	if got.Unplaced != 1 || got.Nodes != 1 || got.Type != "m.big" || !slices.Equal(got.OnDemand, []string{"ns/StatefulSet/a"}) {
		t.Fatalf("got %+v (an unplaced workload is not pinned)", got)
	}
	if got := BuildNodePool(pods[:1], nil, []Offer{off("m.big", "m", 10000, 1)}, 0); got.Unplaced != 1 || got.Type != "" {
		t.Fatalf("nothing placeable: %+v", got)
	}
}

func TestNodePoolFamilies(t *testing.T) {
	pods := []Pod{np("StatefulSet/a", 1000)}
	var offers []Offer
	for i, f := range []string{"m6", "c6", "m5", "r6", "c5", "r5", "m7"} {
		offers = append(offers, off(f+".large", f, 10000, float64(i+1)))
		offers = append(offers, off(f+".xlarge", f, 20000, float64(i+1)*1.5)) // same family, dearer: deduped
	}
	got := BuildNodePool(pods, nil, offers, 0)
	if want := []string{"m6", "c6", "m5", "r6", "c5"}; !slices.Equal(got.Families, want) || got.Type != "m6.large" {
		t.Fatalf("got %+v", got)
	}
	// Equal cost: fewer nodes, then type name.
	tie := BuildNodePool([]Pod{np("StatefulSet/a", 1000), np("StatefulSet/b", 9000)},
		nil, []Offer{off("b.x", "b", 10000, 2), off("a.x", "a", 10000, 2), off("z.big", "z", 20000, 5)}, 0)
	if tie.Type != "a.x" || tie.Nodes != 2 || !slices.Equal(tie.Families, []string{"a", "b", "z"}) {
		t.Fatalf("tie %+v", tie)
	}
}

func TestNodePoolNoOffers(t *testing.T) {
	if got := BuildNodePool([]Pod{np("StatefulSet/a", 1000)}, nil, nil, 5); !reflect.DeepEqual(got, NodePool{}) {
		t.Fatalf("got %+v", got)
	}
}
