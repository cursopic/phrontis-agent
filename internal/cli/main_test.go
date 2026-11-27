package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsfake "k8s.io/metrics/pkg/client/clientset/versioned/fake"

	"github.com/cursopic/phrontis/internal/pricing"
	"github.com/cursopic/phrontis/internal/report"
)

func ctrl(kind, name string) []metav1.OwnerReference {
	t := true
	return []metav1.OwnerReference{{Kind: kind, Name: name, Controller: &t}}
}

func pod(name, owner, ownerName, node, cpu string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: name, OwnerReferences: ctrl(owner, ownerName)},
		Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "app",
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)}}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func pv(name, class, driver, phase, claimNS, claim string, size string) *corev1.PersistentVolume {
	v := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{StorageClassName: class,
			Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)}},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.PersistentVolumePhase(phase)},
	}
	if driver != "" {
		v.Spec.CSI = &corev1.CSIPersistentVolumeSource{Driver: driver}
	}
	if claim != "" { // a Released PV keeps its claimRef; only Bound ones count as claimed
		v.Spec.ClaimRef = &corev1.ObjectReference{Namespace: claimNS, Name: claim}
	}
	return v
}

func TestCollect(t *testing.T) {
	db := pod("db-0", "StatefulSet", "db", "n1", "500m")
	db.Spec.Containers[0].Resources.Limits = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("512Mi")}
	db.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app",
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"}}}}
	db.Labels = map[string]string{"app": "db"}
	db.Spec.Volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"}}}, {Name: "tmp"}}
	web := pod("web-7d9-a", "ReplicaSet", "web-7d9", "n1", "250m")
	web.Labels = map[string]string{"app": "web", "tier": "front"}
	web.Spec.Containers[0].Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")} // raised by the webhook
	webRS := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web-7d9", OwnerReferences: ctrl("Deployment", "web")}}
	webRS.Spec.Template.Spec.Containers = []corev1.Container{{Name: "app", Resources: corev1.ResourceRequirements{
		Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")}}}} // the manifest's
	cs := fake.NewClientset(
		&policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"},
			Spec: policyv1.PodDisruptionBudgetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}}},
		&policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "none"},
			Spec: policyv1.PodDisruptionBudgetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "ghost"}}}},
		&policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Namespace: "dev", Name: "wrong-ns"},
			Spec: policyv1.PodDisruptionBudgetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: map[string]string{
			"node.kubernetes.io/instance-type": "m5.large", "topology.kubernetes.io/region": "eu-central-1"}},
			Spec: corev1.NodeSpec{ProviderID: "aws:///eu-central-1a/i-0abc"}, Status: corev1.NodeStatus{Capacity: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}}},
		webRS,
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "nightly-123", OwnerReferences: ctrl("CronJob", "nightly")}},
		web,
		pod("nightly-123-x", "Job", "nightly-123", "n1", "1"),
		db,
		pod("pending", "StatefulSet", "db", "", "500m"), // not scheduled yet: no node, no cost
		pod("etcd-n1", "Node", "n1", "n1", "100m"),      // static pod mirrored by the kubelet
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "fast"}, Parameters: map[string]string{"type": "io2"}},
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "plain"}},
		pv("pv-io2", "fast", "ebs.csi.aws.com", "Bound", "shop", "data", "10Gi"),
		pv("pv-default", "plain", "ebs.csi.aws.com", "Bound", "shop", "logs", "20Gi"),
		pv("pv-released", "plain", "ebs.csi.aws.com", "Released", "shop", "old", "5Gi"),
		pv("pv-host", "standard", "", "Bound", "dev", "scratch", "1Gi"),
		pv("pv-noclass", "gone", "ebs.csi.aws.com", "Available", "", "", "2Gi"),
		svc("classic", "", "", "", true),
		svc("annot-nlb", "", "nlb-ip", "", true),
		svc("class-nlb", "eks.amazonaws.com/nlb", "", "", true),
		svc("other-class", "example.com/other", "nlb", "", true),
		svc("odd-annot", "", "internal", "", true),
		svc("pending", "", "", "", false),
		svc("clusterip", "", "", corev1.ServiceTypeClusterIP, true),
	)
	// The fake metrics clientset cannot list objects seeded into it, so answer the list directly.
	mc := &metricsfake.Clientset{}
	mc.AddReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &metricsv1beta1.PodMetricsList{Items: []metricsv1beta1.PodMetrics{{
			ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web-7d9-a"},
			Containers: []metricsv1beta1.ContainerMetrics{{Name: "app", Usage: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("40m"), corev1.ResourceMemory: resource.MustParse("100Mi")}}},
		}}}, nil
	})

	nodes, pods, keys, vols, lbs, pdbs, samples, err := collect(context.Background(), cs, mc, 0)
	if err != nil {
		t.Fatal(err)
	}
	if samples != 1 || len(nodes) != 1 || nodes[0].Capacity != (report.Resources{MilliCPU: 4000, MemBytes: 8 << 30}) {
		t.Fatalf("samples=%d nodes=%+v", samples, nodes)
	}
	if len(keys) != 1 || keys["n1"] != (pricing.Key{Provider: "aws", Region: "eu-central-1", Type: "m5.large"}) {
		t.Fatalf("keys = %+v", keys)
	}
	got := map[string]report.Container{}
	for _, p := range pods {
		got[p.Workload] = p.Containers[0]
		if p.Static != (p.Workload == "Pod/etcd-n1") {
			t.Errorf("%s: static = %v", p.Workload, p.Static)
		}
	}
	if want := []report.PDB{{Namespace: "shop", Name: "web", Workloads: []string{"Deployment/web"}}}; !reflect.DeepEqual(pdbs, want) {
		t.Errorf("pdbs = %+v, want %+v", pdbs, want)
	}
	for _, p := range pods {
		if want := map[string][]string{"StatefulSet/db": {"data"}}[p.Workload]; !slices.Equal(p.PVCs, want) {
			t.Errorf("%s: PVCs = %v, want %v", p.Workload, p.PVCs, want)
		}
	}
	want := map[string]report.Container{
		"Deployment/web": {Name: "app", AllowPrivEsc: true, Request: report.Resources{MilliCPU: 250}, Limit: report.Resources{MemBytes: 1 << 30},
			Usage: report.Resources{MilliCPU: 40, MemBytes: 100 << 20}, HasUsage: true, ManifestMemLimit: 512 << 20},
		"CronJob/nightly": {Name: "app", AllowPrivEsc: true, Request: report.Resources{MilliCPU: 1000}},
		"StatefulSet/db":  {Name: "app", AllowPrivEsc: true, Request: report.Resources{MilliCPU: 500}, Limit: report.Resources{MilliCPU: 1000, MemBytes: 512 << 20}, OOMKilled: true},
		"Pod/etcd-n1":     {Name: "app", AllowPrivEsc: true, Request: report.Resources{MilliCPU: 100}},
	}
	if len(pods) != 4 || len(got) != len(want) {
		t.Fatalf("pods = %+v", pods)
	}
	for wl, c := range want {
		if !reflect.DeepEqual(got[wl], c) {
			t.Errorf("%s: got %+v, want %+v", wl, got[wl], c)
		}
	}

	wantVols := map[string]report.Volume{
		"pv-io2":      {Name: "pv-io2", Namespace: "shop", Claim: "data", Bytes: 10 << 30, Type: "io2", Class: "fast"},
		"pv-default":  {Name: "pv-default", Namespace: "shop", Claim: "logs", Bytes: 20 << 30, Type: "gp3", Class: "plain"},
		"pv-released": {Name: "pv-released", Bytes: 5 << 30, Type: "gp3", Class: "plain"},
		"pv-host":     {Name: "pv-host", Namespace: "dev", Claim: "scratch", Bytes: 1 << 30, Class: "standard"},
		"pv-noclass":  {Name: "pv-noclass", Bytes: 2 << 30, Class: "gone"},
	}
	if len(vols) != len(wantVols) {
		t.Fatalf("vols = %+v", vols)
	}
	for _, v := range vols {
		if v != wantVols[v.Name] {
			t.Errorf("%s: got %+v, want %+v", v.Name, v, wantVols[v.Name])
		}
	}
	fp := &fakePricer{calls: map[pricing.Key]int{}}
	ebs := ebsPrices(context.Background(), fp, vols, keys, io.Discard)
	if ebs["io2"] != 0.1 || ebs["gp3"] != 0.1 || len(ebs) != 2 {
		t.Errorf("ebs prices = %v", ebs)
	}
	if got := ebsPrices(context.Background(), fp, vols, map[string]pricing.Key{"a": {Provider: "aws", Region: "r1"}, "b": {Provider: "aws", Region: "r2"}}, io.Discard); len(got) != 0 {
		t.Errorf("two regions must leave EBS unpriced, got %v", got)
	}
	st := report.BuildStorage(vols, ebs)
	var so strings.Builder
	renderStorage(&so, st)
	t.Log("\n" + so.String())
	for _, s := range []string{"shop  ", "(unclaimed: no PVC bound)", "dev", "1 unpriced"} {
		if !strings.Contains(so.String(), s) {
			t.Errorf("storage lacks %q:\n%s", s, so.String())
		}
	}

	wantLBs := map[string]string{"classic": "classic", "annot-nlb": "nlb", "class-nlb": "nlb", "other-class": "unknown", "odd-annot": "unknown"}
	if len(lbs) != len(wantLBs) {
		t.Fatalf("lbs = %+v", lbs)
	}
	for _, l := range lbs {
		if l.Namespace != "shop" || l.Kind != wantLBs[l.Name] {
			t.Errorf("lb %+v, want kind %q", l, wantLBs[l.Name])
		}
	}
	lbp := lbPrices(context.Background(), fp, lbs, keys, io.Discard)
	if lbp["nlb"] != 0.027 || lbPrices(context.Background(), fp, lbs, nil, io.Discard) != nil {
		t.Errorf("lb prices = %v", lbp)
	}
	lb := report.BuildLoadBalancers(lbs, lbp)
	var lo strings.Builder
	renderLoadBalancers(&lo, lb)
	// classic 0.03 + 2 nlb 0.027 = 0.084/h = 61.32/month; 2 unknown kinds unpriced.
	for _, s := range []string{"shop", "5", "61.32", "2 unpriced", "LCU and data processed"} {
		if !strings.Contains(lo.String(), s) {
			t.Errorf("load balancers lack %q:\n%s", s, lo.String())
		}
	}

	prices := report.Prices{CPUHour: 0.03, GiBHour: 0.004}
	r := report.Build(nodes, pods, prices)
	var out strings.Builder
	render(&out, r, prices, priceNodes(context.Background(), nil, nodes, keys, prices, io.Discard), samples, 0, false)
	t.Log("\n" + out.String())
	for _, s := range []string{"Deployment/web", "CronJob/nightly", "250m → 48m", "none → 130Mi"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("report lacks %q", s)
		}
	}

	var js strings.Builder
	if err := writeJSON(&js, export{r, "test", prices, nil, samples, "0s", basis(false), time.Unix(0, 0).UTC(), st, lb, nil, nil, nil, report.SecurityReport{}, nil, nil, nil, nil, nil}); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(js.String()), &doc); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"Nodes", "Capacity", "Monthly", "IdleMonthly", "Namespaces", "Workloads", "Recs", "Context", "Prices", "NodePrices", "Samples", "Window", "Basis", "At", "Storage", "LoadBalancers"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("--json lacks %q, which web/src/report.ts reads", k)
		}
	}
}

type fakePricer struct {
	calls map[pricing.Key]int
	fail  string // instance type that errors
}

func (f *fakePricer) EBSMonthly(_ context.Context, _, _ string) (float64, error) { return 0.1, nil }

func (f *fakePricer) EC2Types(_ context.Context, _ string) (map[string]pricing.Type, error) {
	return map[string]pricing.Type{
		"m5.large": {VCPU: 2, MemGiB: 8, Hourly: 0.1}, "c5.large": {VCPU: 2, MemGiB: 4, Hourly: 0.09},
		"t3.medium": {VCPU: 2, MemGiB: 4, Hourly: 0.04}, "m6g.large": {VCPU: 2, MemGiB: 8, Arm: true, Hourly: 0.08},
		"m5.metal": {VCPU: 96, MemGiB: 384, Hourly: 4}, "m7i.metal-24xl": {VCPU: 96, MemGiB: 384, Hourly: 5}, "p3.2xlarge": {VCPU: 8, MemGiB: 61, Hourly: 3},
	}, nil
}

func (f *fakePricer) DataTransferOut(context.Context, string) (float64, error) { return 0.09, nil }

func (f *fakePricer) ELBHourly(_ context.Context, _ string) (map[string]float64, error) {
	return map[string]float64{"classic": 0.03, "nlb": 0.027}, nil
}

func (f *fakePricer) Hourly(_ context.Context, k pricing.Key) (float64, string, error) {
	f.calls[k]++
	if k.Type == f.fail {
		return 0, "", errors.New("boom")
	}
	return 0.2, "fake", nil
}

func TestPriceNodesFallsBackOnError(t *testing.T) {
	flat := report.Prices{CPUHour: 0.03, GiBHour: 0.004}
	c := report.Resources{MilliCPU: 2000, MemBytes: 8 << 30}
	nodes := []report.Node{{Name: "a", Capacity: c}, {Name: "b", Capacity: c}, {Name: "c", Capacity: c}, {Name: "d", Capacity: c}}
	bad := pricing.Key{Provider: "aws", Region: "r", Type: "bad"}
	good := pricing.Key{Provider: "azure", Region: "r", Type: "good"}
	keys := map[string]pricing.Key{"a": bad, "b": bad, "c": good} // d has no key
	f := &fakePricer{calls: map[pricing.Key]int{}, fail: "bad"}
	var warn strings.Builder
	np := priceNodes(context.Background(), f, nodes, keys, flat, &warn)

	if want := "phrontis: no cloud price for aws bad in r: boom; using flat prices\n"; warn.String() != want {
		t.Errorf("warnings = %q, want exactly one line %q", warn.String(), want)
	}
	if f.calls[bad] != 1 || f.calls[good] != 1 {
		t.Errorf("each key must be looked up once: %v", f.calls)
	}
	flatHourly := flat.Monthly(c) / report.HoursPerMonth
	want := []NodePrice{{"a", "flat", flatHourly}, {"b", "flat", flatHourly}, {"c", "fake", 0.2}, {"d", "flat", flatHourly}}
	for i := range want {
		if np[i] != want[i] {
			t.Errorf("np[%d] = %+v, want %+v", i, np[i], want[i])
		}
	}
	if nodes[0].Prices != (report.Prices{}) || nodes[3].Prices != (report.Prices{}) || nodes[2].Prices == (report.Prices{}) {
		t.Errorf("only node c should carry its own prices: %+v", nodes)
	}

	var out strings.Builder
	render(&out, report.Build(nodes, nil, flat), flat, np, 1, 0, false)
	for _, s := range []string{"3 flat", "1 fake", "USD list prices"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("render lacks %q:\n%s", s, out.String())
		}
	}
}

func TestRenderNoRecsHasNoWhy(t *testing.T) {
	var out strings.Builder
	render(&out, report.Report{}, report.Prices{}, nil, 1, 0, false)
	if strings.Contains(out.String(), "WHY") || !strings.Contains(out.String(), "no rightsizing changes") {
		t.Errorf("empty recs:\n%s", out.String())
	}
	out.Reset()
	render(&out, report.Report{Recs: []report.Rec{{Workload: "d/a", Container: "c", Explanation: "because"}}}, report.Prices{}, nil, 1, 0, false)
	if !strings.Contains(out.String(), "\nWHY\n") || !strings.Contains(out.String(), "because") {
		t.Errorf("with recs:\n%s", out.String())
	}
}

func TestRenderNotes(t *testing.T) {
	r := report.Report{Recs: []report.Rec{{Workload: "Deployment/db", Container: "pg", Pods: 1,
		Request: report.Resources{MilliCPU: 180, MemBytes: 256 << 20}, Limit: report.Resources{MilliCPU: 180, MemBytes: 512 << 20},
		Recommended: report.Resources{MilliCPU: 204, MemBytes: 666 << 20}, OOMKilled: true, Throttled: true}}}
	var out strings.Builder
	render(&out, r, report.Prices{}, nil, 2016, 7*24*time.Hour, true)
	for _, s := range []string{"p95 CPU, peak memory of 2016 sample(s) over 168h0m0s\n", "p95 CPU +20%, peak memory +30%",
		"OOM-killed; CPU-throttled; CPU limit 180m → 204m; mem limit 512Mi → 666Mi"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("report lacks %q:\n%s", s, out.String())
		}
	}
}

// svc is a Service in namespace shop; lbClass, lbType and typ are optional, ingress says whether the balancer is provisioned.
func svc(name, lbClass, lbType string, typ corev1.ServiceType, ingress bool) *corev1.Service {
	if typ == "" {
		typ = corev1.ServiceTypeLoadBalancer
	}
	s := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: name}, Spec: corev1.ServiceSpec{Type: typ}}
	if lbClass != "" {
		s.Spec.LoadBalancerClass = &lbClass
	}
	if lbType != "" {
		s.Annotations = map[string]string{"service.beta.kubernetes.io/aws-load-balancer-type": lbType}
	}
	if ingress {
		s.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{Hostname: "x.example"}}
	}
	return s
}

func TestKarpenterSection(t *testing.T) {
	one := map[string]pricing.Key{"n1": {Provider: "aws", Region: "eu-central-1", Type: "m5.large"}}
	offers := nodeOffers(context.Background(), &fakePricer{}, one, io.Discard)
	var got []string
	for _, o := range offers {
		got = append(got, o.Type+"/"+o.Family)
	}
	// Only amd64 c/m/r types: no burstable, GPU, Graviton or metal.
	if strings.Join(got, " ") != "c5.large/c5 m5.large/m5" {
		t.Fatalf("offers = %v", got)
	}
	two := map[string]pricing.Key{"n1": one["n1"], "n2": {Provider: "aws", Region: "us-east-1", Type: "m5.large"}}
	if nodeOffers(context.Background(), &fakePricer{}, two, io.Discard) != nil || nodeOffers(context.Background(), nil, one, io.Discard) != nil {
		t.Error("several regions or no pricer must give no offers")
	}

	mk := func(name, wl string) report.Pod {
		return report.Pod{Namespace: "shop", Name: name, Workload: wl, Containers: []report.Container{{Name: "app", Request: report.Resources{MilliCPU: 500, MemBytes: 256 << 20}}}}
	}
	pods := []report.Pod{mk("web-1", "Deployment/web"), mk("web-2", "Deployment/web"), mk("db-0", "StatefulSet/db")}
	pool := report.BuildNodePool(pods, nil, offers, 100)
	var sb strings.Builder
	y, err := nodePoolYAML(pool)
	if err != nil {
		t.Fatal(err)
	}
	renderKarpenter(&sb, &karpenter{pool, y})
	out := sb.String()
	for _, want := range []string{"KARPENTER", "100.00/month now", "2 × c5.large", "50% of it on spot-safe nodes", "spot savings are in the SPOT section",
		"shop/StatefulSet/db", "kind: NodePool", "values:\n            - c5\n            - m5\n", "- on-demand\n", "name: phrontis-spot\n", "- spot\n", "effect: NoSchedule\n", "weight: 1\n", "- amd64\n",
		"consolidateAfter: 1m", "WhenEmptyOrUnderutilized", "# Set nodeClassRef.name to your EC2NodeClass"} {
		if !strings.Contains(out, want) {
			t.Errorf("section lacks %q:\n%s", want, out)
		}
	}
	// Nothing spot-safe: only on-demand is allowed.
	if y, err := nodePoolYAML(report.BuildNodePool(pods[2:], nil, offers, 0)); err != nil || !strings.Contains(y, "- on-demand\n") || strings.Contains(y, "spot") {
		t.Errorf("yaml = %s", y)
	}
	sb.Reset()
	renderKarpenter(&sb, nil)
	if sb.Len() != 0 {
		t.Error("no simulation must print nothing")
	}
}

func testNode() *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}, Status: corev1.NodeStatus{Capacity: corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}}}
}

func TestCollectNoMetricsServer(t *testing.T) {
	cs := fake.NewClientset(testNode(), pod("web-0", "StatefulSet", "web", "n1", "250m"))
	mc := &metricsfake.Clientset{}
	mc.AddReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("the server could not find the requested resource")
	})
	nodes, pods, _, _, _, _, samples, err := collect(context.Background(), cs, mc, 0)
	if err != nil || samples != 0 || len(pods) != 1 {
		t.Fatalf("err=%v samples=%d pods=%+v", err, samples, pods)
	}
	if c := pods[0].Containers[0]; c.HasUsage || c.Request.MilliCPU != 250 {
		t.Fatalf("container = %+v, want requests only", c)
	}
	r := report.Build(nodes, pods, report.Prices{CPUHour: 0.03, GiBHour: 0.004})
	if len(r.Recs) != 0 {
		t.Errorf("recs without metrics: %+v", r.Recs)
	}
}

func TestCollectWindowTakesMax(t *testing.T) {
	cs := fake.NewClientset(testNode(), pod("web-0", "StatefulSet", "web", "n1", "250m"))
	cpu := []string{"40m", "90m", "20m", "30m"} // one per 30 s sample over a 90 s window
	calls := 0
	mc := &metricsfake.Clientset{}
	mc.AddReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		u := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu[min(calls, len(cpu)-1)]), corev1.ResourceMemory: resource.MustParse("100Mi")}
		calls++
		return true, &metricsv1beta1.PodMetricsList{Items: []metricsv1beta1.PodMetrics{{
			ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web-0"},
			Containers: []metricsv1beta1.ContainerMetrics{{Name: "app", Usage: u}}}}}, nil
	})
	clock, slept := time.Unix(0, 0), time.Duration(0)
	nowFn = func() time.Time { return clock }
	sleepFn = func(d time.Duration) { clock = clock.Add(d); slept += d }
	t.Cleanup(func() { nowFn, sleepFn = time.Now, time.Sleep })

	_, pods, _, _, _, _, samples, err := collect(context.Background(), cs, mc, 90*time.Second)
	if err != nil || samples != 4 || slept != 90*time.Second {
		t.Fatalf("err=%v samples=%d slept=%v", err, samples, slept)
	}
	if c := pods[0].Containers[0]; !c.HasUsage || c.Usage.MilliCPU != 90 || c.Usage.MemBytes != 100<<20 {
		t.Errorf("usage = %+v, want max 90m", c.Usage)
	}
}

// An older ClusterRole without PDB, StatefulSet, DaemonSet or LimitRange access must not break the report: PDBs are just
// absent, and the pod's own limit stands in for the manifest's.
func TestCollectPDBForbidden(t *testing.T) {
	cs := fake.NewClientset(testNode(), pod("web-0", "StatefulSet", "web", "n1", "250m"))
	for _, res := range []string{"poddisruptionbudgets", "statefulsets", "daemonsets", "limitranges"} {
		cs.PrependReactor("list", res, func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: res}, "", errors.New("rbac"))
		})
	}
	_, pods, _, _, _, pdbs, _, err := collect(context.Background(), cs, &metricsfake.Clientset{}, 0)
	if err != nil || len(pods) != 1 || pdbs != nil {
		t.Fatalf("err=%v pods=%d pdbs=%+v", err, len(pods), pdbs)
	}
}

func TestListHPAs(t *testing.T) {
	h := &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "web"},
			Metrics: []autoscalingv2.MetricSpec{{Type: autoscalingv2.ResourceMetricSourceType, Resource: &autoscalingv2.ResourceMetricSource{Name: corev1.ResourceCPU}},
				{Type: autoscalingv2.PodsMetricSourceType}}}}
	var warn strings.Builder
	got := listHPAs(context.Background(), fake.NewClientset(h), &warn)
	want := []report.HPA{{Namespace: "shop", Name: "web", Target: "Deployment/web", Resources: []string{"cpu"}, MinReplicas: 1}}
	if !reflect.DeepEqual(got, want) || warn.Len() > 0 {
		t.Fatalf("got %+v warn %q", got, warn.String())
	}
	// Spec 020: Resource and ContainerResource utilization targets, min and current replicas; averageValue is no target.
	util := func(n corev1.ResourceName, u int32) autoscalingv2.MetricTarget {
		return autoscalingv2.MetricTarget{Type: autoscalingv2.UtilizationMetricType, AverageUtilization: &u}
	}
	min := int32(2)
	h2 := &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api"},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "api"}, MinReplicas: &min, MaxReplicas: 6,
			Metrics: []autoscalingv2.MetricSpec{
				{Type: autoscalingv2.ContainerResourceMetricSourceType, ContainerResource: &autoscalingv2.ContainerResourceMetricSource{Name: corev1.ResourceCPU, Container: "app", Target: util(corev1.ResourceCPU, 50)}},
				{Type: autoscalingv2.ResourceMetricSourceType, Resource: &autoscalingv2.ResourceMetricSource{Name: corev1.ResourceMemory, Target: autoscalingv2.MetricTarget{Type: autoscalingv2.AverageValueMetricType}}}}},
		Status: autoscalingv2.HorizontalPodAutoscalerStatus{CurrentReplicas: 6}}
	got = listHPAs(context.Background(), fake.NewClientset(h2), &warn)
	want = []report.HPA{{Namespace: "shop", Name: "api", Target: "Deployment/api", Resources: []string{"cpu", "memory"}, CPUTarget: 50, CPUContainer: "app", MinReplicas: 2, Replicas: 6, MaxReplicas: 6}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("targets: got %+v", got)
	}
	// Both a Resource and a ContainerResource CPU target: the container one wins, whatever the order.
	for _, order := range []bool{false, true} {
		ms := []autoscalingv2.MetricSpec{
			{Type: autoscalingv2.ContainerResourceMetricSourceType, ContainerResource: &autoscalingv2.ContainerResourceMetricSource{Name: corev1.ResourceCPU, Container: "app", Target: util(corev1.ResourceCPU, 40)}},
			{Type: autoscalingv2.ResourceMetricSourceType, Resource: &autoscalingv2.ResourceMetricSource{Name: corev1.ResourceCPU, Target: util(corev1.ResourceCPU, 55)}}}
		if order {
			ms[0], ms[1] = ms[1], ms[0]
		}
		h3 := h2.DeepCopy()
		h3.Name, h3.Spec.Metrics = "both", ms
		if got := listHPAs(context.Background(), fake.NewClientset(h3), &warn); len(got) != 1 || got[0].CPUTarget != 40 || got[0].CPUContainer != "app" {
			t.Fatalf("both metrics (swapped=%v): %+v", order, got)
		}
	}
	cs := fake.NewClientset()
	cs.PrependReactor("list", "horizontalpodautoscalers", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "horizontalpodautoscalers"}, "", errors.New("rbac"))
	})
	warn.Reset()
	if got := listHPAs(context.Background(), cs, &warn); got != nil || strings.Count(warn.String(), "\n") != 1 {
		t.Fatalf("forbidden: got %+v warn %q", got, warn.String())
	}
}

func TestListLimitRanges(t *testing.T) {
	lr := &corev1.LimitRange{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "l"}, Spec: corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{
		{Type: corev1.LimitTypeContainer, Min: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")}}}}}
	var warn strings.Builder
	got := listLimitRanges(context.Background(), fake.NewClientset(lr), &warn)
	if len(got["shop"]) != 1 || warn.Len() > 0 {
		t.Fatalf("got %+v warn %q", got, warn.String())
	}
	// The whole path: a 1Gi container that uses 10Mi is sized to 32Mi, below the 128Mi minimum.
	s := snapshot{nodes: []report.Node{{Name: "n1", Capacity: report.Resources{MilliCPU: 1000, MemBytes: 4 << 30}}}, limitRanges: got,
		pods: []report.Pod{{Namespace: "shop", Name: "web-1", Workload: "Deployment/web", Node: "n1", Containers: []report.Container{
			{Name: "app", Request: report.Resources{MilliCPU: 100, MemBytes: 1 << 30}, HasUsage: true, Usage: report.Resources{MilliCPU: 80, MemBytes: 10 << 20}}}}}}
	e := assemble(context.Background(), s, report.Prices{CPUHour: 1, GiBHour: 1}, nil, io.Discard, time.Now())
	if len(e.Recs) != 1 || note(e.Recs[0]) != "the namespace's LimitRange rejects this size" {
		t.Fatalf("recs %+v", e.Recs)
	}
	cs := fake.NewClientset()
	cs.PrependReactor("list", "limitranges", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "limitranges"}, "", errors.New("rbac"))
	})
	if got := listLimitRanges(context.Background(), cs, &warn); got != nil || strings.Count(warn.String(), "\n") != 1 {
		t.Fatalf("forbidden: got %+v warn %q", got, warn.String())
	}
}

func TestNoteHPA(t *testing.T) {
	if got := note(report.Rec{HPA: &report.HPARef{Name: "web", Resources: []string{"cpu", "memory"}}}); got != "HPA: web (cpu, memory)" {
		t.Fatal(got)
	}
	adv := report.HPARef{Name: "api", Advice: []report.TargetAdvice{{Resource: "cpu", From: 50, Saving: 120}}}
	if got := note(report.Rec{HPA: &adv}); got != "Raise HPA api CPU target 50% → 70%: saves ~$120/mo" {
		t.Fatal(got)
	}
}

// #147: the manifest's 512Mi comes from a StatefulSet's or a DaemonSet's template, or from the LimitRange default the API
// gives a container whose template sets none. The webhook raised the pod to 1Gi and it was OOM-killed there; the dry-run
// row shows the 1Gi the webhook writes for the next pod, not 1332Mi, as #145 did for ReplicaSets.
func TestDryRunCapBeyondReplicaSets(t *testing.T) {
	tmpl := func(lim string) (t corev1.PodTemplateSpec) {
		t.Spec.Containers = []corev1.Container{{Name: "app"}}
		if lim != "" {
			t.Spec.Containers[0].Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(lim)}
		}
		return t
	}
	om := func(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Namespace: "shop", Name: name} }
	objs := []runtime.Object{testNode(),
		&appsv1.StatefulSet{ObjectMeta: om("db"), Spec: appsv1.StatefulSetSpec{Template: tmpl("512Mi")}},
		&appsv1.DaemonSet{ObjectMeta: om("logs"), Spec: appsv1.DaemonSetSpec{Template: tmpl("512Mi")}},
		&appsv1.ReplicaSet{ObjectMeta: om("web-1"), Spec: appsv1.ReplicaSetSpec{Template: tmpl("")}},
		&corev1.LimitRange{ObjectMeta: om("defaults"), Spec: corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{
			{Type: corev1.LimitTypeContainer, Default: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")}}}}},
	}
	var usage []metricsv1beta1.PodMetrics
	for _, o := range [][2]string{{"StatefulSet", "db"}, {"DaemonSet", "logs"}, {"ReplicaSet", "web-1"}} {
		p := pod(o[1]+"-x", o[0], o[1], "n1", "500m")
		p.Spec.Containers[0].Resources = corev1.ResourceRequirements{ // raised by the webhook
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("1Gi")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")}}
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app",
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"}}}}
		objs = append(objs, p)
		usage = append(usage, metricsv1beta1.PodMetrics{ObjectMeta: om(p.Name), Containers: []metricsv1beta1.ContainerMetrics{{Name: "app",
			Usage: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("1Gi")}}}})
	}
	mc := &metricsfake.Clientset{}
	mc.AddReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &metricsv1beta1.PodMetricsList{Items: usage}, nil
	})
	_, pods, _, _, _, _, _, err := collect(context.Background(), fake.NewClientset(objs...), mc, 0)
	if err != nil {
		t.Fatal(err)
	}
	rep := report.Build(nil, pods, report.Prices{GiBHour: 1})
	if len(rep.Recs) != 3 || len(rep.Sizes) != 3 {
		t.Fatalf("recs %+v sizes %+v", rep.Recs, rep.Sizes)
	}
	sizes := map[string]report.Resources{}
	for _, s := range rep.Sizes {
		sizes[s.Workload] = s.Requests
	}
	for _, x := range rep.Recs { // the next pod starts from the manifest: 500m/256Mi, limited to 1/512Mi; the webhook caps it at 1Gi too (internal/autoscale)
		if x.Recommended.MemBytes != 1<<30 {
			t.Errorf("%s: dry-run %dMi, want 1024Mi", x.Workload, x.Recommended.MemBytes>>20)
		}
	}
}

// P36e: several LimitRanges in one namespace, the larger default listed first: the cap uses the largest, not the last.
func TestOwnerIndexLargestLimitRangeDefault(t *testing.T) {
	lr := func(name, mem string) *corev1.LimitRange {
		return &corev1.LimitRange{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: name}, Spec: corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{
			{Type: corev1.LimitTypeContainer, Default: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(mem)}}}}}
	}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web-1"}}
	rs.Spec.Template.Spec.Containers = []corev1.Container{{Name: "app"}}
	cs := fake.NewClientset(lr("a-big", "1Gi"), lr("b-small", "256Mi"), rs) // fake lists sorted by name: big first
	_, memLim, err := ownerIndex(context.Background(), cs)
	if err != nil {
		t.Fatal(err)
	}
	if got := memLim["shop/ReplicaSet/web-1"]["app"]; got != 1<<30 {
		t.Errorf("manifest limit = %d, want 1Gi (the largest default)", got)
	}
}

// P36e: a read error other than Forbidden on StatefulSets, DaemonSets or LimitRanges is returned and no sample is sent.
func TestCollectReadErrorsAreReturned(t *testing.T) {
	for _, res := range []string{"statefulsets", "daemonsets", "limitranges"} {
		t.Run(res, func(t *testing.T) {
			cs := fake.NewClientset(testNode(), pod("p", "ReplicaSet", "rs", "n1", "100m"))
			cs.PrependReactor("list", res, func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewInternalError(errors.New("etcd down"))
			})
			_, pods, _, _, _, _, _, err := collect(context.Background(), cs, &metricsfake.Clientset{}, 0)
			if err == nil || !strings.Contains(err.Error(), "list "+res) || pods != nil {
				t.Errorf("err=%v pods=%v; want a list %s error and no pods", err, pods, res)
			}
		})
	}
}

// podPages serves 5 pods as pages of 2 through Continue tokens; failAt (page index, -1 = never) returns an error.
func podPages(t *testing.T, failAt int, calls *int) *fake.Clientset {
	cs := fake.NewClientset()
	cs.PrependReactor("list", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		o := a.(k8stesting.ListActionImpl).ListOptions
		if o.FieldSelector != "status.phase=Running" || o.Limit != 500 {
			t.Errorf("selector %q limit %d", o.FieldSelector, o.Limit)
		}
		page := 0
		if o.Continue != "" {
			page = int(o.Continue[0] - '0')
		}
		*calls++
		if page == failAt {
			return true, nil, errors.New("boom")
		}
		l := &corev1.PodList{}
		for i := page * 2; i < min(page*2+2, 5); i++ {
			l.Items = append(l.Items, corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: string(rune('a' + i))}})
		}
		if page < 2 {
			l.Continue = string(rune('0' + page + 1))
		}
		return true, l, nil
	})
	return cs
}

func TestListPagedReadsAllPages(t *testing.T) {
	calls := 0
	cs := podPages(t, -1, &calls)
	pl, err := listPaged[corev1.Pod](context.Background(), func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) {
		return cs.CoreV1().Pods("").List(ctx, o)
	}, metav1.ListOptions{FieldSelector: "status.phase=Running"})
	if err != nil || len(pl) != 5 || calls != 3 {
		t.Fatalf("calls %d err %v pods %d", calls, err, len(pl))
	}
}

func TestListPagedErrorMidway(t *testing.T) {
	calls := 0
	cs := podPages(t, 1, &calls)
	_, err := listPaged[corev1.Pod](context.Background(), func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) {
		return cs.CoreV1().Pods("").List(ctx, o)
	}, metav1.ListOptions{FieldSelector: "status.phase=Running"})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err %v", err)
	}
}

func TestCheckFlags(t *testing.T) {
	defer func() { asCSV, asMD, shareIdle, topN, onlyNS, skipNS = false, false, false, 0, nil, nil }()
	for _, c := range []struct {
		json, noRecs, csv, md, share bool
		top                          int
		ns, skip                     []string
		ok                           bool
	}{
		{ok: true}, {json: true, ok: true}, {noRecs: true, ok: true}, {csv: true, ok: true},
		{csv: true, top: 3, ns: []string{"a"}, ok: true}, {md: true, top: 3, ns: []string{"a"}, ok: true},
		{json: true, csv: true}, {json: true, md: true}, {csv: true, md: true}, {top: -1},
		{json: true, top: 1}, {json: true, ns: []string{"a"}},
		{noRecs: true, top: 1}, {noRecs: true, ns: []string{"a"}}, {noRecs: true, md: true},
		{csv: true, skip: []string{"a"}, ok: true}, {ns: []string{"a"}, skip: []string{"b"}}, {json: true, skip: []string{"a"}}, {noRecs: true, skip: []string{"a"}},
		{share: true, ok: true}, {share: true, json: true, ok: true}, {share: true, csv: true}, {share: true, md: true}, {share: true, noRecs: true},
	} {
		asCSV, asMD, shareIdle, topN, onlyNS, skipNS = c.csv, c.md, c.share, c.top, c.ns, c.skip
		if err := checkFlags(c.json, c.noRecs); (err == nil) != c.ok {
			t.Errorf("%+v: %v", c, err)
		}
	}
}

func TestTop(t *testing.T) {
	defer func() { asCSV, asMD, topN = false, false, 0 }()
	show := func(n int, csv, md bool) string {
		topN, asCSV, asMD = n, csv, md
		var out strings.Builder
		if err := output(context.Background(), &out, demoSnapshot(), report.Prices{CPUHour: 0.03}, nil, false, time.Time{}); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	net := func(s string) string {
		_, s, _ = strings.Cut(s, "\nnet saving ")
		s, _, _ = strings.Cut(s, "/month")
		return s
	}
	lines := strings.SplitAfter(show(0, true, false), "\n") // header, rows, ""
	count := len(lines) - 2
	if count < 3 {
		t.Fatalf("the demo needs 3 recommendations, has %d", count)
	}
	for _, n := range []int{1, 2, count, count + 5} {
		rows := min(n, count)
		if got, want := show(n, true, false), strings.Join(lines[:rows+1], ""); got != want {
			t.Errorf("--top %d --csv:\n%s\nwant\n%s", n, got, want)
		}
		if got := strings.Count(show(n, false, true), "\n"); got != rows+2 {
			t.Errorf("--top %d --markdown: %d lines, want %d", n, got, rows+2)
		}
		all, table := show(0, false, false), show(n, false, false)
		if net(table) == "" || net(table) != net(all) {
			t.Errorf("--top %d: net saving %q, want %q", n, net(table), net(all))
		}
		if got, want := strings.Contains(table, fmt.Sprintf("%d more recommendation(s)", count-rows)), rows < count; got != want {
			t.Errorf("--top %d: says what it left out: %v, want %v", n, got, want)
		}
	}

	// Equal savings: the names decide, not the order of the pods.
	var pods []report.Pod
	for _, n := range []string{"c", "a", "b"} {
		pods = append(pods, report.Pod{Namespace: "shop", Workload: n, Node: "n1", Containers: []report.Container{{Name: "app", HasUsage: true,
			Request: report.Resources{MilliCPU: 1000}, Usage: report.Resources{MilliCPU: 100}}}})
	}
	topN = 2
	got := top(report.Build(nil, pods, report.Prices{CPUHour: 0.03}).Recs)
	if len(got) != 2 || got[0].Workload != "a" || got[1].Workload != "b" {
		t.Errorf("ties: %+v", got)
	}
}

func TestNamespaceFilter(t *testing.T) {
	for v, want := range map[string][]string{"shop": {"shop"}, "shop, kube-system": {"shop", "kube-system"}, "": nil, "shop,": nil} {
		if got, err := parseList("namespace", v); !slices.Equal(got, want) || (err == nil) != (want != nil) {
			t.Errorf("parseList(%q) = %v, %v", v, got, err)
		}
	}

	s := demoSnapshot()
	show := func(csv bool, only ...string) (string, error) {
		onlyNS, asCSV = only, csv
		defer func() { onlyNS, asCSV = nil, false }()
		var out strings.Builder
		err := output(context.Background(), &out, s, report.Prices{CPUHour: 0.03}, nil, false, time.Time{})
		return out.String(), err
	}
	var other, one string
	for _, r := range report.Build(s.nodes, s.pods, report.Prices{CPUHour: 0.03}).Recs {
		if one == "" {
			one = r.Namespace
		} else if r.Namespace != one {
			other = r.Namespace
		}
	}
	if other == "" {
		t.Fatal("the demo needs recommendations in two namespaces")
	}
	full, err := show(true)
	if err != nil || !strings.Contains(full, other+",") {
		t.Fatalf("unfiltered CSV, err %v:\n%s", err, full)
	}
	got, err := show(true, one)
	if err != nil {
		t.Fatal(err)
	}
	var want strings.Builder // the header and the namespace's own rows, figures unchanged
	for i, l := range strings.SplitAfter(full, "\n") {
		if i == 0 || strings.HasPrefix(l, one+",") {
			want.WriteString(l)
		}
	}
	if got != want.String() {
		t.Errorf("CSV of %s:\n%s\nwant\n%s", one, got, want.String())
	}
	table, err := show(false, one)
	if err != nil || strings.Contains(table, "\nSTORAGE") || strings.Contains(table, other+"/") || !strings.Contains(table, one+"/") ||
		!strings.Contains(table, "only namespaces "+one+";") {
		t.Errorf("tables of %s, err %v:\n%s", one, err, table)
	}

	skip := func(csv bool, ns ...string) (string, error) {
		skipNS = ns
		defer func() { skipNS = nil }()
		return show(csv)
	}
	got, err = skip(true, one)
	if err != nil {
		t.Fatal(err)
	}
	var rest strings.Builder // everything but the namespace's own rows
	for _, l := range strings.SplitAfter(full, "\n") {
		if !strings.HasPrefix(l, one+",") {
			rest.WriteString(l)
		}
	}
	if got != rest.String() {
		t.Errorf("CSV without %s:\n%s\nwant\n%s", one, got, rest.String())
	}
	table, err = skip(false, one)
	if err != nil || strings.Contains(table, "\nSTORAGE") || strings.Contains(table, one+"/") || !strings.Contains(table, other+"/") ||
		!strings.Contains(table, "without namespaces "+one+";") {
		t.Errorf("tables without %s, err %v:\n%s", one, err, table)
	}
	// Spec 033: the list and its counter follow the filter; payments/Deployment/worker is OOM-killed and at its CPU limit.
	// The HPA shows the snapshot's HPAs reach the list.
	s.hpas = []report.HPA{{Namespace: "checkout", Name: "web-hpa", Target: "Deployment/web", MinReplicas: 1, Replicas: 4, MaxReplicas: 4}}
	for _, c := range []struct {
		f       func(bool, ...string) (string, error)
		ns      []string
		n       int
		pay, co bool
	}{{show, nil, 3, true, true}, {show, []string{"payments"}, 2, true, false}, {skip, []string{"payments"}, 1, false, true}, {show, []string{"monitoring"}, 0, false, false}} {
		out, err := c.f(false, c.ns...)
		_, list, _ := strings.Cut(out, "\nAT RISK ")
		if err != nil || strings.HasPrefix(list, fmt.Sprintf("(%d)\n", c.n)) != (c.n > 0) || strings.Contains(list, "OOM-killed") != c.pay || strings.Contains(list, "HPA web-hpa at its maximum, 4 replicas") != c.co {
			t.Errorf("AT RISK of %v, err %v:\n%s", c.ns, err, list)
		}
	}
	s.hpas = nil
	if out, err := skip(false, "no-such"); err == nil || out != "" || !strings.Contains(err.Error(), "no-such") {
		t.Errorf("unknown excluded namespace: %q, %v", out, err)
	}
	var all []string
	for _, l := range report.Build(s.nodes, s.pods, report.Prices{}).Namespaces {
		all = append(all, l.Namespace)
	}
	if out, err := skip(false, all...); err == nil || out != "" || !strings.Contains(err.Error(), "every namespace") {
		t.Errorf("all namespaces excluded: %q, %v", out, err)
	}
	shareIdle = true
	defer func() { shareIdle = false }()
	if out, err := show(false); err != nil || !strings.Contains(out, "IDLE SHARE  WITH IDLE") || !strings.Contains(out, "\nidle     shared per node by cost;") {
		t.Errorf("--share-idle, err %v:\n%s", err, out)
	}
	shareIdle = false
	if out, err := show(false, one, "no-such"); err == nil || out != "" || !strings.Contains(err.Error(), "no-such") {
		t.Errorf("unknown namespace: %q, %v", out, err)
	}
}

func TestRunEach(t *testing.T) {
	var out, warn strings.Builder
	err := runEach(&out, &warn, []string{"dead", "prod", "stage"}, func(w io.Writer, c string) error {
		if c == "dead" {
			fmt.Fprintln(w, "half a report")
			return errors.New("no route")
		}
		fmt.Fprintln(w, "report", c)
		return nil
	})
	if err == nil || err.Error() != "1 of 3 contexts failed" {
		t.Error(err)
	}
	if want := "## prod\n\nreport prod\n\n## stage\n\nreport stage\n\n"; out.String() != want {
		t.Errorf("%q", out.String())
	}
	if want := "phrontis: context dead: no route\n"; warn.String() != want {
		t.Errorf("%q", warn.String())
	}
	if err := runEach(io.Discard, io.Discard, []string{"prod"}, func(io.Writer, string) error { return nil }); err != nil {
		t.Error(err)
	}
}
