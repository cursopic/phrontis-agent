package cli

import (
	"bytes"
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/cursopic/phrontis/internal/pricing"
	"github.com/cursopic/phrontis/internal/report"
)

// spotFake is a pricer with on-demand 0.10/h for every type and a spot price (or none) from spot.
type spotFake struct{ spot pricing.SpotPrice }

func (spotFake) Hourly(context.Context, pricing.Key) (float64, string, error) {
	return 0.10, "aws on-demand", nil
}
func (f spotFake) SpotHourly(context.Context, pricing.Key) pricing.SpotPrice { return f.spot }
func (spotFake) EBSMonthly(context.Context, string, string) (float64, error) {
	return 0, errors.New("unused")
}
func (spotFake) ELBHourly(context.Context, string) (map[string]float64, error) { return nil, nil }
func (spotFake) EC2Types(context.Context, string) (map[string]pricing.Type, error) {
	return nil, nil
}

func TestBuildSpot(t *testing.T) {
	pod := func(n string) report.Pod { return report.Pod{Namespace: "a", Workload: "Deployment/web", Node: n} }
	s := snapshot{
		pods: []report.Pod{pod("n1"), pod("n1"), {Namespace: "a", Workload: "Deployment/solo", Node: "n1"}},
		pdbs: []report.PDB{{Namespace: "a", Workloads: []string{"Deployment/web"}}},
		keys: map[string]pricing.Key{"n1": {Provider: "aws", Region: "us-east-1", Type: "m5.large"}},
	}
	r := report.Report{Workloads: []report.Line{{Namespace: "a", Workload: "Deployment/web", Pods: 2, Monthly: 100}}}
	at := pricing.SpotPrice{Hourly: 0.04}
	tests := []struct {
		name   string
		pr     pricer
		priced bool
		saving float64
		note   string
	}{
		{"priced: 60% off", spotFake{at}, true, 60, "spot (highest zone), price at"},
		{"spot lookup failed", spotFake{pricing.SpotPrice{Reason: "spot price lookup failed"}}, false, 0, "spot price lookup failed"},
		{"cloud prices off", nil, false, 0, "cloud prices are off"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildSpot(context.Background(), tt.pr, s, r)
			if got == nil || got.Priced != tt.priced || math.Abs(got.Saving-tt.saving) > 1e-9 || !strings.Contains(got.Note, tt.note) || len(got.Workloads) != 1 || got.Kept != 1 {
				t.Fatalf("got %+v", got)
			}
			var b bytes.Buffer
			renderSpot(&b, got)
			if tt.priced != strings.Contains(b.String(), "saves 60.00") {
				t.Errorf("render shows money wrongly:\n%s", b.String())
			}
		})
	}
	if buildSpot(context.Background(), nil, snapshot{}, report.Report{}) != nil {
		t.Error("no pods: want no section")
	}
}
