package cli

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/cursopic/phrontis/internal/report"
)

func callTool(t *testing.T, build func() (export, error), tool, args string) map[string]any {
	t.Helper()
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tool + `","arguments":` + args + `}}` + "\n"
	var out strings.Builder
	if err := mcpServer(build).Serve(strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	var m struct{ Result map[string]any }
	if err := json.Unmarshal([]byte(out.String()), &m); err != nil {
		t.Fatal(err)
	}
	return m.Result
}

func TestMCPTools(t *testing.T) {
	e := export{Report: report.Report{Nodes: 2, Monthly: 100, IdleMonthly: 40,
		Namespaces: []report.Line{{Namespace: "web"}},
		Recs:       []report.Rec{{Namespace: "web", Workload: "a"}, {Namespace: "db", Workload: "b"}}}, Context: "ctx"}
	build := func() (export, error) { return e, nil }
	text := func(r map[string]any) string { return r["content"].([]any)[0].(map[string]any)["text"].(string) }

	var s map[string]any
	if err := json.Unmarshal([]byte(text(callTool(t, build, "cost_summary", `{}`))), &s); err != nil {
		t.Fatal(err)
	}
	if s["Context"] != "ctx" || s["Monthly"] != 100.0 || s["IdleMonthly"] != 40.0 || s["Nodes"] != 2.0 {
		t.Fatalf("summary: %v", s)
	}
	if got := text(callTool(t, build, "cost_by_namespace", `{}`)); !strings.Contains(got, `"web"`) {
		t.Fatalf("namespaces: %s", got)
	}
	var recs []report.Rec
	if err := json.Unmarshal([]byte(text(callTool(t, build, "rightsizing", `{"namespace":"db"}`))), &recs); err != nil || len(recs) != 1 || recs[0].Workload != "b" {
		t.Fatalf("filtered recs: %v %v", recs, err)
	}
	if err := json.Unmarshal([]byte(text(callTool(t, build, "rightsizing", `{}`))), &recs); err != nil || len(recs) != 2 {
		t.Fatalf("all recs: %v %v", recs, err)
	}
	var sp spotReport
	if err := json.Unmarshal([]byte(text(callTool(t, build, "spot_candidates", `{}`))), &sp); err != nil || sp.Priced || sp.Note == "" {
		t.Fatalf("spot without data: %+v %v", sp, err)
	}
	var ns nodeSavings
	if err := json.Unmarshal([]byte(text(callTool(t, build, "node_savings", `{}`))), &ns); err != nil || ns.Available || ns.Note == "" || ns.Current != 100 {
		t.Fatalf("nodes without simulation: %+v %v", ns, err)
	}
	e.Spot = &spotReport{Priced: true, Note: "spot", Saving: 12, Kept: 1, Workloads: []report.SpotLine{{}}}
	e.NodePool = &karpenter{NodePool: report.NodePool{Type: "m5.large", Nodes: 3, Monthly: 70, Current: 100}}
	if err := json.Unmarshal([]byte(text(callTool(t, build, "spot_candidates", `{}`))), &sp); err != nil || !sp.Priced || sp.Saving != 12 {
		t.Fatalf("spot: %+v %v", sp, err)
	}
	if err := json.Unmarshal([]byte(text(callTool(t, build, "node_savings", `{}`))), &ns); err != nil || !ns.Available || ns.Saving != 30 || ns.Type != "m5.large" {
		t.Fatalf("nodes: %+v %v", ns, err)
	}
	r := callTool(t, func() (export, error) { return export{}, errors.New("no kubeconfig") }, "cost_summary", `{}`)
	if r["isError"] != true || text(r) != "no kubeconfig" {
		t.Fatalf("error result: %v", r)
	}
}
