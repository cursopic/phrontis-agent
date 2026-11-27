package cli

import (
	"bytes"
	"context"
	"io"
	"math"
	"slices"
	"testing"

	"github.com/cursopic/phrontis/internal/pricing"
	"github.com/cursopic/phrontis/internal/report"
)

// S23: a family discount beats a cloud discount, other nodes keep list price, the labels name what applied.
func TestAdjustedPricer(t *testing.T) {
	c := report.Resources{MilliCPU: 2000, MemBytes: 8 << 30}
	nodes := []report.Node{{Name: "a", Capacity: c}, {Name: "b", Capacity: c}, {Name: "c", Capacity: c}}
	keys := map[string]pricing.Key{
		"a": {Provider: "aws", Region: "r", Type: "m5.large"},
		"b": {Provider: "aws", Region: "r", Type: "c5.large"},
		"c": {Provider: "gcp", Region: "r", Type: "n2-standard-2"},
	}
	flat := report.Prices{CPUHour: 0.03, GiBHour: 0.004}
	pr := NewAdjustedPricer(&fakePricer{calls: map[pricing.Key]int{}}, []pricing.Override{{Scope: "cloud", Name: "aws", Percent: 10}, {Scope: "family", Name: "m5", Percent: 25}})
	np := priceNodes(context.Background(), pr, nodes, keys, flat, io.Discard)
	for i, want := range []float64{0.15, 0.18, 0.2} { // 0.2 list; m5 -25%, aws -10%, gcp untouched
		if np[i].Hourly != want {
			t.Errorf("node %s hourly = %v, want %v", np[i].Node, np[i].Hourly, want)
		}
	}
	if got, want := pr.(*adjusted).labels(), []string{"aws -10%", "m5 -25%"}; !slices.Equal(got, want) {
		t.Errorf("labels = %v, want %v", got, want)
	}
}

// Default off: no override returns the very same pricer, and nil stays nil.
func TestAdjustedPricerOff(t *testing.T) {
	f := &fakePricer{calls: map[pricing.Key]int{}}
	if NewAdjustedPricer(f, nil) != Pricer(f) {
		t.Error("no overrides must not wrap the pricer")
	}
	if NewAdjustedPricer(nil, []pricing.Override{{Scope: "cloud", Name: "aws", Percent: 10}}) != nil {
		t.Error("no pricer must stay nil")
	}
}

// S23: an override keeps the spot section, and the spot saving equals the unadjusted one.
func TestAdjustedSpot(t *testing.T) {
	s := snapshot{
		pods: []report.Pod{{Namespace: "a", Workload: "Deployment/web", Node: "n1"}, {Namespace: "a", Workload: "Deployment/web", Node: "n1"}},
		pdbs: []report.PDB{{Namespace: "a", Workloads: []string{"Deployment/web"}}},
		keys: map[string]pricing.Key{"n1": {Provider: "aws", Region: "us-east-1", Type: "m5.large"}},
	}
	r := report.Report{Workloads: []report.Line{{Namespace: "a", Workload: "Deployment/web", Pods: 2, Monthly: 100}}}
	base := spotFake{pricing.SpotPrice{Hourly: 0.04}}
	want := buildSpot(context.Background(), base, s, r)
	got := buildSpot(context.Background(), NewAdjustedPricer(base, []pricing.Override{{Scope: "family", Name: "m5", Percent: 25}}), s, r)
	if !got.Priced || len(got.Workloads) != 1 || got.Saving <= 0 || math.Abs(got.Saving-want.Saving) > 1e-6 {
		t.Fatalf("adjusted spot %+v, unadjusted %+v", got, want)
	}
	var a, b bytes.Buffer
	renderSpot(&a, buildSpot(context.Background(), NewAdjustedPricer(base, nil), s, r))
	renderSpot(&b, want)
	if a.String() != b.String() {
		t.Error("no override must print the same spot section")
	}
}
