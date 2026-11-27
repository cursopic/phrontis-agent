package report

import (
	"strings"
	"testing"
)

func TestCSVInjectionAndQuoting(t *testing.T) {
	for _, c := range []string{"=", "+", "-", "@", "\t", "\r", "\n"} {
		out := string(CSV([]Rec{{Namespace: c + "x", Workload: "w", Container: "c"}}))
		// encoding/csv rewrites CR and LF inside a quoted cell, so only the prefix is compared.
		if !strings.Contains(out, "'") || strings.Contains(out, "\r\n"+c+"x") {
			t.Errorf("%q not prefixed: %q", c, out)
		}
	}
	out := string(CSV([]Rec{{Namespace: "a,b", Workload: `q"r`, Container: "c", MonthlySaving: -3.456,
		Request: Resources{500, 256 << 20}, Recommended: Resources{100, 64 << 20}, Pods: 2,
		Pod: "p-1", CPUUsage: 80, MemUsage: 100 << 20, MonthlyCost: 12.345}}))
	want := "namespace,workload,container,pods,cpu_request_m,cpu_recommended_m,mem_request_mib,mem_recommended_mib,monthly_saving_usd,pod,cpu_usage_m,mem_usage_mib,monthly_cost_usd\r\n" +
		"\"a,b\",\"q\"\"r\",c,2,500,100,256,64,-3.46,p-1,80,100,12.35\r\n"
	if out != want {
		t.Errorf("got %q", out)
	}
}

func TestCSVPodNameIsEscaped(t *testing.T) {
	out := string(CSV([]Rec{{Namespace: "n", Workload: "w", Container: "c", Pod: "=x"}}))
	if strings.Contains(out, ",=x") || !strings.Contains(out, ",'=x,") {
		t.Errorf("pod name not escaped: %q", out)
	}
}

func TestMarkdown(t *testing.T) {
	out := string(Markdown([]Rec{{Namespace: "a|b", Workload: "x\r\ny", Container: `c\|<b>[l]` + "`", MonthlySaving: -3.456,
		Request: Resources{500, 256 << 20}, Recommended: Resources{100, 64 << 20}, Pods: 2,
		Pod: "=p", CPUUsage: 80, MemUsage: 100 << 20, MonthlyCost: 12.345}}))
	want := "| " + strings.Join(CSVHeader, " | ") + " |\n" +
		"|" + strings.Repeat(" --- |", len(CSVHeader)) + "\n" +
		`| a\|b | x y | c\\\|\<b>\[l]\` + "` | 2 | 500 | 100 | 256 | 64 | -3.46 | =p | 80 | 100 | 12.35 |\n"
	if out != want {
		t.Errorf("got\n%s\nwant\n%s", out, want)
	}
}
