package pricing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseAWS(t *testing.T) {
	f, err := os.Open("testdata/aws-eu-central-1.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, ebs, specs, err := parseAWS(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["m5.large"] != 0.115 || got["t3.medium"] != 0.048 {
		t.Fatalf("got %v", got)
	}
	// gp3 in GB-Mo, io2 in GB-month; the Local Zone, Reserved and IOPS-Mo rows are ignored.
	if len(ebs) != 2 || ebs["gp3"] != 0.0952 || ebs["io2"] != 0.149 {
		t.Fatalf("ebs = %v", ebs)
	}
	if specs["m5.large"] != (spec{2, 8, false}) || specs["t3.medium"] != (spec{2, 4, false}) || len(specs) != 2 {
		t.Fatalf("specs = %v", specs)
	}
	if _, _, _, err := parseAWS(strings.NewReader("\"FormatVersion\",\"v1.0\"\n")); err == nil {
		t.Error("no header must be an error")
	}
	if _, _, _, err := parseAWS(strings.NewReader("\"SKU\",\"TermType\"\n")); err == nil {
		t.Error("missing columns must be an error")
	}
	// Two kept rows with different prices for one type.
	hdr := "SKU," + strings.Join(awsCols, ",") + "\n"
	row := func(price string) string { return "x,OnDemand,Linux,Shared,NA,Used,Hrs,USD," + price + ",m5.large\n" }
	if _, _, _, err := parseAWS(strings.NewReader(hdr + row("0.1") + row("0.1"))); err != nil {
		t.Errorf("identical duplicate is fine: %v", err)
	}
	if _, _, _, err := parseAWS(strings.NewReader(hdr + row("0.1") + row("0.2"))); err == nil {
		t.Error("conflicting duplicate must be an error")
	}
	// A bad or conflicting EBS row leaves only that volume type unpriced; instances still price.
	vol := func(v, price string) string {
		return "x,OnDemand,,,,,GB-Mo,USD," + price + ",,Storage," + v + ",AWS Region\n"
	}
	got, ebs, _, err = parseAWS(strings.NewReader(hdr + row("0.1") + vol("gp3", "0.08") + vol("gp3", "0.09") + vol("st1", "n/a") + vol("sc1", "0.018")))
	if err != nil || got["m5.large"] != 0.1 || len(ebs) != 1 || ebs["sc1"] != 0.018 {
		t.Errorf("got %v ebs %v err %v", got, ebs, err)
	}
}

func TestAWSClient(t *testing.T) {
	var hits atomic.Int32
	var fail atomic.Bool
	csv := fixture(t, "aws-eu-central-1.csv")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if fail.Load() {
			http.Error(w, "down", 500)
			return
		}
		if r.URL.Path != "/offers/v1.0/aws/AmazonEC2/current/eu-central-1/index.csv" {
			http.NotFound(w, r)
			return
		}
		w.Write(csv)
	}))
	defer srv.Close()
	dir, now := t.TempDir(), time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	mk := func() *Client {
		return &Client{AWSBase: srv.URL, CacheDir: dir, Now: func() time.Time { return now }}
	}
	ctx := context.Background()
	k := Key{"aws", "eu-central-1", "m5.large", false}

	c := mk()
	if p, src, err := c.Hourly(ctx, k); err != nil || p != 0.115 || src != "aws on-demand" {
		t.Fatalf("on-demand = %v %q %v", p, src, err)
	}
	k.Spot = true
	if p, src, err := c.Hourly(ctx, k); err != nil || p != 0.115 || !strings.HasPrefix(src, "aws spot (priced as on-demand") {
		t.Fatalf("spot = %v %q %v", p, src, err)
	}
	if hits.Load() != 1 {
		t.Fatalf("one run must download a region once, got %d", hits.Load())
	}
	if _, err := os.Stat(dir + "/aws-ec2-eu-central-1.json"); err != nil {
		t.Fatalf("cache not written: %v", err)
	}

	fail.Store(true) // a new Client answers from the disk cache
	if p, _, err := mk().Hourly(ctx, k); err != nil || p != 0.115 {
		t.Fatalf("cached = %v %v", p, err)
	}
	if p, err := mk().EBSMonthly(ctx, "eu-central-1", "gp3"); err != nil || p != 0.0952 {
		t.Fatalf("ebs gp3 = %v %v", p, err)
	}
	if _, err := mk().EBSMonthly(ctx, "eu-central-1", "sc1"); err == nil {
		t.Error("a volume type with no row must be an error")
	}
	ty, err := mk().EC2Types(ctx, "eu-central-1")
	if err != nil || len(ty) != 2 || ty["m5.large"] != (Type{2, 8, false, 0.115}) {
		t.Fatalf("EC2Types = %v %v", ty, err)
	}
	// A cache with prices and EBS but no specs predates node simulation: it must refetch (server is down here).
	if err := os.WriteFile(dir+"/aws-ec2-eu-central-1.json", []byte(`{"FetchedAt":"2026-11-12T00:00:00Z","Prices":{"m5.large":0.115},"EBS":{"gp3":0.1}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := mk().EC2Types(ctx, "eu-central-1"); err == nil {
		t.Error("cache without specs must be stale")
	}
	// A cache written before EBS pricing existed has no EBS rows: it must refetch, not answer.
	if err := os.WriteFile(dir+"/aws-ec2-eu-central-1.json", []byte(`{"FetchedAt":"2026-11-12T00:00:00Z","Prices":{"m5.large":0.115}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := mk().EBSMonthly(ctx, "eu-central-1", "gp3"); err == nil {
		t.Error("old cache without EBS must be stale (server is down here)")
	}
	now = now.Add(25 * time.Hour) // stale: refetch, and the server is down
	if _, _, err := mk().Hourly(ctx, k); err == nil {
		t.Fatal("stale cache must refetch")
	}
	fail.Store(false)
	before := hits.Load()
	if _, _, err := mk().Hourly(ctx, k); err != nil || hits.Load() != before+1 {
		t.Fatalf("refetch: err=%v hits=%d->%d", err, before, hits.Load())
	}

	before = hits.Load()
	for _, bad := range []Key{{"aws", "eu/../x", "m5.large", false}, {"azure", "eu", "a b", false}, {"aws", "EU", "m5.large", false}} {
		if _, _, err := mk().Hourly(ctx, bad); err == nil {
			t.Errorf("%+v must be rejected", bad)
		}
	}
	if hits.Load() != before {
		t.Error("a rejected key must not reach the network")
	}
}

func TestAzureClient(t *testing.T) {
	body := string(fixture(t, "azure-westeurope-Standard_D2s_v5.json"))
	var filters []string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/retail/prices" {
			http.NotFound(w, r)
			return
		}
		filters = append(filters, r.URL.Query().Get("$filter"))
		w.Write([]byte(body))
	}))
	defer srv.Close()
	c := &Client{AzureBase: srv.URL}
	ctx := context.Background()

	k := Key{"azure", "westeurope", "Standard_D2s_v5", false}
	if p, src, err := c.Hourly(ctx, k); err != nil || p != 0.115 || src != "azure on-demand" {
		t.Fatalf("on-demand = %v %q %v", p, src, err)
	}
	if want := "serviceName eq 'Virtual Machines' and armRegionName eq 'westeurope' and armSkuName eq 'Standard_D2s_v5' and priceType eq 'Consumption'"; filters[0] != want {
		t.Errorf("filter = %q", filters[0])
	}
	k.Spot = true
	if p, src, err := c.Hourly(ctx, k); err != nil || p != 0.021252 || src != "azure spot" {
		t.Fatalf("spot = %v %q %v", p, src, err)
	}
	if _, _, err := c.Hourly(ctx, Key{"gcp", "europe-west1", "e2-medium", false}); err == nil || !strings.Contains(err.Error(), "API key") {
		t.Errorf("gcp must say it is not built: %v", err)
	}
}

func TestAzurePaging(t *testing.T) {
	full := string(fixture(t, "azure-westeurope-Standard_D2s_v5.json"))
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			w.Write([]byte(full))
			return
		}
		w.Write([]byte(`{"Items":[],"NextPageLink":"` + srv.URL + `/api/retail/prices?page=2"}`))
	}))
	defer srv.Close()
	c := &Client{AzureBase: srv.URL}
	if p, _, err := c.Hourly(context.Background(), Key{"azure", "westeurope", "Standard_D2s_v5", false}); err != nil || p != 0.115 {
		t.Fatalf("paged = %v %v", p, err)
	}
	if _, _, err := c.Hourly(context.Background(), Key{"azure", "westeurope", "Standard_D2s_v5", true}); err != nil {
		t.Fatal(err)
	}
}

func TestKeyOf(t *testing.T) {
	std := map[string]string{"node.kubernetes.io/instance-type": "m5.large", "topology.kubernetes.io/region": "eu-central-1"}
	with := func(kv ...string) map[string]string {
		m := map[string]string{}
		for k, v := range std {
			m[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	for _, tc := range []struct {
		name, pid string
		labels    map[string]string
		want      Key
		ok        bool
	}{
		{"aws", "aws:///eu-central-1a/i-1", std, Key{"aws", "eu-central-1", "m5.large", false}, true},
		{"linux label", "aws:///x", with("kubernetes.io/os", "linux"), Key{"aws", "eu-central-1", "m5.large", false}, true},
		{"windows", "aws:///x", with("kubernetes.io/os", "windows"), Key{}, false},
		{"beta labels", "azure:///subscriptions/x", map[string]string{"beta.kubernetes.io/instance-type": "Standard_D2s_v5", "failure-domain.beta.kubernetes.io/region": "westeurope"}, Key{"azure", "westeurope", "Standard_D2s_v5", false}, true},
		{"gce", "gce://p/z/n", std, Key{"gcp", "eu-central-1", "m5.large", false}, true},
		{"unknown provider", "kind://docker/x", std, Key{}, false},
		{"no type", "aws:///x", map[string]string{"topology.kubernetes.io/region": "r"}, Key{}, false},
		{"no region", "aws:///x", map[string]string{"node.kubernetes.io/instance-type": "t"}, Key{}, false},
		{"karpenter spot", "aws:///x", with("karpenter.sh/capacity-type", "spot"), Key{"aws", "eu-central-1", "m5.large", true}, true},
		{"karpenter on-demand", "aws:///x", with("karpenter.sh/capacity-type", "on-demand"), Key{"aws", "eu-central-1", "m5.large", false}, true},
		{"eks spot", "aws:///x", with("eks.amazonaws.com/capacityType", "SPOT"), Key{"aws", "eu-central-1", "m5.large", true}, true},
		{"gke spot", "gce://p/z/n", with("cloud.google.com/gke-spot", "true"), Key{"gcp", "eu-central-1", "m5.large", true}, true},
		{"aks spot", "azure:///x", with("kubernetes.azure.com/scalesetpriority", "spot"), Key{"azure", "eu-central-1", "m5.large", true}, true},
	} {
		got, ok := KeyOf(tc.pid, tc.labels)
		if ok != tc.ok || got != tc.want {
			t.Errorf("%s: got %+v %v, want %+v %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestParseELB(t *testing.T) {
	f, err := os.Open("testdata/aws-elb-eu-central-1.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, err := parseELB(f)
	// Application, Gateway, Outposts and LCU-Hrs rows are ignored.
	if err != nil || len(got) != 2 || got["classic"] != 0.03 || got["nlb"] != 0.027 {
		t.Fatalf("got %v %v", got, err)
	}
	if _, err := parseELB(strings.NewReader("\"x\"\n")); err == nil {
		t.Error("no header must be an error")
	}
	if _, err := parseELB(strings.NewReader("\"SKU\"\n")); err == nil {
		t.Error("missing columns must be an error")
	}
	hdr := "SKU," + strings.Join(elbCols, ",") + "\n"
	row := func(op, price string) string { return "x,OnDemand,Hrs," + price + ",USD,AWS Region," + op + "\n" }
	got, err = parseELB(strings.NewReader(hdr + row("LoadBalancing", "0.03") + row("LoadBalancing", "0.04") + row("LoadBalancing:Network", "n/a") + row("LoadBalancing:Network", "0.02") + row("LoadBalancing:Network", "0.02")))
	if err != nil || len(got) != 0 {
		t.Errorf("conflicting or bad rows leave the kind unpriced: %v %v", got, err)
	}
	got, err = parseELB(strings.NewReader(hdr + row("LoadBalancing", "0.03") + row("LoadBalancing", "0.03") + row("LoadBalancing:Network", "0.02")))
	if err != nil || got["classic"] != 0.03 || got["nlb"] != 0.02 {
		t.Errorf("identical duplicates are fine: %v %v", got, err)
	}
	got, err = parseELB(strings.NewReader(hdr + row("LoadBalancing", "Inf") + row("LoadBalancing:Network", "NaN")))
	if err != nil || len(got) != 0 {
		t.Errorf("non-finite prices leave the kind unpriced: %v %v", got, err)
	}
}

func TestELBHourly(t *testing.T) {
	csv := fixture(t, "aws-elb-eu-central-1.csv")
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/offers/v1.0/aws/AWSELB/current/eu-central-1/index.csv" {
			http.NotFound(w, r)
			return
		}
		w.Write(csv)
	}))
	defer srv.Close()
	c := &Client{AWSBase: srv.URL}
	for i := 0; i < 2; i++ {
		if m, err := c.ELBHourly(context.Background(), "eu-central-1"); err != nil || m["nlb"] != 0.027 {
			t.Fatalf("got %v %v", m, err)
		}
	}
	if hits.Load() != 1 {
		t.Errorf("a region downloads once per run, got %d", hits.Load())
	}
	if _, err := c.ELBHourly(context.Background(), "eu/../x"); err == nil {
		t.Error("odd region must be rejected")
	}
}

// Concurrent cold-cache callers share one download, and a reader of the disk cache never sees a partial file.
func TestAWSClientConcurrent(t *testing.T) {
	var hits atomic.Int32
	csv := fixture(t, "aws-eu-central-1.csv")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(50 * time.Millisecond) // keep every caller waiting on the first download
		w.Write(csv)
	}))
	defer srv.Close()
	dir := t.TempDir()
	c := &Client{AWSBase: srv.URL, CacheDir: dir}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p, _, err := c.Hourly(context.Background(), Key{"aws", "eu-central-1", "m5.large", false}); err != nil || p != 0.115 {
				t.Errorf("hourly = %v, %v", p, err)
			}
		}()
	}
	wg.Wait()
	if hits.Load() != 1 {
		t.Errorf("downloads = %d, want 1", hits.Load())
	}
	if es, _ := os.ReadDir(dir); len(es) != 1 || es[0].Name() != "aws-ec2-eu-central-1.json" {
		t.Errorf("cache dir has leftovers: %v", es)
	}
}
