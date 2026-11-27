package manifest

import (
	"testing"

	"github.com/cursopic/phrontis/internal/report"
)

const Mi = 1 << 20

func rec(workload string, mem int64) report.Rec {
	return report.Rec{Namespace: "shop", Workload: workload, Container: "api",
		Request: report.Resources{MilliCPU: 500, MemBytes: 2048 * Mi}, Recommended: report.Resources{MilliCPU: 120, MemBytes: mem}}
}

// The same values as web/test/report.test.ts pins for patch() and prSteps(): exact Mi, limits raised only when exceeded.
func TestValuesMatchWebPatch(t *testing.T) {
	v, ok := ValuesFor(rec("Deployment/api", 1536*Mi))
	if !ok || v != (Values{CPU: "120m", Memory: "1536Mi"}) {
		t.Fatalf("deployment = %+v %v", v, ok)
	}
	if v, _ := ValuesFor(rec("StatefulSet/db", 300*Mi+1)); v.Memory != "301Mi" {
		t.Fatalf("round up = %q", v.Memory)
	}
	x := rec("Deployment/api", 666*Mi)
	x.Recommended.MilliCPU, x.Limit = 204, report.Resources{MilliCPU: 180, MemBytes: 4096 * Mi}
	if v, _ := ValuesFor(x); v != (Values{CPU: "204m", Memory: "666Mi", LimitCPU: true}) {
		t.Fatalf("cpu limit = %+v", v)
	}
	x.Limit = report.Resources{MilliCPU: 0, MemBytes: 512 * Mi} // no CPU limit: none added
	if v, _ := ValuesFor(x); v != (Values{CPU: "204m", Memory: "666Mi", LimitMemory: true}) {
		t.Fatalf("mem limit = %+v", v)
	}
	x.Limit = report.Resources{MilliCPU: 180, MemBytes: 512 * Mi}
	if v, _ := ValuesFor(x); !v.LimitCPU || !v.LimitMemory {
		t.Fatalf("both limits = %+v", v)
	}
	for _, w := range []string{"Pod/x", "Job/x", "ReplicaSet/x", "Rollout/x", "Deployment"} {
		if _, ok := ValuesFor(rec(w, Mi)); ok {
			t.Errorf("%s got values", w)
		}
	}
	for _, w := range []string{"DaemonSet/x", "CronJob/x", "StatefulSet/x"} {
		if _, ok := ValuesFor(rec(w, Mi)); !ok {
			t.Errorf("%s got none", w)
		}
	}
}
