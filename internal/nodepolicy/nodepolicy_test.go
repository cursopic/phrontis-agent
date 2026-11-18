package nodepolicy

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// fixtures are the policies whose YAML is kept in testdata/<name>.yaml and dry-run applied by TestServerDryRun.
var fixtures = []struct {
	name string
	p    Policy
	c    NodeClass
}{
	{"on-demand", Policy{Name: "default", NodeClass: "default", Families: []string{"m6i", "c6i"}, Arch: []string{"amd64"}},
		NodeClass{Cluster: "demo", Role: "KarpenterNodeRole-demo", AMI: "al2023@v20240625"}},
	{"spot-arm-limit", Policy{Name: "phrontis", NodeClass: "phrontis", Families: []string{"m7g", "c7g", "m6i"},
		Arch: []string{"amd64", "arm64"}, Spot: true, CPULimit: 16, ExpireAfter: "Never"},
		NodeClass{Cluster: "prod-eu", Role: "KarpenterNodeRole-prod-eu", AMI: "bottlerocket@v1.10.0"}},
	{"weekday-window", Policy{Name: "weekday", NodeClass: "weekday", Families: []string{"m6i"}, Arch: []string{"amd64"},
		DrainPercent: 20, Window: Window{"0 1 * * 1-5", 4 * time.Hour}, ExpireAfter: "336h"},
		NodeClass{Cluster: "demo", Role: "KarpenterNodeRole-demo", AMI: "al2023@v20240625"}},
}

func golden(name string) string { return filepath.Join("testdata", name+".yaml") }

func TestRenderGolden(t *testing.T) {
	for _, f := range fixtures {
		got, err := Render(f.p, f.c)
		if err != nil {
			t.Fatalf("%s: %v", f.name, err)
		}
		if *update {
			if err := os.WriteFile(golden(f.name), got, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(golden(f.name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: got\n%s\nwant\n%s\nafter an intended change: go test ./internal/nodepolicy -update", f.name, got, want)
		}
	}
}

// TestRenderRefuses checks that each bad input is caught by its own rule and by that rule only.
func TestRenderRefuses(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*Policy, *NodeClass)
		want string
	}{
		"no family":       {func(p *Policy, _ *NodeClass) { p.Families = nil }, "at least one instance family"},
		"yaml in family":  {func(p *Policy, _ *NodeClass) { p.Families = []string{"m6i\nkind: Secret"} }, "not a family name"},
		"gpu family":      {func(p *Policy, _ *NodeClass) { p.Families = []string{"m6i", "g6e"} }, `"g6e" is GPU`},
		"no arch":         {func(p *Policy, _ *NodeClass) { p.Arch = nil }, "at least one architecture"},
		"bad arch":        {func(p *Policy, _ *NodeClass) { p.Arch = []string{"x86_64"} }, `architecture "x86_64"`},
		"negative cpu":    {func(p *Policy, _ *NodeClass) { p.CPULimit = -1 }, "negative"},
		"bad pool":        {func(p *Policy, _ *NodeClass) { p.Name = "Default" }, `name "Default"`},
		"no class":        {func(p *Policy, _ *NodeClass) { p.NodeClass = "" }, `name ""`},
		"long spot name":  {func(p *Policy, _ *NodeClass) { p.Spot, p.Name = true, strings.Repeat("a", 59) }, "at most 58 characters with spot"},
		"latest ami":      {func(_ *Policy, c *NodeClass) { c.AMI = "al2023@latest" }, "not latest"},
		"windows ami":     {func(_ *Policy, c *NodeClass) { c.AMI = "windows2022@v20240625" }, "Windows AMIs cannot be pinned"},
		"no role":         {func(_ *Policy, c *NodeClass) { c.Role = "" }, "role is empty"},
		"no cluster":      {func(_ *Policy, c *NodeClass) { c.Cluster = "" }, "EKS cluster name"},
		"star cluster":    {func(_ *Policy, c *NodeClass) { c.Cluster = "*" }, `EKS cluster name "*"`},
		"drain over 100":  {func(p *Policy, _ *NodeClass) { p.DrainPercent = 101 }, "drain budget 101%: use 1 to 100 percent of nodes, or 0 for the default 10%"},
		"negative drain":  {func(p *Policy, _ *NodeClass) { p.DrainPercent = -5 }, "drain budget -5%"},
		"short cron":      {func(p *Policy, _ *NodeClass) { p.Window = Window{"0 1 * *", time.Hour} }, `window schedule "0 1 * *"`},
		"day names":       {func(p *Policy, _ *NodeClass) { p.Window = Window{"0 1 * * mon-fri", time.Hour} }, `window schedule "0 1 * * mon-fri"`},
		"day 7":           {func(p *Policy, _ *NodeClass) { p.Window = Window{"0 1 * * 5-7", time.Hour} }, `window schedule "0 1 * * 5-7"`},
		"twice a day":     {func(p *Policy, _ *NodeClass) { p.Window = Window{"0 1,13 * * *", time.Hour} }, `window schedule "0 1,13 * * *"`},
		"monthly":         {func(p *Policy, _ *NodeClass) { p.Window = Window{"0 1 1 * *", time.Hour} }, `window schedule "0 1 1 * *"`},
		"no schedule":     {func(p *Policy, _ *NodeClass) { p.Window = Window{"", time.Hour} }, `window schedule ""`},
		"no length":       {func(p *Policy, _ *NodeClass) { p.Window = Window{"0 1 * * *", 0} }, "window length 0s"},
		"negative length": {func(p *Policy, _ *NodeClass) { p.Window = Window{"0 1 * * *", -time.Hour} }, "window length -1h0m0s"},
		"seconds":         {func(p *Policy, _ *NodeClass) { p.Window = Window{"0 1 * * *", 90 * time.Second} }, "window length 1m30s"},
		"plus minute":     {func(p *Policy, _ *NodeClass) { p.Window = Window{"+5 1 * * *", time.Hour} }, `window schedule "+5 1 * * *"`},
		"minus zero hour": {func(p *Policy, _ *NodeClass) { p.Window = Window{"0 -0 * * *", time.Hour} }, `window schedule "0 -0 * * *"`},
		"plus day":        {func(p *Policy, _ *NodeClass) { p.Window = Window{"0 1 * * +1-5", time.Hour} }, `window schedule "0 1 * * +1-5"`},
		"plus day end":    {func(p *Policy, _ *NodeClass) { p.Window = Window{"0 1 * * 1-+5", time.Hour} }, `window schedule "0 1 * * 1-+5"`},
		"expire zero":     {func(p *Policy, _ *NodeClass) { p.ExpireAfter = "0h" }, `expireAfter "0h"`},
		"expire days":     {func(p *Policy, _ *NodeClass) { p.ExpireAfter = "30d" }, `expireAfter "30d"`},
		"expire never lc": {func(p *Policy, _ *NodeClass) { p.ExpireAfter = "never" }, `expireAfter "never"`},
		"expire overflow": {func(p *Policy, _ *NodeClass) { p.ExpireAfter = "9999999999h" }, `expireAfter "9999999999h"`},
	} {
		p, c := fixtures[0].p, fixtures[0].c
		tc.edit(&p, &c)
		y, err := Render(p, c)
		if err == nil {
			t.Errorf("%s: rendered, want an error:\n%s", name, y)
		} else if !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "\n") {
			t.Errorf("%s: error %q, want only the error containing %q", name, err, tc.want)
		}
	}
}

// TestSpotPools checks P50 on every fixture: a pool that allows spot allows only spot and carries exactly the taint the
// webhook's opt-in toleration matches, so no other pod gets a spot node; the on-demand pool has no taint.
func TestSpotPools(t *testing.T) {
	type pool struct {
		Metadata struct{ Name string }
		Spec     struct {
			Weight   *int
			Limits   map[string]string
			Template struct {
				Spec struct {
					Requirements []struct {
						Key    string
						Values []string
					}
					Taints []map[string]string
				}
			}
		}
	}
	for _, f := range fixtures {
		y, err := NodePool(f.p)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, doc := range strings.Split(string(y), "---\n") {
			var np pool
			if err := yaml.Unmarshal([]byte(doc), &np); err != nil {
				t.Fatal(err)
			}
			names = append(names, np.Metadata.Name)
			s := np.Spec.Template.Spec
			var capacity []string
			for _, r := range s.Requirements {
				if r.Key == CapacityType {
					capacity = r.Values
				}
			}
			spot := []map[string]string{{"key": CapacityType, "value": "spot", "effect": "NoSchedule"}}
			// With spot, only the on-demand pool is weighted, so a pod tolerating every taint (operator Exists) without
			// opting in gets on-demand first.
			weight := 0
			if np.Spec.Weight != nil {
				weight = *np.Spec.Weight
			}
			switch {
			case slices.Equal(capacity, []string{"on-demand"}) && s.Taints == nil && (weight == 1) == f.p.Spot:
			case slices.Equal(capacity, []string{"spot"}) && reflect.DeepEqual(s.Taints, spot) && np.Spec.Weight == nil:
			default:
				t.Errorf("%s: pool %s: capacity %v, taints %v, weight %v", f.name, np.Metadata.Name, capacity, s.Taints, np.Spec.Weight)
			}
		}
		want := []string{f.p.Name}
		if f.p.Spot {
			want = append(want, f.p.Name+"-spot")
		}
		if !slices.Equal(names, want) {
			t.Errorf("%s: pools %v, want %v", f.name, names, want)
		}

		// Spot off: the spot pool keeps its taint, launches nothing and asks for on-demand, which drifts its spot nodes.
		off := f.p
		off.Spot = false
		y, err = SpotOff(off)
		var np pool
		if err != nil || yaml.Unmarshal(y, &np) != nil {
			t.Fatalf("%s: SpotOff: %v\n%s", f.name, err, y)
		}
		reqs := np.Spec.Template.Spec.Requirements
		if np.Metadata.Name != f.p.Name+"-spot" || np.Spec.Weight != nil || np.Spec.Limits["cpu"] != "0" ||
			reqs[len(reqs)-1].Key != CapacityType || !slices.Equal(reqs[len(reqs)-1].Values, []string{"on-demand"}) ||
			!reflect.DeepEqual(np.Spec.Template.Spec.Taints, []map[string]string{{"key": CapacityType, "value": "spot", "effect": "NoSchedule"}}) {
			t.Errorf("%s: SpotOff:\n%s", f.name, y)
		}
	}
	long := fixtures[0].p
	long.Name = strings.Repeat("a", 59) // a valid name whose spot pool's name would not fit: there is none to retire
	if y, err := SpotOff(long); y != nil || err != nil {
		t.Errorf("SpotOff(59 characters) = %s, %v", y, err)
	}
}

// TestWindowShut walks every minute of a week and checks that a nodes-0 budget is active exactly while the window is
// shut. A budget is active for its duration from each hit of its schedule, in UTC (CRD spec.disruption.budgets).
func TestWindowShut(t *testing.T) {
	every := []int{0, 1, 2, 3, 4, 5, 6}
	for _, tc := range []struct {
		w    Window
		days []int // weekdays it opens on, 0 is Sunday
		at   int   // minute of the day it opens at
	}{
		{Window{"0 1 * * *", 4 * time.Hour}, every, 60},
		{Window{"30 1 * * 1-5", 4 * time.Hour}, []int{1, 2, 3, 4, 5}, 90},
		{Window{"45 22 * * 0,3,6", 3*time.Hour + 30*time.Minute}, []int{0, 3, 6}, 22*60 + 45}, // closes after midnight
		{Window{"0 0 * * 6", 48 * time.Hour}, []int{6}, 0},                                    // the weekend, into Monday
		{Window{"0 0 * * *", 30 * time.Hour}, every, 0},                                       // never shut
	} {
		budgets, err := tc.w.shut()
		if err != nil {
			t.Fatalf("%v: %v", tc.w, err)
		}
		var starts, lengths []int
		for _, b := range budgets {
			f := strings.Fields(b["schedule"].(string))
			d, err := time.ParseDuration(b["duration"].(string))
			if b["nodes"] != "0" || len(f) != 5 || f[2] != "*" || f[3] != "*" || err != nil {
				t.Fatalf("%v: budget %v is not a nodes-0 budget on fixed weekdays", tc.w, b)
			}
			mi, err1 := strconv.Atoi(f[0])
			h, err2 := strconv.Atoi(f[1])
			if err1 != nil || err2 != nil {
				t.Fatalf("%v: budget schedule %q has no numeric minute and hour", tc.w, b["schedule"])
			}
			days := strings.Split(f[4], ",")
			if f[4] == "*" {
				days = strings.Split("0,1,2,3,4,5,6", ",")
			}
			for _, s := range days {
				n, err := strconv.Atoi(s)
				if err != nil {
					t.Fatalf("%v: budget schedule %q has a day that is not a number", tc.w, b["schedule"])
				}
				starts, lengths = append(starts, n*day+h*60+mi), append(lengths, int(d/time.Minute))
			}
		}
		for x := range week {
			open, blocked := false, false
			for _, d := range tc.days {
				open = open || (x-d*day-tc.at+week)%week < int(tc.w.Duration/time.Minute)
			}
			for i, s := range starts {
				blocked = blocked || (x-s+week)%week < lengths[i]
			}
			if open == blocked {
				t.Fatalf("%v: day %d %02d:%02d UTC: open %v, nodes-0 budget active %v\n%v", tc.w, x/day, x%day/60, x%60, open, blocked, budgets)
			}
		}
	}
}

// TestExpireAfter checks expireAfter is always written, Karpenter's own 720h when the policy leaves it out.
func TestExpireAfter(t *testing.T) {
	for in, want := range map[string]string{"": "expireAfter: 720h\n", "Never": "expireAfter: Never\n", "336h": "expireAfter: 336h\n", "1h30m": "expireAfter: 1h30m\n"} {
		p := fixtures[0].p
		p.ExpireAfter = in
		y, err := NodePool(p)
		if err != nil || !strings.Contains(string(y), "      "+want) {
			t.Errorf("expireAfter %q: err %v, want template.spec %q in\n%s", in, err, want, y)
		}
	}
}

// TestAccelerated pins the GPU and accelerator families from the AWS page against look-alike CPU families.
func TestAccelerated(t *testing.T) {
	for _, f := range []string{"p5", "p6-b200", "g4dn", "g6f", "gr6", "inf2", "trn2u", "dl2q", "f2", "vt1"} {
		if !accelerated.MatchString(f) {
			t.Errorf("%s is accelerated computing, want it refused", f)
		}
	}
	for _, f := range []string{"m6i", "c7g", "r7iz", "i4i", "im4gn", "is4gen", "d3", "hpc7g", "t3", "x2gd", "z1d", "u7i-12tb"} {
		if accelerated.MatchString(f) {
			t.Errorf("%s is a CPU family, want it allowed", f)
		}
	}
}

// TestCRDsMatchVersion fails when KarpenterVersion moves without re-vendoring testdata/crds.
func TestCRDsMatchVersion(t *testing.T) {
	for _, f := range []string{"karpenter.sh_nodepools.yaml", "karpenter.k8s.aws_ec2nodeclasses.yaml"} {
		b, err := os.ReadFile(filepath.Join("testdata", "crds", f))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(b, []byte("/karpenter-provider-aws/v"+KarpenterVersion+"/")) {
			t.Errorf("%s is not from Karpenter v%s: re-vendor it from that tag", f, KarpenterVersion)
		}
	}
}

// TestServerDryRun is the P41 acceptance check: each golden file passes `kubectl apply --dry-run=server` against
// the pinned CRDs. It creates a throwaway kind cluster with the CRDs only (no Karpenter controller) and deletes it.
// The kubeconfig is a temp file, so ~/.kube/config and its current context are never touched.
// Opt-in, because it pulls the kindest/node image: needs PHRONTIS_KIND_TEST=1, kind, kubectl and a running Docker.
//
//	PHRONTIS_KIND_TEST=1 go test ./internal/nodepolicy -run TestServerDryRun -v
func TestServerDryRun(t *testing.T) {
	if os.Getenv("PHRONTIS_KIND_TEST") != "1" {
		t.Skip("opt-in: set PHRONTIS_KIND_TEST=1 to start a kind cluster (pulls kindest/node)")
	}
	for _, tool := range []string{"kind", "kubectl", "docker"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not found", tool)
		}
	}
	if exec.Command("docker", "info").Run() != nil {
		t.Skip("Docker is not running")
	}
	cluster := fmt.Sprintf("phrontis-p41-%d", os.Getpid()) // unique per run, so parallel worktrees never share a cluster
	dir := t.TempDir()
	kubeconfig := filepath.Join(dir, "kubeconfig")
	run := func(name string, args ...string) (string, error) {
		out, err := exec.Command(name, args...).CombinedOutput()
		return string(out), err
	}
	kubectl := func(args ...string) (string, error) {
		return run("kubectl", append([]string{"--kubeconfig", kubeconfig}, args...)...)
	}
	if out, err := run("kind", "create", "cluster", "--name", cluster, "--kubeconfig", kubeconfig); err != nil {
		t.Fatalf("kind create cluster: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		if out, err := run("kind", "delete", "cluster", "--name", cluster, "--kubeconfig", kubeconfig); err != nil {
			t.Errorf("kind delete cluster %s: %v\n%s", cluster, err, out)
		}
	})
	if out, err := kubectl("apply", "-f", filepath.Join("testdata", "crds")); err != nil {
		t.Fatalf("install CRDs: %v\n%s", err, out)
	}
	if out, err := kubectl("wait", "--for", "condition=established", "--timeout", "60s",
		"crd/nodepools.karpenter.sh", "crd/ec2nodeclasses.karpenter.k8s.aws"); err != nil {
		t.Fatalf("CRDs not established: %v\n%s", err, out)
	}
	for _, f := range fixtures {
		out, err := kubectl("apply", "--dry-run=server", "-f", golden(f.name))
		if err != nil {
			t.Errorf("%s: %v\n%s", f.name, err, out)
		}
		t.Logf("%s:\n%s", f.name, out)
	}
	// The check has teeth: a NodePool with disruption but no consolidateAfter (required by the CRD) is refused.
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte(`apiVersion: karpenter.sh/v1
kind: NodePool
metadata: {name: bad}
spec:
  template:
    spec:
      nodeClassRef: {group: karpenter.k8s.aws, kind: EC2NodeClass, name: default}
      requirements: [{key: kubernetes.io/arch, operator: In, values: [amd64]}]
  disruption: {consolidationPolicy: WhenEmptyOrUnderutilized}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := kubectl("apply", "--dry-run=server", "-f", bad); err == nil || !strings.Contains(out, "consolidateAfter") {
		t.Errorf("NodePool without consolidateAfter: err %v, want a CRD error naming consolidateAfter\n%s", err, out)
	}
}
