package cli

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cursopic/phrontis/internal/pricing"
	"github.com/cursopic/phrontis/internal/report"
)

// netServer answers the coverage query with cov seconds and the traffic query with per-pod series: a hostNetwork pod,
// a series without a namespace and an idle pod beside the real ones.
func netServer(t *testing.T, queries *[]string, cov string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		*queries = append(*queries, q)
		if strings.HasPrefix(q, "time()") {
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"`+cov+`"]}]}}`)
			return
		}
		fmt.Fprint(w, promBody(
			[4]string{"shop", "web-1", "", "5368709120"},                                 // 5 GiB
			[4]string{"shop", "web-2", "", "5368709120"},                                 // 5 GiB: shop = 10 GiB
			[4]string{"data", "db-0", "", "21474836480"},                                 // 20 GiB
			[4]string{"kube-system", "proxy-x", "", "107374182400"},                      // hostNetwork: dropped
			[4]string{"kube-system", "kube-proxy-old1z", "", "107374182400"},             // a vanished pod of a host-network DaemonSet: dropped
			[4]string{"kube-system", "kube-proxy-api-7d9f8c6b5-x2k9q", "", "1073741824"}, // live, not host network, same prefix: kept
			[4]string{"", "orphan", "", "107374182400"},                                  // no namespace: ignored
			[4]string{"idle", "i-0", "", "0"}))
	}))
	t.Cleanup(srv.Close)
	return srv
}

var netPods = []report.Pod{{Namespace: "kube-system", Name: "proxy-x", Workload: "DaemonSet/kube-proxy", HostNetwork: true}, {Namespace: "shop", Name: "web-1"}, {Namespace: "kube-system", Name: "kube-proxy-api-7d9f8c6b5-x2k9q"}}

const weekSecs = "604800"

func TestBuildNetwork(t *testing.T) {
	var qs []string
	srv := netServer(t, &qs, weekSecs)
	src := promSource{url: srv.URL, selector: `cluster="prod"`, history: 7 * 24 * time.Hour}
	aws := map[string]pricing.Key{"n1": {Provider: "aws", Region: "eu-central-1", Type: "m5.large"}}
	var warn bytes.Buffer
	got := buildNetwork(context.Background(), src, &fakePricer{calls: map[pricing.Key]int{}}, aws, netPods, &warn)
	if len(qs) != 2 || qs[0] != `sum by (namespace, pod) (increase(container_network_transmit_bytes_total{namespace!="",pod!="",interface!="lo",cluster="prod"}[604800s])) unless on (namespace, pod) kube_pod_info{host_network="true"}` {
		t.Errorf("query = %q", qs)
	}
	perMonth := 730.0 / 168 // 7d window to one month
	want := []networkEgress{{"data", 20, 20 * perMonth * 0.09, "upper-bound"}, {"shop", 10, 10 * perMonth * 0.09, "upper-bound"}, {"kube-system", 1, 1 * perMonth * 0.09, "upper-bound"}}
	if len(got) != 3 {
		t.Fatalf("got %+v", got)
	}
	for i, w := range want {
		g := got[i]
		if g.Namespace != w.Namespace || g.Basis != w.Basis || math.Abs(g.GiB-w.GiB) > 1e-9 || math.Abs(g.Monthly-w.Monthly) > 1e-9 {
			t.Errorf("row %d = %+v, want %+v", i, g, w)
		}
	}
	if len(got) != 3 || got[2].GiB != 1 {
		t.Errorf("host-network (live and vanished) and blank-namespace series must be dropped, the live look-alike kept: %+v", got)
	}
	if warn.Len() != 0 {
		t.Errorf("unexpected warning %q", warn.String())
	}
	var out bytes.Buffer
	renderNetwork(&out, got, "168h0m0s")
	for _, s := range []string{"NETWORK EGRESS, UPPER BOUND", networkCaption, "data", "20.00", "7.82"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("table lacks %q:\n%s", s, out.String())
		}
	}
}

func TestBuildNetworkSkips(t *testing.T) {
	var qs []string
	srv := netServer(t, &qs, weekSecs)
	src := promSource{url: srv.URL, history: 7 * 24 * time.Hour}
	fp := &fakePricer{calls: map[pricing.Key]int{}}
	for name, keys := range map[string]map[string]pricing.Key{
		"gke":         {"n": {Provider: "gcp", Region: "europe-west1", Type: "e2"}},
		"aks":         {"n": {Provider: "azure", Region: "westeurope", Type: "D2"}},
		"no nodes":    nil,
		"two regions": {"a": {Provider: "aws", Region: "eu-west-1"}, "b": {Provider: "aws", Region: "us-east-1"}},
	} {
		var warn bytes.Buffer
		if got := buildNetwork(context.Background(), src, fp, keys, nil, &warn); got != nil || strings.Count(warn.String(), "\n") != 1 || !strings.Contains(warn.String(), "no network egress estimate") {
			t.Errorf("%s: got %v, warning %q", name, got, warn.String())
		}
	}
	if len(qs) != 0 {
		t.Errorf("Prometheus was asked %d time(s) for a cluster that cannot be priced", len(qs))
	}
	var out bytes.Buffer
	renderNetwork(&out, nil, "168h0m0s")
	if out.Len() != 0 {
		t.Errorf("no data must print no section: %q", out.String())
	}
}

// Without --prometheus nothing about the network reaches the JSON or the tables.
func TestNetworkAbsentWithoutPrometheus(t *testing.T) {
	prom = nil
	var warn bytes.Buffer
	e := assemble(context.Background(), demoSnapshot(), report.Prices{CPUHour: 0.03}, nil, &warn, time.Time{})
	var js bytes.Buffer
	if err := writeJSON(&js, e); err != nil {
		t.Fatal(err)
	}
	if e.Network != nil || strings.Contains(js.String(), "Network") || strings.Contains(warn.String(), "network") {
		t.Errorf("network leaked into a report without --prometheus: %v", e.Network)
	}
}

// Prometheus holds 3.5 days of a 7d window: the month is scaled by the hours held, with one warning.
func TestBuildNetworkShortRetention(t *testing.T) {
	var qs []string
	srv := netServer(t, &qs, "302400")
	src := promSource{url: srv.URL, history: 7 * 24 * time.Hour}
	aws := map[string]pricing.Key{"n1": {Provider: "aws", Region: "eu-central-1"}}
	var warn bytes.Buffer
	got := buildNetwork(context.Background(), src, &fakePricer{calls: map[pricing.Key]int{}}, aws, netPods, &warn)
	if len(got) != 3 || math.Abs(got[0].Monthly-20*730.0/84*0.09) > 1e-9 {
		t.Errorf("got %+v", got)
	}
	if strings.Count(warn.String(), "\n") != 1 || !strings.Contains(warn.String(), "holds 3.5 days") {
		t.Errorf("warning %q", warn.String())
	}
}
