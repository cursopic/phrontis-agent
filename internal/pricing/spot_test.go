package pricing

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// fakeSpot answers DescribeSpotPriceHistory from prices (zone -> price) and records each call's input and region.
type fakeSpot struct {
	prices  []string
	err     error
	calls   int
	in      *ec2.DescribeSpotPriceHistoryInput
	regions []string
}

func (f *fakeSpot) DescribeSpotPriceHistory(_ context.Context, in *ec2.DescribeSpotPriceHistoryInput, opts ...func(*ec2.Options)) (*ec2.DescribeSpotPriceHistoryOutput, error) {
	f.calls++
	f.in = in
	var o ec2.Options
	for _, fn := range opts {
		fn(&o)
	}
	f.regions = append(f.regions, o.Region)
	if f.err != nil {
		return nil, f.err
	}
	out := &ec2.DescribeSpotPriceHistoryOutput{}
	for _, p := range f.prices {
		out.SpotPriceHistory = append(out.SpotPriceHistory, types.SpotPrice{SpotPrice: aws.String(p), InstanceType: types.InstanceType(in.InstanceTypes[0])})
	}
	return out, nil
}

func TestSpotHourlyCachesOneHour(t *testing.T) {
	now := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	f := &fakeSpot{prices: []string{"0.0350", "0.0412", "0.0390"}}
	c := &Client{Spot: f, Now: func() time.Time { return now }}
	k := Key{"aws", "eu-central-1", "m5.large", true}

	p := c.SpotHourly(context.Background(), k)
	if p.Reason != "" || p.Hourly != 0.0412 || !p.At.Equal(now) {
		t.Fatalf("got %+v, want the highest zone price at now", p)
	}
	if p.Label() != "spot (highest zone), price at 2026-11-16 10:00 UTC" {
		t.Errorf("label = %q", p.Label())
	}
	in := f.in
	if f.regions[0] != "eu-central-1" || in.InstanceTypes[0] != "m5.large" || in.ProductDescriptions[0] != "Linux/UNIX" ||
		!in.StartTime.Equal(now) || !in.EndTime.Equal(now) {
		t.Errorf("request = %+v in %v", in, f.regions)
	}

	now = now.Add(SpotTTL - time.Second)
	c.SpotHourly(context.Background(), k)
	if f.calls != 1 {
		t.Fatalf("within the hour: %d calls, want 1", f.calls)
	}
	now = now.Add(time.Second)
	f.prices = []string{"0.05"}
	if p := c.SpotHourly(context.Background(), k); f.calls != 2 || p.Hourly != 0.05 {
		t.Fatalf("after the hour: %d calls, %+v", f.calls, p)
	}
}

func TestSpotNoPriceHasReasonNotMoney(t *testing.T) {
	now := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	ctx := context.Background()
	k := Key{"aws", "eu-central-1", "m5.large", true}

	none := &fakeSpot{}
	c := &Client{Spot: none, Now: func() time.Time { return now }}
	if p := c.SpotHourly(ctx, k); p.Hourly != 0 || !strings.Contains(p.Reason, "no Linux spot price") {
		t.Errorf("no offer = %+v", p)
	}
	c.SpotHourly(ctx, k)
	if none.calls != 1 {
		t.Errorf("a missing offer is cached for the hour: %d calls", none.calls)
	}

	failing := &fakeSpot{err: errors.New("throttled")}
	c = &Client{Spot: failing, Now: func() time.Time { return now }}
	if p := c.SpotHourly(ctx, k); p.Hourly != 0 || p.Reason != "spot price lookup failed" {
		t.Errorf("failed call = %+v; the reason must not carry the AWS error", p)
	}
	now = now.Add(5*time.Minute - time.Second)
	c.SpotHourly(ctx, k)
	if failing.calls != 1 {
		t.Errorf("a failure is reused for 5 min: %d calls", failing.calls)
	}
	now = now.Add(time.Second)
	c.SpotHourly(ctx, k)
	if failing.calls != 2 {
		t.Errorf("after 5 min a failure is retried: %d calls", failing.calls)
	}

	if p := (&Client{}).SpotHourly(ctx, k); p.Hourly != 0 || p.Reason == "" {
		t.Errorf("no source = %+v", p)
	}
	if p := c.SpotHourly(ctx, Key{"aws", "eu-central-1", "m5.large&x", true}); p.Reason == "" || failing.calls != 2 {
		t.Errorf("odd type must not reach AWS: %+v", p)
	}
}

// TestHourlySpotNode prices a spot node at spot with the label, and falls back to on-demand with the reason.
func TestHourlySpotNode(t *testing.T) {
	csv := fixture(t, "aws-eu-central-1.csv")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(csv) }))
	defer srv.Close()
	now := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	f := &fakeSpot{prices: []string{"0.04"}}
	c := &Client{AWSBase: srv.URL, Spot: f, Now: func() time.Time { return now }}
	k := Key{"aws", "eu-central-1", "m5.large", true}
	if p, src, err := c.Hourly(context.Background(), k); err != nil || p != 0.04 || src != "aws spot (highest zone), price at 2026-11-16 10:00 UTC" {
		t.Fatalf("spot = %v %q %v", p, src, err)
	}
	c = &Client{AWSBase: srv.URL, Spot: &fakeSpot{}, Now: func() time.Time { return now }}
	if p, src, err := c.Hourly(context.Background(), k); err != nil || p != 0.115 || !strings.Contains(src, "no Linux spot price") {
		t.Fatalf("fallback = %v %q %v", p, src, err)
	}
}

// TestSpotKeyNeverLogged runs the real SDK client from env credentials against a local stand-in for EC2 (no AWS call)
// and checks the secret is in no log line, reason or price source.
func TestSpotKeyNeverLogged(t *testing.T) {
	const id, secret = "phrontis-test-key-id", "phrontis-test-secret-do-not-log-0123456789"
	var signed int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), id) {
			signed++
		}
		r.ParseForm()
		if r.Form.Get("InstanceType.1") != "m5.large" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`<Response><Errors><Error><Code>AuthFailure</Code><Message>AWS was not able to validate the provided access credentials</Message></Error></Errors><RequestID>x</RequestID></Response>`))
			return
		}
		w.Header().Set("Content-Type", "text/xml")
		w.Write([]byte(`<DescribeSpotPriceHistoryResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><requestId>x</requestId><spotPriceHistorySet>` +
			`<item><instanceType>m5.large</instanceType><productDescription>Linux/UNIX</productDescription><spotPrice>0.0412</spotPrice>` +
			`<timestamp>2026-11-16T09:12:00.000Z</timestamp><availabilityZone>eu-central-1a</availabilityZone></item></spotPriceHistorySet><nextToken/></DescribeSpotPriceHistoryResponse>`))
	}))
	defer srv.Close()
	dir := t.TempDir() // no shared files and no instance role: only the env key below
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_ACCESS_KEY_ID", id)
	t.Setenv("AWS_SECRET_ACCESS_KEY", secret)
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_ENDPOINT_URL_EC2", srv.URL)
	t.Setenv("AWS_MAX_ATTEMPTS", "1")

	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(old)

	ctx := context.Background()
	api, err := NewSpotAPI(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{Spot: api, Now: func() time.Time { return time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC) }}
	ok := c.SpotHourly(ctx, Key{"aws", "eu-central-1", "m5.large", true})
	bad := c.SpotHourly(ctx, Key{"aws", "eu-central-1", "c5.large", true})
	if ok.Hourly != 0.0412 || ok.Reason != "" {
		t.Fatalf("priced = %+v", ok)
	}
	if bad.Hourly != 0 || bad.Reason != "spot price lookup failed" || !strings.Contains(logs.String(), "AuthFailure") {
		t.Fatalf("refused = %+v, logs %q", bad, logs.String())
	}
	if signed != 2 {
		t.Fatalf("%d of 2 requests were signed with the env key", signed)
	}
	for name, s := range map[string]string{"log": logs.String(), "reason": bad.Reason, "label": ok.Label()} {
		if strings.Contains(s, secret) || strings.Contains(s, id) {
			t.Errorf("%s holds the AWS key: %q", name, s)
		}
	}
}
