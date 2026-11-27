package report

import (
	"strings"
	"testing"
	"time"
)

func TestExplain(t *testing.T) {
	const day = 24 * time.Hour
	cases := []struct {
		name     string
		r        Rec
		windowed bool
		window   time.Duration
		has      []string
		not      []string
	}{
		{"live", Rec{}, false, 0, []string{"peak CPU and memory of the live samples", "20% CPU and 30% memory headroom"}, []string{"OOM", "throttled", "HPA"}},
		{"window days", Rec{}, true, 7 * day, []string{"p95 CPU and peak memory over 7 days"}, []string{"live"}},
		{"window hours", Rec{}, true, 36 * time.Hour, []string{"over 36 hours"}, nil},
		{"window 20m", Rec{}, true, 20 * time.Minute, []string{"over 1 hour"}, []string{"0 hours"}},
		{"window 166h", Rec{}, true, 166 * time.Hour, []string{"over 7 days"}, []string{"h0m"}},
		{"oom floor", Rec{OOMKilled: true}, false, 0, []string{"held above the last OOM kill"}, []string{"throttled"}},
		{"throttle", Rec{Throttled: true}, false, 0, []string{"within 10% of its limit", "limit must rise"}, []string{"OOM"}},
		{"hpa", Rec{HPA: &HPARef{Name: "web"}}, false, 0, []string{"HPA web scales this workload"}, []string{"OOM"}},
		{"floor cpu held", Rec{Request: Resources{50, 64 << 20}, Recommended: Resources{MilliCPU: minMilliCPU, MemBytes: 64 << 20}}, false, 0, []string{"held at it", "minimum size (10m CPU, 32Mi memory)"}, []string{"raised", "unset"}},
		{"floor mem held", Rec{Request: Resources{50, 64 << 20}, Recommended: Resources{MilliCPU: 50, MemBytes: minMemBytes}}, false, 0, []string{"held at it"}, []string{"raised"}},
		{"floor unset", Rec{Recommended: Resources{MilliCPU: minMilliCPU, MemBytes: 64 << 20}}, false, 0, []string{"unset request is set to the minimum"}, []string{"held", "raised"}},
		{"request below floor", Rec{Request: Resources{5, 30 << 20}, Recommended: Resources{MilliCPU: minMilliCPU, MemBytes: 64 << 20}}, false, 0, nil, []string{"minimum", "raised"}},
		{"above floor", Rec{Recommended: Resources{MilliCPU: 50, MemBytes: 64 << 20}}, false, 0, nil, []string{"minimum"}},
		{"all", Rec{OOMKilled: true, Throttled: true, HPA: &HPARef{Name: "w"}}, true, day, []string{"over 24 hours", "OOM", "throttled", "HPA w"}, nil},
	}
	for _, c := range cases {
		got := Explain(c.r, c.windowed, c.window)
		for _, s := range c.has {
			if !strings.Contains(got, s) {
				t.Errorf("%s: %q lacks %q", c.name, got, s)
			}
		}
		for _, s := range c.not {
			if strings.Contains(got, s) {
				t.Errorf("%s: %q has %q", c.name, got, s)
			}
		}
		if !strings.HasSuffix(got, ".") || strings.Contains(got, ". ") {
			t.Errorf("%s: not one sentence: %q", c.name, got)
		}
	}
}

func TestExplainAll(t *testing.T) {
	recs := []Rec{{OOMKilled: true}, {}}
	ExplainAll(recs, false, 0)
	if !strings.Contains(recs[0].Explanation, "OOM") || recs[1].Explanation == "" || strings.Contains(recs[1].Explanation, "OOM") {
		t.Errorf("%+v", recs)
	}
}
