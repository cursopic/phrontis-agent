package report

import (
	"math"
	"testing"
)

func TestHPATargetAdvice(t *testing.T) {
	rec := func() Rec {
		return Rec{Namespace: "a", Workload: "Deployment/api", Container: "app", Request: Resources{MilliCPU: 1000, MemBytes: 1 << 30}, Recommended: Resources{MilliCPU: 400, MemBytes: 512 << 20},
			MonthlySaving: 50, CPUCost: 210, MemCost: 90, CPUSaving: 30}
	}
	hpa := func(h HPA) HPA { h.Namespace, h.Name, h.Target = "a", "api", "Deployment/api"; return h }
	tests := []struct {
		name    string
		h       HPA
		advice  int
		saving  float64
		cpuKept bool
		memKept bool
	}{
		{"target 50 replicas above min", hpa(HPA{CPUTarget: 50, MinReplicas: 2, Replicas: 6}), 1, 50 - 30 + 210*(1-50.0/70), true, false},
		{"replicas at min", hpa(HPA{CPUTarget: 50, MinReplicas: 2, Replicas: 2}), 0, 50, false, false},
		{"target 65", hpa(HPA{CPUTarget: 65, MinReplicas: 2, Replicas: 6}), 0, 50, false, false},
		{"averageValue: no target", hpa(HPA{Resources: []string{"cpu"}, MinReplicas: 2, Replicas: 6}), 0, 50, false, false},
		{"old agent", hpa(HPA{Resources: []string{"cpu"}}), 0, 50, false, false},
		{"container target, same container", hpa(HPA{CPUTarget: 50, CPUContainer: "app", MinReplicas: 2, Replicas: 6}), 1, 50 - 30 + 210*(1-50.0/70), true, false},
		{"container target, other container", hpa(HPA{CPUTarget: 50, CPUContainer: "side", MinReplicas: 2, Replicas: 6}), 0, 50, false, false},
		{"memory target 40", hpa(HPA{MemTarget: 40, MinReplicas: 1, Replicas: 3}), 1, 30 + 90*(1-40.0/70), false, true},
	}
	for _, tc := range tests {
		recs := []Rec{rec()}
		MarkHPA(recs, []HPA{tc.h})
		r := recs[0]
		if r.HPA == nil || len(r.HPA.Advice) != tc.advice || math.Abs(r.MonthlySaving-tc.saving) > 1e-9 {
			t.Errorf("%s: advice %+v saving %v, want %d / %v", tc.name, r.HPA, r.MonthlySaving, tc.advice, tc.saving)
		}
		if got := r.Recommended.MilliCPU == 1000; got != tc.cpuKept {
			t.Errorf("%s: CPU request kept = %v", tc.name, got)
		}
		if got := r.Recommended.MemBytes == 1<<30; got != tc.memKept {
			t.Errorf("%s: memory request kept = %v", tc.name, got)
		}
	}
	if got := (TargetAdvice{Resource: "cpu", From: 50, Saving: 120.4}).Text("api"); got != "Raise HPA api CPU target 50% → 70%: saves ~$120/mo" {
		t.Errorf("text = %q", got)
	}
}

// The advice never lowers the saving: a request change that saves more than raising the target stays as it is.
func TestHPAAdviceKeepsBiggerRequestSaving(t *testing.T) {
	recs := []Rec{{Namespace: "a", Workload: "Deployment/api", Request: Resources{MilliCPU: 1000}, Recommended: Resources{MilliCPU: 100},
		MonthlySaving: 100, CPUCost: 210, CPUSaving: 100}}
	MarkHPA(recs, []HPA{{Namespace: "a", Name: "api", Target: "Deployment/api", CPUTarget: 50, MinReplicas: 1, Replicas: 3}})
	r := recs[0]
	if len(r.HPA.Advice) != 0 || r.MonthlySaving != 100 || r.Recommended.MilliCPU != 100 {
		t.Errorf("%+v", r)
	}
}
