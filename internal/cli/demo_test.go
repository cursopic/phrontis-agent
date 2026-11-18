package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/cursopic/phrontis/internal/pricing"
	"github.com/cursopic/phrontis/internal/report"
)

var update = flag.Bool("update", false, "rewrite web/public/report.json from the demo snapshot")

const demoPath = "../../web/public/report.json"

// demoSnapshot is a made-up, scheduler-valid cluster: six m5.large nodes in eu-central-1, requests within every node.
// Usage above requests overcommits node-a's CPU (nightly-etl bursts), so the demo shows the fitted charges of spec 001.
func demoSnapshot() snapshot {
	const mi, gi = 1 << 20, 1 << 30
	m5 := report.Resources{MilliCPU: 2000, MemBytes: 8 * gi}
	names := []string{"node-a", "node-b", "node-c", "node-d", "node-e", "node-f"}
	var nodes []report.Node
	keys := map[string]pricing.Key{}
	for _, n := range names {
		nodes = append(nodes, report.Node{Name: n, Capacity: m5})
		keys[n] = pricing.Key{Provider: "aws", Region: "eu-central-1", Type: "m5.large"}
	}
	res := func(milli, mem int64) report.Resources { return report.Resources{MilliCPU: milli, MemBytes: mem} }
	type c = report.Container
	pod := func(ns, name, wl, node string, cs ...c) report.Pod {
		return report.Pod{Namespace: ns, Name: name, Workload: wl, Node: node, Containers: cs}
	}
	api := c{Name: "api", Request: res(1000, 2*gi), Usage: res(180, 600*mi), HasUsage: true}
	web := c{Name: "nginx", Request: res(500, 512*mi), Usage: res(60, 120*mi), HasUsage: true}
	worker := c{Name: "worker", Request: res(250, 256*mi), Limit: res(300, 384*mi), Usage: res(290, 380*mi), HasUsage: true, OOMKilled: true}
	dns := c{Name: "coredns", Request: res(100, 70*mi), Usage: res(4, 25*mi), HasUsage: true}
	pods := []report.Pod{
		pod("checkout", "api-7d9f-a", "Deployment/api", "node-a", api),
		pod("checkout", "api-7d9f-b", "Deployment/api", "node-b", api),
		pod("checkout", "api-7d9f-c", "Deployment/api", "node-c", api),
		pod("checkout", "web-5c4b-a", "Deployment/web", "node-d", web),
		pod("checkout", "web-5c4b-b", "Deployment/web", "node-e", web),
		pod("payments", "ledger-db-0", "StatefulSet/ledger-db", "node-f", c{Name: "postgres", Request: res(1500, 6*gi), Usage: res(1300, 5632*mi), HasUsage: true}),
		pod("payments", "worker-6f8d-a", "Deployment/worker", "node-d", worker),
		pod("payments", "worker-6f8d-b", "Deployment/worker", "node-e", worker),
		pod("data", "nightly-etl-29170", "CronJob/nightly-etl", "node-a", c{Name: "etl", Request: res(700, 3*gi), Usage: res(1300, 3584*mi), HasUsage: true}),
		pod("data", "kafka-connect-0", "Deployment/kafka-connect", "node-b", c{Name: "connect", Request: res(800, 4*gi), Usage: res(150, 1200*mi), HasUsage: true}),
		pod("monitoring", "prometheus-0", "StatefulSet/prometheus", "node-c", c{Name: "prometheus", Request: res(800, 4*gi), Usage: res(400, 2900*mi), HasUsage: true}),
		pod("monitoring", "grafana-84b7", "Deployment/grafana", "node-d", c{Name: "grafana", Request: res(200, 512*mi), Usage: res(20, 150*mi), HasUsage: true}),
		pod("kube-system", "coredns-a", "Deployment/coredns", "node-d", dns),
		pod("kube-system", "coredns-b", "Deployment/coredns", "node-e", dns),
		{Namespace: "kube-system", Name: "etcd-node-a", Workload: "Pod/etcd-node-a", Node: "node-a", Static: true,
			Containers: []c{{Name: "etcd", Request: res(100, 100*mi), Usage: res(30, 80*mi), HasUsage: true}}},
	}
	for _, n := range names {
		pods = append(pods, pod("kube-system", "kube-proxy-"+n, "DaemonSet/kube-proxy", n, c{Name: "kube-proxy", Request: res(100, 64*mi), Usage: res(5, 30*mi), HasUsage: true}))
	}
	vol := func(name, ns, claim string, gib int64, typ string) report.Volume {
		return report.Volume{Name: name, Namespace: ns, Claim: claim, Bytes: gib * gi, Type: typ}
	}
	vols := []report.Volume{
		vol("pv-ledger", "payments", "data-ledger-db-0", 200, "gp3"),
		vol("pv-kafka", "data", "kafka-connect-offsets", 400, "gp3"),
		vol("pv-etl", "data", "etl-scratch", 200, "gp3"),
		vol("pv-prom", "monitoring", "data-prometheus-0", 100, "gp3"),
		vol("pv-uploads", "checkout", "uploads", 50, "gp3"),
		vol("pv-old-1", "", "", 200, "gp3"), // Released: billed, used by nobody
		vol("pv-old-2", "", "", 100, "gp3"),
	}
	lbs := []report.LoadBalancer{{Name: "web", Namespace: "checkout", Kind: "classic"}, {Name: "api", Namespace: "checkout", Kind: "nlb"}, {Name: "grafana", Namespace: "monitoring", Kind: "unknown"}}
	pdbs := []report.PDB{{Namespace: "checkout", Name: "api", Workloads: []string{"Deployment/api"}}, {Namespace: "checkout", Name: "web", Workloads: []string{"Deployment/web"}},
		{Namespace: "kube-system", Name: "coredns", Workloads: []string{"Deployment/coredns"}}} // worker has two pods but no PDB: it stays on-demand
	return snapshot{kctx: "sample", nodes: nodes, pods: pods, keys: keys, volumes: vols, lbs: lbs, samples: 1, pdbs: pdbs}
}

// demoSpot is a made-up spot price for the made-up demo cluster: a flat 0.04 per hour for any type.
type demoSpot struct{}

func (demoSpot) DescribeSpotPriceHistory(context.Context, *ec2.DescribeSpotPriceHistoryInput, ...func(*ec2.Options)) (*ec2.DescribeSpotPriceHistoryOutput, error) {
	return &ec2.DescribeSpotPriceHistoryOutput{SpotPriceHistory: []types.SpotPrice{{SpotPrice: aws.String("0.04")}}}, nil
}

// TestDemoReport keeps web/public/report.json equal to the demo snapshot priced by the AWS price-list fixtures in
// internal/pricing/testdata. After a change to the math or the export: go test ./internal/cli -run TestDemoReport -update
func TestDemoReport(t *testing.T) {
	files := map[string]string{
		"/offers/v1.0/aws/AmazonEC2/current/eu-central-1/index.csv": "../../internal/pricing/testdata/aws-eu-central-1.csv",
		"/offers/v1.0/aws/AWSELB/current/eu-central-1/index.csv":    "../../internal/pricing/testdata/aws-elb-eu-central-1.csv",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f, ok := files[r.URL.Path]; ok {
			http.ServeFile(w, r, f)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	var warn strings.Builder
	flat := report.Prices{CPUHour: 0.03, GiBHour: 0.004}
	at := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	e := assemble(context.Background(), demoSnapshot(), flat, &pricing.Client{AWSBase: srv.URL, Spot: demoSpot{}, Now: func() time.Time { return at }}, &warn, at)
	if warn.Len() > 0 || e.NodePool == nil || e.NodePool.Unplaced > 0 || e.Storage.Total.Unpriced > 0 {
		t.Fatalf("demo must be fully priced: warnings %q, node pool %v, unpriced volumes %d", warn.String(), e.NodePool, e.Storage.Total.Unpriced)
	}
	if e.Spot != nil {
		e.Spot.Note = "made-up demo spot price, not an AWS quote" // the demo must never read as a real AWS lookup
	}
	ns := 0.0
	for _, l := range e.Namespaces {
		ns += l.Monthly
	}
	if math.Abs(ns+e.IdleMonthly-e.Monthly) > 1e-6 {
		t.Errorf("namespaces %.4f + idle %.4f != cluster %.4f", ns, e.IdleMonthly, e.Monthly)
	}

	var b bytes.Buffer
	if err := writeJSON(&b, e); err != nil {
		t.Fatal(err)
	}
	if *update {
		if err := os.WriteFile(demoPath, b.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(demoPath)
	var want, have any
	if err == nil {
		err = json.Unmarshal(got, &want)
	}
	if err == nil {
		err = json.Unmarshal(b.Bytes(), &have)
	}
	if err != nil || !near(want, have) {
		t.Errorf("%s is out of date (%v): go test ./internal/cli -run TestDemoReport -update", demoPath, err)
	}
}

// near compares decoded JSON, numbers within 1e-6: float last bits differ by CPU (arm64 fuses multiply-add, amd64 does not).
func near(a, b any) bool {
	switch a := a.(type) {
	case float64:
		b, ok := b.(float64)
		return ok && math.Abs(a-b) <= 1e-6*math.Max(1, math.Abs(a))
	case map[string]any:
		b, ok := b.(map[string]any)
		if !ok || len(a) != len(b) {
			return false
		}
		for k, v := range a {
			if w, ok := b[k]; !ok || !near(v, w) {
				return false
			}
		}
		return true
	case []any:
		b, ok := b.([]any)
		if !ok || len(a) != len(b) {
			return false
		}
		for i := range a {
			if !near(a[i], b[i]) {
				return false
			}
		}
		return true
	}
	return a == b
}

func TestUnlistedNodeWarns(t *testing.T) {
	s := snapshot{nodes: []report.Node{{Name: "n1", Capacity: report.Resources{MilliCPU: 1000}}}, pods: []report.Pod{
		{Namespace: "a", Name: "p1", Workload: "Pod/p1", Node: "n1"}, {Namespace: "a", Name: "p2", Workload: "Pod/p2", Node: "gone"}, {Namespace: "a", Name: "p3", Workload: "Pod/p3", Node: "gone"}}}
	var warn strings.Builder
	assemble(context.Background(), s, report.Prices{CPUHour: 0.03}, nil, &warn, time.Time{})
	if want := "phrontis: 2 pod(s) on nodes missing from the node list are costed at flat prices, on top of the cluster total\n"; warn.String() != want {
		t.Errorf("warnings = %q, want %q", warn.String(), want)
	}
}

// A pricing outage still answers, but the JSON says the prices are flat and why.
func TestFallbackVisibleInJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "down", 500) }))
	defer srv.Close()
	s := snapshot{nodes: []report.Node{{Name: "n1", Capacity: report.Resources{MilliCPU: 1000}}},
		keys: map[string]pricing.Key{"n1": {Provider: "aws", Region: "eu-central-1", Type: "m5.large"}}}
	e := assemble(context.Background(), s, report.Prices{CPUHour: 0.03}, &pricing.Client{AWSBase: srv.URL}, io.Discard, time.Time{})
	var b strings.Builder
	if err := writeJSON(&b, e); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Warnings   []string
		NodePrices []NodePrice
	}
	if err := json.Unmarshal([]byte(b.String()), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Warnings) == 0 || !strings.Contains(doc.Warnings[0], "using flat prices") || doc.NodePrices[0].Source != "flat" {
		t.Errorf("fallback not visible: warnings %q, node prices %v", doc.Warnings, doc.NodePrices)
	}
}
