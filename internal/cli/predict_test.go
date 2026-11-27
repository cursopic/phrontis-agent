package cli

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cursopic/phrontis/internal/report"
)

// workloadYAML is one workload; spec goes under spec:, containers under the pod's spec.
func workloadYAML(kind, spec, pod string) string {
	api := "apps/v1"
	if kind == "Job" {
		api = "batch/v1"
	}
	return "apiVersion: " + api + "\nkind: " + kind + "\nmetadata: {name: web}\nspec:\n  " + spec + "\n  template:\n    spec: {" + pod + "}\n"
}

const oneCore = `containers: [{name: app, resources: {requests: {cpu: "1", memory: 1Gi}}}]`

// runPredict parses the JSON output; every test file is priced at 1 per vCPU-hour and 0.1 per GiB-hour.
func runPredict(t *testing.T, in string, args ...string) (p prediction, errOut string, err error) {
	t.Helper()
	t.Setenv("KUBECONFIG", "/nonexistent/kubeconfig")
	var out, e bytes.Buffer
	err = predict(append([]string{"-f", "-", "--json", "--cpu-hour", "1", "--gib-hour", "0.1"}, args...), strings.NewReader(in), &out, &e)
	if err == nil {
		if err := json.Unmarshal(out.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
	} else if out.Len() > 0 {
		t.Errorf("printed a result with an error: %s", out.String())
	}
	return p, e.String(), err
}

func TestPredictKinds(t *testing.T) {
	for _, c := range []struct {
		kind, spec string
		args       []string
		replicas   int64
		monthly    float64
		hourly     float64
	}{
		{"Deployment", "replicas: 3", nil, 3, 3 * 1.1 * 730, 0},
		{"Deployment", "paused: false", nil, 1, 1.1 * 730, 0},
		{"Deployment", "replicas: 0", nil, 0, 0, 0},
		{"StatefulSet", "replicas: 2", nil, 2, 2 * 1.1 * 730, 0},
		{"DaemonSet", "minReadySeconds: 1", []string{"--nodes", "4"}, 4, 4 * 1.1 * 730, 0},
		{"Job", "parallelism: 5", nil, 5, 0, 5 * 1.1},
		{"Job", "completions: 9", nil, 1, 0, 1.1},
	} {
		p, _, err := runPredict(t, workloadYAML(c.kind, c.spec, oneCore), c.args...)
		if err != nil || len(p.Workloads) != 1 {
			t.Errorf("%s %s: %+v, %v", c.kind, c.spec, p, err)
			continue
		}
		w := p.Workloads[0]
		if w.Workload != c.kind+"/web" || w.Namespace != "default" || w.Replicas != c.replicas ||
			math.Abs(w.Monthly-c.monthly) > 1e-6 || math.Abs(w.Hourly-c.hourly) > 1e-6 || math.Abs(p.Monthly-c.monthly) > 1e-6 || p.AtLeast {
			t.Errorf("%s %s: %+v", c.kind, c.spec, p)
		}
	}
}

// Documents, Lists, JSON, stdin and a file all end in the same workloads.
func TestPredictInput(t *testing.T) {
	dep := workloadYAML("Deployment", "replicas: 1", oneCore)
	sts := workloadYAML("StatefulSet", "replicas: 1", oneCore)
	js := `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web"},"spec":{"template":{"spec":{"containers":[{"name":"app"}]}}}}`
	for name, c := range map[string]struct {
		in string
		n  int
	}{
		"documents":       {"---\n# only a comment\n---\n" + dep + "---\n\n---\n" + sts + "---\n", 2},
		"document end":    {dep + "...\n" + sts, 2},
		"list":            {"apiVersion: v1\nkind: List\nitems:\n- " + strings.ReplaceAll(strings.TrimSpace(dep), "\n", "\n  ") + "\n- " + strings.ReplaceAll(strings.TrimSpace(sts), "\n", "\n  "), 2},
		"json":            {js, 1},
		"json list":       {`{"apiVersion":"v1","kind":"List","items":[` + js + `,` + js + `]}`, 2},
		"json stream":     {js + "\n" + js, 2},
		"json, then yaml": {js + "\n---\n" + dep, 2},
		"crlf":            {strings.ReplaceAll(dep+"---\n"+sts, "\n", "\r\n"), 2},
	} {
		if p, _, err := runPredict(t, c.in); err != nil || len(p.Workloads) != c.n {
			t.Errorf("%s: %+v, %v", name, p, err)
		}
	}
	f := filepath.Join(t.TempDir(), "app.yaml")
	if err := os.WriteFile(f, []byte(dep), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, e bytes.Buffer
	if err := predict([]string{"-f", f}, nil, &out, &e); err != nil || !strings.Contains(out.String(), "Deployment/web") {
		t.Errorf("file: %s, %v", out.String(), err)
	}
}

func TestPredictRequests(t *testing.T) {
	res := func(s string) string { return "containers: [{name: app, resources: {" + s + "}}]" }
	for name, c := range map[string]struct {
		pod  string
		want report.Resources
		gaps string
	}{
		"quantities":       {res(`requests: {cpu: 500m, memory: 128Mi}`), report.Resources{MilliCPU: 500, MemBytes: 128 << 20}, ""},
		"decimals":         {res(`requests: {cpu: 0.5, memory: 1e9}`), report.Resources{MilliCPU: 500, MemBytes: 1e9}, ""},
		"none":             {res(``), report.Resources{}, "web/app: no CPU request|web/app: no memory request"},
		"limit only":       {res(`requests: {cpu: 1}, limits: {cpu: 2, memory: 1Gi}`), report.Resources{MilliCPU: 1000, MemBytes: 1 << 30}, ""},
		"zero with limit":  {res(`requests: {cpu: 0, memory: 1Gi}, limits: {cpu: 2}`), report.Resources{MemBytes: 1 << 30}, "web/app: the CPU request is 0"},
		"sidecar, no init": {`initContainers: [{name: s, restartPolicy: Always, resources: {requests: {cpu: 1, memory: 1Gi}}}, {name: i, resources: {requests: {cpu: 9, memory: 9Gi}}}], ` + oneCore, report.Resources{MilliCPU: 2000, MemBytes: 2 << 30}, ""},
	} {
		p, _, err := runPredict(t, workloadYAML("Deployment", "replicas: 1", c.pod))
		if err != nil || len(p.Workloads) != 1 || p.Workloads[0].Resources != c.want || strings.Join(p.NoRequest, "|") != c.gaps || p.AtLeast != (c.gaps != "") {
			t.Errorf("%s: %+v, %v", name, p, err)
		}
	}
}

func TestPredictSkips(t *testing.T) {
	dep := workloadYAML("Deployment", "replicas: 1", oneCore) + "---\n"
	for name, c := range map[string]struct {
		doc, stderr string
		atLeast     bool
	}{
		"daemonset without --nodes": {workloadYAML("DaemonSet", "minReadySeconds: 1", oneCore), "DaemonSet/web (set --nodes)", true},
		"pod-level resources":       {workloadYAML("Deployment", "replicas: 1", "resources: {requests: {cpu: 1}}, "+oneCore), "Deployment/web (pod-level resources are not supported)", true},
		"pod":                       {"apiVersion: v1\nkind: Pod\nmetadata: {name: p}\nspec: {" + oneCore + "}", "Pod/p", true},
		"cronjob":                   {"apiVersion: batch/v1\nkind: CronJob\nmetadata: {name: c}\nspec: {jobTemplate: {spec: {template: {spec: {" + oneCore + "}}}}}", "CronJob/c", true},
		"replicaset":                {workloadYAML("ReplicaSet", "replicas: 1", oneCore), "ReplicaSet/web", true},
		"custom kind":               {"apiVersion: argoproj.io/v1alpha1\nkind: Rollout\nmetadata: {name: r}\nspec: {template: {spec: {" + oneCore + "}}}", "Rollout/r", true},
		"old api group":             {strings.Replace(workloadYAML("Deployment", "replicas: 1", oneCore), "apps/v1", "extensions/v1beta1", 1), "Deployment/web", true},
		"typed list":                {"apiVersion: apps/v1\nkind: DeploymentList\nitems: [{spec: {template: {spec: {" + oneCore + "}}}}]", "DeploymentList", true},
		"no pods":                   {"apiVersion: autoscaling/v2\nkind: HorizontalPodAutoscaler\nmetadata: {name: web}\n---\napiVersion: v1\nkind: PersistentVolumeClaim\nmetadata: {name: data}\nspec: {template: x}", "HorizontalPodAutoscaler/web, PersistentVolumeClaim/data", false},
	} {
		p, e, err := runPredict(t, dep+c.doc)
		if err != nil || len(p.Workloads) != 1 || e != "not counted: "+c.stderr+"\n" || p.AtLeast != c.atLeast || len(p.NotCounted) == 0 {
			t.Errorf("%s: %+v, %q, %v", name, p, e, err)
		}
	}
}

func TestPredictErrors(t *testing.T) {
	ok := workloadYAML("Deployment", "replicas: 1", oneCore) + "---\n"
	res := func(r string) string { return "containers: [{name: app, resources: {requests: {" + r + "}}}]" }
	for name, c := range map[string]struct{ in, want string }{
		"broken yaml":          {ok + "a: [", "document 2"},
		"broken json":          {`{"kind": "Deployment"`, "document 1"},
		"not an object":        {ok + "- a", "document 2"},
		"no kind":              {ok + "a: b", "document 2: the object has no kind"},
		"wrong type":           {ok + workloadYAML("Deployment", `replicas: "two"`, oneCore), "document 2: Deployment/web"},
		"bad quantity":         {ok + workloadYAML("Deployment", "replicas: 1", res("cpu: fast")), "document 2: Deployment/web"},
		"negative replicas":    {workloadYAML("StatefulSet", "replicas: -1", oneCore), "replicas is negative"},
		"negative parallelism": {workloadYAML("Job", "parallelism: -1", oneCore), "parallelism is negative"},
		"negative request":     {workloadYAML("Deployment", "replicas: 1", res("memory: -1Mi")), "memory request is negative"},
		"negative, skipped":    {ok + workloadYAML("DaemonSet", "minReadySeconds: 1", res("cpu: -1")), "CPU request is negative"},
		"too large":            {workloadYAML("Deployment", "replicas: 2147483647", res("memory: 1Ei")), "too large"},
		"quantity overflow":    {workloadYAML("Deployment", "replicas: 1", res("memory: 1e30")), "memory request is too large"},
		"quantity saturates":   {workloadYAML("Deployment", "replicas: 1", res("memory: 8Ei")), "memory request is too large"},
		"cpu overflow":         {workloadYAML("Deployment", "replicas: 1", res("cpu: 1e16")), "CPU request is too large"},
		"too many replicas":    {workloadYAML("Deployment", "replicas: 2147483647", res("memory: 1Pi")), "requests are too large"},
		"no containers":        {workloadYAML("Deployment", "replicas: 1", ""), "no containers"},
		"nothing to count":     {"apiVersion: v1\nkind: Service\nmetadata: {name: s}", "no Deployment"},
		"empty":                {"", "no Deployment"},
	} {
		if _, _, err := runPredict(t, c.in); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	if _, _, err := runPredict(t, ok, "--nodes", "-1"); err == nil {
		t.Error("--nodes -1: no error")
	}
	if _, _, err := runPredict(t, ok, "--cpu-hour", "NaN"); err == nil {
		t.Error("--cpu-hour NaN: no error")
	}
	if err := predict(nil, nil, nil, nil); err == nil {
		t.Error("no -f: no error")
	}
}

func TestPredictTable(t *testing.T) {
	in := workloadYAML("Deployment", "replicas: 2", `containers: [{name: app, resources: {requests: {cpu: 500m}}}]`) + "---\n" + workloadYAML("Job", "parallelism: 2", oneCore)
	var out, e bytes.Buffer
	if err := predict([]string{"-f", "-"}, strings.NewReader(in), &out, &e); err != nil {
		t.Fatal(err)
	}
	want := `WORKLOAD        NAMESPACE  REPLICAS  CPU    MEMORY  $/MO   $/HOUR (JOB)
Deployment/web  default    2         1000m  0Mi     21.90  
Job/web         default    2         2000m  2048Mi         0.0680

total  at least 21.90/month
web/app: no memory request
flat prices: 0.03 per vCPU-hour, 0.004 per GiB-hour; requests only; init containers, storage, network and LimitRange defaults are not counted
`
	if out.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", out.String(), want)
	}
}
