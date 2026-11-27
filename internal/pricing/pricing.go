// Package pricing looks up public on-demand and spot list prices (USD per instance-hour)
// from the AWS Price List bulk files and the Azure Retail Prices API.
package pricing

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Key identifies a node's price. Windows nodes have no Key: those prices are not built.
type Key struct {
	Provider, Region, Type string
	Spot                   bool
}

func first(l map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := l[k]; v != "" {
			return v
		}
	}
	return ""
}

// KeyOf reads a node's provider ID and labels; false means the node cannot be priced.
func KeyOf(providerID string, labels map[string]string) (Key, bool) {
	var k Key
	switch {
	case strings.HasPrefix(providerID, "aws://"):
		k.Provider = "aws"
	case strings.HasPrefix(providerID, "azure://"):
		k.Provider = "azure"
	case strings.HasPrefix(providerID, "gce://"):
		k.Provider = "gcp"
	default:
		return k, false
	}
	k.Type = first(labels, "node.kubernetes.io/instance-type", "beta.kubernetes.io/instance-type")
	k.Region = first(labels, "topology.kubernetes.io/region", "failure-domain.beta.kubernetes.io/region")
	if os, ok := labels["kubernetes.io/os"]; k.Type == "" || k.Region == "" || (ok && os != "linux") {
		return Key{}, false
	}
	k.Spot = labels["karpenter.sh/capacity-type"] == "spot" || labels["eks.amazonaws.com/capacityType"] == "SPOT" ||
		labels["cloud.google.com/gke-spot"] == "true" || labels["kubernetes.azure.com/scalesetpriority"] == "spot"
	return k, true
}

// Client fetches prices. Zero fields take defaults; it is safe for concurrent use (one lock, so a cold region downloads once).
type Client struct {
	HTTP               *http.Client
	CacheDir           string // empty: no disk cache
	Now                func() time.Time
	AWSBase, AzureBase string
	mu                 sync.Mutex          // guards aws and elb; held across a download so concurrent callers wait for it instead of repeating it
	aws                map[string]awsCache // region -> prices, so one run downloads a region once
	elb                map[string]map[string]float64
	dto                map[string]float64 // region -> USD per GB out to the internet
	Spot               SpotAPI            // nil: spot nodes are priced as on-demand (the CLI has no AWS key)
	spotMu             sync.Mutex
	spot               map[string]spotCache // region/type -> last lookup
}

const cacheTTL = 24 * time.Hour

// The AWS file takes seconds on a good link; the timeout only stops a stalled download from hanging the CLI.
var defaultHTTP = &http.Client{Timeout: 5 * time.Minute}

var (
	regionRe = regexp.MustCompile(`^[a-z0-9-]+$`)
	typeRe   = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
)

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) get(ctx context.Context, u string) (*http.Response, error) {
	h := c.HTTP
	if h == nil {
		h = defaultHTTP
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := h.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return resp, nil
}

// Hourly returns the whole-instance price per hour in USD and where it came from.
func (c *Client) Hourly(ctx context.Context, k Key) (float64, string, error) {
	if !regionRe.MatchString(k.Region) || !typeRe.MatchString(k.Type) {
		return 0, "", fmt.Errorf("refusing odd region %q or type %q", k.Region, k.Type)
	}
	switch k.Provider {
	case "aws":
		d, err := c.awsRegion(ctx, k.Region)
		if err != nil {
			return 0, "", err
		}
		p, ok := d.Prices[k.Type]
		if !ok {
			return 0, "", fmt.Errorf("no on-demand Linux price for %s in %s", k.Type, k.Region)
		}
		if k.Spot && c.Spot != nil {
			s := c.SpotHourly(ctx, k)
			if s.Reason == "" {
				return s.Hourly, "aws " + s.Label(), nil
			}
			return p, "aws spot (priced as on-demand: " + s.Reason + ")", nil
		}
		if k.Spot {
			return p, "aws spot (priced as on-demand: AWS has no public spot price list)", nil
		}
		return p, "aws on-demand", nil
	case "azure":
		return c.azure(ctx, k)
	case "gcp":
		return 0, "", errors.New("gcp prices need a Cloud Billing Catalog API key (not built)")
	}
	return 0, "", fmt.Errorf("unknown provider %q", k.Provider)
}

// awsCache is one region's price list as cached on disk: hourly USD by instance type, and EBS USD per GB-month by volume type.
type awsCache struct {
	FetchedAt time.Time
	Prices    map[string]float64
	EBS       map[string]float64
	Specs     map[string]spec
}

// spec is what the price list says an instance type is made of.
type spec struct {
	VCPU   int
	MemGiB float64
	Arm    bool
}

// Type is a current-generation, non-GPU EC2 instance type with its on-demand Linux price.
type Type struct {
	VCPU   int
	MemGiB float64
	Arm    bool
	Hourly float64
}

// EC2Types returns every instance type of a region that has both a price and a spec. It shares the region's single download.
func (c *Client) EC2Types(ctx context.Context, region string) (map[string]Type, error) {
	if !regionRe.MatchString(region) {
		return nil, fmt.Errorf("refusing odd region %q", region)
	}
	d, err := c.awsRegion(ctx, region)
	if err != nil {
		return nil, err
	}
	out := map[string]Type{}
	for t, sp := range d.Specs {
		if p, ok := d.Prices[t]; ok {
			out[t] = Type{sp.VCPU, sp.MemGiB, sp.Arm, p}
		}
	}
	return out, nil
}

// EBSMonthly returns the on-demand USD price of one GB-month of an EBS volume type (gp3, io2, ...) in a region.
// It shares the region's single download with Hourly.
func (c *Client) EBSMonthly(ctx context.Context, region, volType string) (float64, error) {
	if !regionRe.MatchString(region) || !typeRe.MatchString(volType) {
		return 0, fmt.Errorf("refusing odd region %q or volume type %q", region, volType)
	}
	d, err := c.awsRegion(ctx, region)
	if err != nil {
		return 0, err
	}
	p, ok := d.EBS[volType]
	if !ok {
		return 0, fmt.Errorf("no EBS price for %s in %s", volType, region)
	}
	return p, nil
}

func (c *Client) awsRegion(ctx context.Context, region string) (awsCache, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if d, ok := c.aws[region]; ok {
		return d, nil
	}
	var file string
	if c.CacheDir != "" {
		file = filepath.Join(c.CacheDir, "aws-ec2-"+region+".json")
		if b, err := os.ReadFile(file); err == nil {
			var ac awsCache
			// A cache without EBS rows or specs predates storage pricing or node simulation: treat it as stale so it refetches.
			if json.Unmarshal(b, &ac) == nil && len(ac.Prices) > 0 && len(ac.EBS) > 0 && len(ac.Specs) > 0 && !ac.FetchedAt.After(c.now()) && c.now().Sub(ac.FetchedAt) < cacheTTL {
				return c.remember(region, ac), nil
			}
		}
	}
	base := c.AWSBase
	if base == "" {
		base = "https://pricing.us-east-1.amazonaws.com"
	}
	resp, err := c.get(ctx, base+"/offers/v1.0/aws/AmazonEC2/current/"+region+"/index.csv")
	if err != nil {
		return awsCache{}, err
	}
	defer resp.Body.Close()
	prices, ebs, specs, err := parseAWS(resp.Body)
	if err != nil {
		return awsCache{}, err
	}
	d := awsCache{c.now(), prices, ebs, specs}
	if file != "" { // a cache that cannot be written only costs the next run a download
		if b, err := json.Marshal(d); err == nil && os.MkdirAll(c.CacheDir, 0o755) == nil {
			writeAtomic(file, b)
		}
	}
	return c.remember(region, d), nil
}

// writeAtomic renames a finished temp file into place, so another process never reads a truncated cache. Errors only cost a download.
func writeAtomic(file string, b []byte) {
	f, err := os.CreateTemp(filepath.Dir(file), ".tmp-*")
	if err != nil {
		return
	}
	_, werr := f.Write(b)
	if cerr := f.Close(); werr != nil || cerr != nil || os.Rename(f.Name(), file) != nil {
		os.Remove(f.Name())
	}
}

func (c *Client) remember(region string, d awsCache) awsCache {
	if c.aws == nil {
		c.aws = map[string]awsCache{}
	}
	c.aws[region] = d
	return d
}

var awsCols = []string{"TermType", "Operating System", "Tenancy", "Pre Installed S/W", "CapacityStatus", "Unit", "Currency", "PricePerUnit", "Instance Type", "Product Family", "Volume API Name", "Location Type", "Current Generation", "vCPU", "Memory", "Physical Processor", "GPU"}

// parseAWS streams an EC2 price list CSV and returns on-demand Linux hourly USD by instance type,
// and on-demand EBS USD per GB-month by volume type (gp3, io2, ...), in one pass.
// shortcut: EBS provisioned IOPS and throughput rows (gp3/io1/io2) are not priced, only the per-GB storage rate; add them when volumes with extra IOPS matter.
func parseAWS(r io.Reader) (map[string]float64, map[string]float64, map[string]spec, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.ReuseRecord = true
	var col map[string]int
	out, ebs, specs := map[string]float64{}, map[string]float64{}, map[string]spec{}
	for {
		row, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, nil, err
		}
		if col == nil {
			if len(row) == 0 || row[0] != "SKU" {
				continue // the file opens with metadata rows
			}
			col = map[string]int{}
			for i, name := range row {
				col[name] = i
			}
			for _, name := range awsCols {
				if _, ok := col[name]; !ok {
					return nil, nil, nil, fmt.Errorf("aws price list lacks column %q", name)
				}
			}
			continue
		}
		f := func(name string) string {
			if i := col[name]; i < len(row) {
				return row[i]
			}
			return ""
		}
		if f("Product Family") == "Storage" {
			// "Location Type" excludes Local Zones and Wavelength; io2 says "GB-month", the rest "GB-Mo".
			if v := f("Volume API Name"); f("TermType") == "OnDemand" && f("Location Type") == "AWS Region" && v != "" &&
				(f("Unit") == "GB-Mo" || f("Unit") == "GB-month") && f("Currency") == "USD" {
				// An unreadable or conflicting storage price leaves that volume type unpriced (-1, dropped below),
				// never guessed, and never fails the instance prices that share this file.
				p, err := strconv.ParseFloat(f("PricePerUnit"), 64)
				if old, ok := ebs[v]; err != nil || (ok && old != p) {
					ebs[v] = -1
				} else if p > 0 {
					ebs[v] = p
				}
			}
			continue
		}
		if f("TermType") != "OnDemand" || f("Operating System") != "Linux" || f("Tenancy") != "Shared" || f("Pre Installed S/W") != "NA" ||
			f("CapacityStatus") != "Used" || f("Unit") != "Hrs" || f("Currency") != "USD" {
			continue
		}
		p, err := strconv.ParseFloat(f("PricePerUnit"), 64)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("aws price %q: %w", f("PricePerUnit"), err)
		}
		if p <= 0 {
			continue
		}
		t := f("Instance Type")
		if old, ok := out[t]; ok && old != p {
			return nil, nil, nil, fmt.Errorf("aws price list has two prices for %s: %v and %v", t, old, p)
		}
		out[t] = p
		if sp, ok := parseSpec(f); ok {
			specs[t] = sp
		}
	}
	if col == nil {
		return nil, nil, nil, errors.New("aws price list has no SKU header row")
	}
	for v, p := range ebs {
		if p < 0 {
			delete(ebs, v)
		}
	}
	return out, ebs, specs, nil
}

// parseSpec reads vCPU and memory ("8 GiB"; "NA" for some types) of a kept row; GPU and previous-generation types are not candidates.
func parseSpec(f func(string) string) (spec, bool) {
	if f("Current Generation") != "Yes" || f("GPU") != "" {
		return spec{}, false
	}
	v, err := strconv.Atoi(f("vCPU"))
	m, ok := strings.CutSuffix(f("Memory"), " GiB")
	g, err2 := strconv.ParseFloat(strings.ReplaceAll(m, ",", ""), 64)
	if err != nil || !ok || err2 != nil || v <= 0 || g <= 0 {
		return spec{}, false
	}
	return spec{v, g, strings.Contains(f("Physical Processor"), "Graviton")}, true
}

type azurePage struct {
	Items []struct {
		RetailPrice   float64
		UnitOfMeasure string
		CurrencyCode  string
		Type          string
		ProductName   string
		SkuName       string
	}
	NextPageLink string
}

func (c *Client) azure(ctx context.Context, k Key) (float64, string, error) {
	base := c.AzureBase
	if base == "" {
		base = "https://prices.azure.com"
	}
	q := url.Values{"$filter": {fmt.Sprintf("serviceName eq 'Virtual Machines' and armRegionName eq '%s' and armSkuName eq '%s' and priceType eq 'Consumption'", k.Region, k.Type)}}
	next := base + "/api/retail/prices?" + q.Encode()
	for page := 0; page < 10 && next != ""; page++ {
		resp, err := c.get(ctx, next)
		if err != nil {
			return 0, "", err
		}
		var pg azurePage
		err = json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&pg)
		resp.Body.Close()
		if err != nil {
			return 0, "", err
		}
		for _, it := range pg.Items {
			if it.Type == "Consumption" && it.UnitOfMeasure == "1 Hour" && it.CurrencyCode == "USD" && !strings.Contains(it.ProductName, "Windows") &&
				!strings.HasSuffix(it.SkuName, " Low Priority") && strings.HasSuffix(it.SkuName, " Spot") == k.Spot {
				if k.Spot {
					return it.RetailPrice, "azure spot", nil
				}
				return it.RetailPrice, "azure on-demand", nil
			}
		}
		next = pg.NextPageLink
	}
	return 0, "", fmt.Errorf("no azure price for %s in %s", k.Type, k.Region)
}

// ELBHourly returns the on-demand USD per load-balancer-hour in a region, by kind: "classic" and "nlb".
// A kind with no usable row is absent. Source: the AWSELB price list, whose Unit "Hrs" rows are the hourly charge.
// shortcut: only the hourly charge; LCU, data-processed and NLB capacity-unit charges are usage-based, invisible to a read-only agent.
// The file is ~6 KB, so it is kept in memory for the run and has no disk cache.
func (c *Client) ELBHourly(ctx context.Context, region string) (map[string]float64, error) {
	if !regionRe.MatchString(region) {
		return nil, fmt.Errorf("refusing odd region %q", region)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if m, ok := c.elb[region]; ok {
		return m, nil
	}
	base := c.AWSBase
	if base == "" {
		base = "https://pricing.us-east-1.amazonaws.com"
	}
	resp, err := c.get(ctx, base+"/offers/v1.0/aws/AWSELB/current/"+region+"/index.csv")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	m, err := parseELB(resp.Body)
	if err != nil {
		return nil, err
	}
	if c.elb == nil {
		c.elb = map[string]map[string]float64{}
	}
	c.elb[region] = m
	return m, nil
}

var elbCols = []string{"TermType", "Unit", "PricePerUnit", "Currency", "Location Type", "operation"}

// parseELB reads an AWSELB price list CSV. Matching on "operation" (not usageType, which has a region prefix except in
// us-east-1): "LoadBalancing" is Classic, "LoadBalancing:Network" is NLB. A bad or conflicting row leaves that kind unpriced.
func parseELB(r io.Reader) (map[string]float64, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.ReuseRecord = true
	var col map[string]int
	out := map[string]float64{}
	for {
		row, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if col == nil {
			if len(row) == 0 || row[0] != "SKU" {
				continue
			}
			col = map[string]int{}
			for i, name := range row {
				col[name] = i
			}
			for _, name := range elbCols {
				if _, ok := col[name]; !ok {
					return nil, fmt.Errorf("aws elb price list lacks column %q", name)
				}
			}
			continue
		}
		f := func(name string) string {
			if i := col[name]; i < len(row) {
				return row[i]
			}
			return ""
		}
		kind := map[string]string{"LoadBalancing": "classic", "LoadBalancing:Network": "nlb"}[f("operation")]
		if kind == "" || f("TermType") != "OnDemand" || f("Location Type") != "AWS Region" || f("Unit") != "Hrs" || f("Currency") != "USD" {
			continue
		}
		p, err := strconv.ParseFloat(f("PricePerUnit"), 64)
		if old, ok := out[kind]; err != nil || math.IsInf(p, 0) || math.IsNaN(p) || (ok && old != p) {
			out[kind] = -1
		} else if p > 0 {
			out[kind] = p
		}
	}
	if col == nil {
		return nil, errors.New("aws elb price list has no SKU header row")
	}
	for k, p := range out {
		if p < 0 {
			delete(out, k)
		}
	}
	return out, nil
}

// DataTransferOut returns the USD per GB of the first tier (first 10 TB a month) of data transfer out of a region to the
// internet, from the AWSDataTransfer price list. Kept in memory for the run; the file is ~0.5 MB, so there is no disk cache.
// shortcut: first tier only, and the account-wide 100 GB free allowance is ignored; both make the estimate higher, never lower.
func (c *Client) DataTransferOut(ctx context.Context, region string) (float64, error) {
	if !regionRe.MatchString(region) {
		return 0, fmt.Errorf("refusing odd region %q", region)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if p, ok := c.dto[region]; ok {
		return p, nil
	}
	base := c.AWSBase
	if base == "" {
		base = "https://pricing.us-east-1.amazonaws.com"
	}
	resp, err := c.get(ctx, base+"/offers/v1.0/aws/AWSDataTransfer/current/"+region+"/index.csv")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	p, err := parseDataTransferOut(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("%w in %s", err, region)
	}
	if c.dto == nil {
		c.dto = map[string]float64{}
	}
	c.dto[region] = p
	return p, nil
}

var dtoCols = []string{"TermType", "Unit", "PricePerUnit", "Currency", "StartingRange", "Transfer Type", "To Location", "usageType"}

// parseDataTransferOut reads an AWSDataTransfer price list CSV and returns the on-demand USD per GB of the row that starts at
// 0 GB, goes "AWS Outbound" to "External" (the internet) and belongs to the region (the "Global-" row is the free allowance).
// No such row, or two rows that disagree, is an error: the price is never guessed.
func parseDataTransferOut(r io.Reader) (float64, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.ReuseRecord = true
	var col map[string]int
	price, found := 0.0, false
	for {
		row, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, err
		}
		if col == nil {
			if len(row) == 0 || row[0] != "SKU" {
				continue
			}
			col = map[string]int{}
			for i, name := range row {
				col[name] = i
			}
			for _, name := range dtoCols {
				if _, ok := col[name]; !ok {
					return 0, fmt.Errorf("aws data transfer price list lacks column %q", name)
				}
			}
			continue
		}
		f := func(name string) string {
			if i := col[name]; i < len(row) {
				return row[i]
			}
			return ""
		}
		if f("TermType") != "OnDemand" || f("Transfer Type") != "AWS Outbound" || f("To Location") != "External" || f("StartingRange") != "0" ||
			f("Unit") != "GB" || f("Currency") != "USD" || strings.HasPrefix(f("usageType"), "Global-") || !strings.HasSuffix(f("usageType"), "DataTransfer-Out-Bytes") {
			continue
		}
		p, err := strconv.ParseFloat(f("PricePerUnit"), 64)
		if err != nil || math.IsInf(p, 0) || math.IsNaN(p) || p <= 0 || (found && p != price) {
			return 0, errors.New("unusable data transfer price")
		}
		price, found = p, true
	}
	if col == nil {
		return 0, errors.New("aws data transfer price list has no SKU header row")
	}
	if !found {
		return 0, errors.New("no data transfer out price")
	}
	return price, nil
}
