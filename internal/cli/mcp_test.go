package cli

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/cursopic/phrontis/internal/report"
)

func callTool(t *testing.T, build func() (export, error), tool, args string) map[string]any {
	t.Helper()
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tool + `","arguments":` + args + `}}` + "\n"
	var out strings.Builder
	if err := mcpServer(build, "", "").Serve(strings.NewReader(in), &out); err != nil {
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

func linkServer(t *testing.T, tool, args, cluster string) (string, bool) {
	t.Helper()
	const Mi = 1 << 20
	recs := []report.Rec{
		{Namespace: "shop", Workload: "Deployment/api", Container: "api", Recommended: report.Resources{MilliCPU: 120, MemBytes: 1536 * Mi}},
		{Namespace: "a&b#c/d", Workload: "Deployment/x", Container: "y", Recommended: report.Resources{MilliCPU: 1, MemBytes: Mi}},
		{Namespace: "shop", Workload: "Job/migrate", Container: "m"},
	}
	build := func() (export, error) { return export{Report: report.Report{Recs: recs}}, nil }
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tool + `","arguments":` + args + `}}` + "\n"
	var out strings.Builder
	if err := mcpServer(build, cluster, "https://app.example.com").Serve(strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	var m struct {
		Result struct {
			Content []struct{ Text string }
			IsError bool
		}
	}
	if err := json.Unmarshal([]byte(out.String()), &m); err != nil {
		t.Fatal(err)
	}
	return m.Result.Content[0].Text, m.Result.IsError
}

type trap struct{ t *testing.T }

func (r trap) RoundTrip(*http.Request) (*http.Response, error) {
	r.t.Error("MCP tool opened a network connection")
	return nil, errors.New("blocked")
}

func TestMCPPatchAndLink(t *testing.T) {
	old := http.DefaultTransport
	http.DefaultTransport = trap{t} // the tools must not talk to GitHub or the Phrontis API
	defer func() { http.DefaultTransport = old }()

	txt, isErr := linkServer(t, "patch", `{"namespace":"shop","workload":"Deployment/api","container":"api"}`, "")
	if isErr || !strings.Contains(txt, `"File":"api.patch.yaml"`) || !strings.Contains(txt, "memory: 1536Mi") {
		t.Fatalf("patch without --cluster: %v %s", isErr, txt)
	}
	for _, c := range []struct{ tool, args, want string }{
		{"patch", `{"namespace":"shop","workload":"Deployment/zzz","container":"api"}`, "No recommendation for shop/Deployment/zzz/api"},
		{"patch", `{"namespace":"shop","workload":"Job/migrate","container":"m"}`, "no patch for this kind"},
		{"open_pr_link", `{"namespace":"shop","workload":"Deployment/api","container":"api","repo":"nope"}`, "repo: Use owner/name"},
		{"open_pr_link", `{"namespace":"shop","workload":"Deployment/api","container":"api","path":"../x.yaml"}`, "path: "},
		{"open_pr_link", `{"namespace":"shop","workload":"Deployment/zzz","container":"api"}`, "No recommendation for"},
	} {
		if txt, isErr := linkServer(t, c.tool, c.args, "c1"); !isErr || !strings.Contains(txt, c.want) || strings.Contains(txt, "http") {
			t.Errorf("%s %s: %v %q, want error with %q and no link", c.tool, c.args, isErr, txt, c.want)
		}
	}
	if txt, isErr := linkServer(t, "open_pr_link", `{"namespace":"shop","workload":"Deployment/api","container":"api"}`, ""); !isErr || txt != "Set --cluster to the Phrontis cluster id (Clusters page)" {
		t.Errorf("no --cluster: %v %q", isErr, txt)
	}
	txt, isErr = linkServer(t, "open_pr_link", `{"namespace":"a&b#c/d","workload":"Deployment/x","container":"y","repo":"acme/infra","path":"k8s/a b.yaml"}`, "c&1#/")
	var out struct{ Link string }
	if err := json.Unmarshal([]byte(txt), &out); err != nil || isErr {
		t.Fatalf("link: %v %s", err, txt)
	}
	u, err := url.Parse(out.Link)
	if err != nil || u.Scheme != "https" || u.Host != "app.example.com" || u.Path != "/" || u.Fragment != "" {
		t.Fatalf("link shape: %q %v", out.Link, err)
	}
	q := u.Query()
	if q.Get("cluster") != "c&1#/" || q.Get("pr") != "a&b#c/d/Deployment/x/y" || q.Get("repo") != "acme/infra" || q.Get("path") != "k8s/a b.yaml" || len(q) != 4 {
		t.Fatalf("link params: %v", q)
	}
}

func TestCheckAppURL(t *testing.T) {
	for _, ok := range []string{"https://app.phrontis.ai", "https://host:8443", "http://localhost", "http://localhost:5173", "http://127.0.0.1:3000"} {
		if err := checkAppURL(ok); err != nil {
			t.Errorf("%s rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "app.phrontis.ai", "http://localhost.evil.com", "https://user@host", "https://user:p@host", "https://host/x", "https://host/",
		"https://host?a=1", "https://host?", "https://host#f", "https://host#", "http://example.com", "http://127.0.0.1.evil.com", "ftp://host", "https:host", "http://[::1]"} {
		if err := checkAppURL(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
