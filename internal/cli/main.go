// Package cli is the phrontis command (cmd/phrontis). It prints what a Kubernetes cluster costs per namespace and workload,
// how much of it sits idle, and which container requests to change.
// It is read-only: it only lists nodes, pods, replicasets, statefulsets, daemonsets, jobs, limit ranges, persistent volumes,
// storage classes, services and pod metrics.
// With --api it runs as the in-cluster agent instead and sends a sample to the API every --every (agent.go).
// Other builds may add subcommands through Commands and FromDB (below).
package cli

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8slabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/pager"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metrics "k8s.io/metrics/pkg/client/clientset/versioned"

	"github.com/cursopic/phrontis/internal/nodepolicy"
	"github.com/cursopic/phrontis/internal/perms"
	"github.com/cursopic/phrontis/internal/pricing"
	"github.com/cursopic/phrontis/internal/report"
)

const sampleEvery = 30 * time.Second

// clock seams so tests can run a --window without waiting.
var (
	nowFn   = time.Now
	sleepFn = time.Sleep
)

// DefaultPrices are the flat rates the CLI defaults to and the API always uses.
var DefaultPrices = report.Prices{CPUHour: 0.03, GiBHour: 0.004}

func Main() {
	if len(os.Args) > 1 && os.Args[1] == "mcp" {
		if err := serveMCP(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "phrontis:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "permissions" {
		switch {
		case len(os.Args) == 2:
			perms.Text(os.Stdout)
		case os.Args[2] == "--md":
			perms.Markdown(os.Stdout)
		case os.Args[2] == "--json":
			os.Stdout.Write(perms.JSON())
		default:
			fmt.Fprintln(os.Stderr, "usage: phrontis permissions [--md|--json]")
			os.Exit(2)
		}
		return
	}
	for _, c := range Commands {
		if c.Match(os.Args[1:]) {
			if err := c.Run(os.Args[1:]); err != nil {
				fmt.Fprintln(os.Stderr, c.Prefix, err)
				os.Exit(1)
			}
			return
		}
	}
	if len(os.Args) > 1 && os.Args[1] == "predict" {
		if err := predict(os.Args[2:], os.Stdin, os.Stdout, os.Stderr); err != nil {
			fmt.Fprintln(os.Stderr, "phrontis:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 4 && os.Args[1] == "compare" {
		if err := compare(os.Stdout, os.Args[2], os.Args[3]); err != nil {
			fmt.Fprintln(os.Stderr, "phrontis:", err)
			os.Exit(1)
		}
		return
	}
	var ctxs []string
	flag.Func("context", "kubeconfig context (default: current); several, comma-separated or repeated, print one report each (tables and --markdown), one after the other", func(v string) error {
		if v == "" { // a script's unset variable: the current context
			return nil
		}
		cs, err := parseList("context", v)
		for _, c := range cs {
			if !slices.Contains(ctxs, c) {
				ctxs = append(ctxs, c)
			}
		}
		return err
	})
	cpuHour := flag.Float64("cpu-hour", DefaultPrices.CPUHour, "price of one vCPU for one hour")
	gibHour := flag.Float64("gib-hour", DefaultPrices.GiBHour, "price of one GiB of memory for one hour")
	window := flag.Duration("window", 0, "sample usage every 30s for this long (0 = one sample)")
	asJSON := flag.Bool("json", false, "print the report as JSON (what web/ reads) instead of tables")
	flag.BoolVar(&asCSV, "csv", false, "print the recommendations as CSV (one row per container) instead of tables")
	flag.BoolVar(&asMD, "markdown", false, "print the recommendations as a Markdown table, with the columns of --csv, instead of tables")
	flag.IntVar(&topN, "top", 0, "list only the n recommendations with the largest saving (tables, --csv and --markdown; 0 = all)")
	flag.BoolVar(&shareIdle, "share-idle", false, "add to each namespace and workload its part of the idle cost, in proportion to its cost on each node (tables and --json)")
	flag.Func("namespace", "show only these namespaces, comma-separated or repeated (tables, --csv and --markdown)", func(v string) error {
		ns, err := parseList("namespace", v)
		onlyNS = append(onlyNS, ns...)
		return err
	})
	flag.Func("exclude-namespace", "leave these namespaces out, comma-separated or repeated; not with --namespace (tables, --csv and --markdown)", func(v string) error {
		ns, err := parseList("namespace", v)
		skipNS = append(skipNS, ns...)
		return err
	})
	cloud := flag.Bool("cloud-prices", true, "price nodes from AWS/Azure public list prices (USD); false = flat prices only")
	api := flag.String("api", "", "agent mode: instead of a report, POST a sample every --every to this Phrontis API (key in $PHRONTIS_API_KEY)")
	sec := flag.Bool("security", false, "print configuration checks (privileged, host access, root, :latest, no limits) instead of the cost report")
	every := flag.Duration("every", time.Minute, "agent mode: time between samples")
	promURL := flag.String("prometheus", "", "read p95 CPU and peak memory over --history from this Prometheus (or Thanos, VictoriaMetrics, Mimir) instead of sampling")
	promTokenFile := flag.String("prometheus-token-file", "", "file with the bearer token for --prometheus (read from the file only, never printed)")
	promSelector := flag.String("prometheus-selector", "", `label matcher added as is to both --prometheus queries, e.g. 'cluster="prod"'`)
	history := flag.String("history", "7d", "span of --prometheus history, 1h to 30d (d = days)")
	var db *string
	if FromDB != nil { // --db exists only in the private build
		db = flag.String("db", "", "report from the samples the agent stored instead of the live cluster: app-role Postgres URL (cluster from $PHRONTIS_API_KEY)")
	}
	flag.Parse()
	err := checkFlags(*asJSON, *sec || *api != "")
	if err == nil && len(ctxs) > 1 && (*asJSON || asCSV || *sec || *api != "" || db != nil && *db != "") {
		err = errors.New("several contexts work only with the tables and --markdown")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "phrontis:", err)
		os.Exit(2)
	}
	if err == nil {
		err = setupProm(*promURL, *promTokenFile, *promSelector, *history, flagSet("history"), *window,
			*api != "" || *sec || db != nil && *db != "" || len(ctxs) > 1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "phrontis:", err)
		os.Exit(2)
	}
	kctx := strings.Join(ctxs, "") // the one context, or "" for the current one
	prices := report.Prices{CPUHour: *cpuHour, GiBHour: *gibHour}
	switch {
	case *sec:
		err = securityRun(os.Stdout, kctx)
	case *api != "":
		err = agent(kctx, *api, *every)
	case db != nil && *db != "":
		err = FromDB(os.Stdout, *db, prices, *window, *asJSON, *cloud, time.Now().UTC())
	default:
		pr := newPricer(*cloud)
		one := func(out io.Writer, kctx string) error { return run(out, kctx, prices, *window, *asJSON, pr) }
		if len(ctxs) > 1 {
			err = runEach(os.Stdout, os.Stderr, ctxs, one)
		} else {
			err = one(os.Stdout, kctx)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "phrontis:", err)
		os.Exit(1)
	}
}

// RestConfig reads the kubeconfig, or the pod's service account when there is none (in-cluster agent), and
// returns the context it resolved to: empty in-cluster.
func RestConfig(kctx string) (*rest.Config, string, error) {
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{CurrentContext: kctx})
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, "", err
	}
	if kctx == "" {
		if raw, err := cc.RawConfig(); err == nil {
			kctx = raw.CurrentContext
		}
	}
	cfg.Timeout = 30 * time.Second // a dead cluster fails fast instead of hanging
	return cfg, kctx, nil
}

func clients(kctx string) (kubernetes.Interface, metrics.Interface, string, error) {
	cfg, kctx, err := RestConfig(kctx)
	if err != nil {
		return nil, nil, "", err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, "", err
	}
	mc, err := metrics.NewForConfig(cfg)
	if err != nil {
		return nil, nil, "", err
	}
	return cs, mc, kctx, nil
}

// runEach writes one report per context under its name. A context that fails does not stop the others; its error is on warn and the returned one counts them.
func runEach(out, warn io.Writer, ctxs []string, report func(out io.Writer, kctx string) error) error {
	failed := 0
	for _, c := range ctxs {
		var b bytes.Buffer // a failed context leaves no heading or half a report
		if err := report(&b, c); err != nil {
			fmt.Fprintf(warn, "phrontis: context %s: %v\n", c, err)
			failed++
			continue
		}
		if _, err := fmt.Fprintf(out, "## %s\n\n%s\n", c, b.Bytes()); err != nil {
			return err
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d contexts failed", failed, len(ctxs))
	}
	return nil
}

func run(out io.Writer, kctx string, prices report.Prices, window time.Duration, asJSON bool, pr pricer) error {
	cs, mc, kctx, err := clients(kctx)
	if err != nil {
		return err
	}
	nodes, pods, keys, vols, lbs, pdbs, samples, err := collect(context.Background(), cs, mc, window)
	if err != nil {
		return err
	}
	s := snapshot{kctx, nodes, pods, keys, vols, lbs, samples, window, false, pdbs, nil, nil}
	if prom != nil {
		if s.samples, err = prom.applyHistory(context.Background(), s.pods); err != nil {
			return err
		}
		s.window, s.windowed = prom.history, true
	}
	s.hpas = listHPAs(context.Background(), cs, os.Stderr)
	s.limitRanges = listLimitRanges(context.Background(), cs, os.Stderr)
	return output(context.Background(), out, s, prices, pr, asJSON, time.Now().UTC())
}

// Command is a subcommand another build adds; Prefix starts its error line.
type Command struct {
	Match  func(args []string) bool
	Run    func(args []string) error
	Prefix string
}

// Commands and FromDB are empty in the public build; another build can fill them in. FromDB non-nil adds the --db flag.
var (
	Commands []Command
	FromDB   func(out io.Writer, dbURL string, prices report.Prices, window time.Duration, asJSON, cloud bool, now time.Time) error
)

// Pricer prices nodes; NewPricer is its one shared client (nil without cloud).
type Pricer = pricer

func NewPricer(cloud bool) Pricer { return newPricer(cloud) }

// Sample is the POST /v1/samples body.
type Sample = sample

// ReportStored writes the report of usage the agent stored: CPU sized on its p95, memory on its peak over window.
func ReportStored(ctx context.Context, out io.Writer, label string, nodes []report.Node, pods []report.Pod, keys map[string]pricing.Key,
	vols []report.Volume, lbs []report.LoadBalancer, samples int, window time.Duration, pdbs []report.PDB, hpas []report.HPA,
	prices report.Prices, pr Pricer, asJSON bool, now time.Time) error {
	return output(ctx, out, snapshot{label, nodes, pods, keys, vols, lbs, samples, window, true, pdbs, hpas, nil}, prices, pr, asJSON, now)
}

// Cost is the report's cost for one stored sample: nodes priced as the report prices them (priceNodes), then
// report.Build, and where each node's price came from. The daily alert check (P62) uses it so its numbers match the report.
func Cost(ctx context.Context, pr Pricer, nodes []report.Node, pods []report.Pod, keys map[string]pricing.Key) (report.Report, []NodePrice) {
	np := priceNodes(ctx, pr, nodes, keys, DefaultPrices, io.Discard)
	return report.Build(nodes, pods, DefaultPrices), np
}

// snapshot is what a report is built from; window is the span the usage covers; windowed means usage is p95 CPU / peak memory over stored samples, not a live peak.
type snapshot struct {
	kctx        string
	nodes       []report.Node
	pods        []report.Pod
	keys        map[string]pricing.Key
	volumes     []report.Volume
	lbs         []report.LoadBalancer
	samples     int
	window      time.Duration
	windowed    bool
	pdbs        []report.PDB
	hpas        []report.HPA                       // stored reports: the latest sample's (#229)
	limitRanges map[string][]corev1.LimitRangeItem // by namespace; live reports only
}

// newPricer is the one client a process shares (nil without --cloud-prices): its in-memory region cache lives as long as it does.
func newPricer(cloud bool) pricer {
	if !cloud {
		return nil
	}
	c := &pricing.Client{}
	if d, err := os.UserCacheDir(); err == nil {
		c.CacheDir = filepath.Join(d, "phrontis")
	}
	return c
}

// asCSV, asMD, topN and shareIdle are --csv, --markdown, --top and --share-idle; package variables so the private build's --db path (FromDB) honours them without a new parameter.
var (
	asCSV, asMD, shareIdle bool
	topN                   int
)

// checkFlags rejects flags that cannot be used together; noRecs means --security or --api, which print no recommendations.
func checkFlags(asJSON, noRecs bool) error {
	switch {
	case asJSON && asCSV || asJSON && asMD || asCSV && asMD:
		return errors.New("use only one of --json, --csv and --markdown")
	case topN < 0:
		return errors.New("--top must not be negative")
	case shareIdle && (asCSV || asMD || noRecs):
		return errors.New("--share-idle works only with the tables and --json")
	case len(onlyNS) > 0 && len(skipNS) > 0:
		return errors.New("use only one of --namespace and --exclude-namespace")
	case (asJSON || noRecs) && (len(onlyNS) > 0 || len(skipNS) > 0 || topN > 0), noRecs && asMD:
		return errors.New("--namespace, --exclude-namespace, --top and --markdown do not work with --json, --security or --api")
	}
	return nil
}

// top keeps the first topN recommendations: report.Build sorts them by saving, largest first.
func top(recs []report.Rec) []report.Rec {
	if topN > 0 && topN < len(recs) {
		return recs[:topN]
	}
	return recs
}

// onlyNS is --namespace and skipNS --exclude-namespace; both empty show every namespace. The figures are computed on the whole cluster first, so a namespace costs the same with or without them.
var onlyNS, skipNS []string

func parseList(what, v string) ([]string, error) {
	var l []string
	for _, n := range strings.Split(v, ",") {
		if n = strings.TrimSpace(n); n == "" {
			return nil, fmt.Errorf("empty %s name", what)
		}
		l = append(l, n)
	}
	return l, nil
}

// filterNamespaces keeps the rows of the namespaces in only, or drops those in skip. One without running pods is an error, and so is dropping them all: neither must read as "nothing to save".
func filterNamespaces(r *report.Report, only, skip []string) error {
	if len(only)+len(skip) == 0 {
		return nil
	}
	for _, n := range slices.Concat(only, skip) {
		if !slices.ContainsFunc(r.Namespaces, func(l report.Line) bool { return l.Namespace == n }) {
			return fmt.Errorf("namespace %q has no pods in this report", n)
		}
	}
	out := func(ns string) bool { return len(only) > 0 && !slices.Contains(only, ns) || slices.Contains(skip, ns) }
	other := func(l report.Line) bool { return out(l.Namespace) }
	r.Namespaces = slices.DeleteFunc(r.Namespaces, other)
	if len(r.Namespaces) == 0 {
		return errors.New("--exclude-namespace leaves out every namespace")
	}
	r.Workloads = slices.DeleteFunc(r.Workloads, other)
	r.Recs = slices.DeleteFunc(r.Recs, func(x report.Rec) bool { return out(x.Namespace) })
	r.Risks = slices.DeleteFunc(r.Risks, func(x report.Risk) bool { return out(x.Namespace) })
	return nil
}

func output(ctx context.Context, out io.Writer, s snapshot, prices report.Prices, pr pricer, asJSON bool, now time.Time) error {
	e := assemble(ctx, s, prices, pr, os.Stderr, now)
	if shareIdle {
		e.ShareIdle()
	}
	if asJSON {
		return writeJSON(out, e)
	}
	if err := filterNamespaces(&e.Report, onlyNS, skipNS); err != nil {
		return err
	}
	if asCSV || asMD {
		recs, body := top(e.Recs), report.CSV
		if asMD {
			body = report.Markdown
		}
		if n := len(e.Recs) - len(recs); n > 0 { // on stderr, so stdout stays only the rows
			fmt.Fprintf(os.Stderr, "phrontis: %d more recommendation(s) with a smaller saving not shown (--top %d)\n", n, topN)
		}
		_, err := out.Write(body(recs))
		return err
	}
	render(out, e.Report, prices, e.NodePrices, s.samples, s.window, s.windowed)
	if len(e.PricesAdjusted) > 0 {
		fmt.Fprintf(out, "prices   adjusted (%s): the figures above are estimates, not an invoice\n", strings.Join(e.PricesAdjusted, ", "))
	}
	renderRisks(out, e.Risks)
	if names, what := onlyNS, "only"; len(onlyNS)+len(skipNS) > 0 {
		if len(skipNS) > 0 {
			names, what = skipNS, "without"
		}
		fmt.Fprintf(out, "\n%s namespaces %s; the cluster line covers the whole cluster, and the storage, load balancer, spot, node, eviction and abandoned-workload checks are left out\n",
			what, strings.Join(names, ", "))
		return nil
	}
	renderStorage(out, e.Storage)
	renderLoadBalancers(out, e.LoadBalancers)
	renderNetwork(out, e.Network, s.window.String())
	renderSpot(out, e.Spot)
	renderKarpenter(out, e.NodePool)
	renderConsolidation(out, e.Consolidation)
	renderUnevictable(out, e.Unevictable)
	renderAbandoned(out, e.Abandoned)
	return nil
}

// assemble prices a snapshot (pr nil: flat prices only) into the report that both the tables and --json show.
func assemble(ctx context.Context, s snapshot, prices report.Prices, pr pricer, warn io.Writer, at time.Time) export {
	var seen bytes.Buffer // the same lines go into the JSON, so a flat-price fallback is visible to API readers
	warn = io.MultiWriter(warn, &seen)
	np := priceNodes(ctx, pr, s.nodes, s.keys, prices, warn)
	r := report.Build(s.nodes, s.pods, prices)
	r.Risks = report.Risks(s.pods, s.hpas)
	report.MarkHPA(r.Recs, s.hpas)
	report.MarkLimitRanges(r.Recs, s.limitRanges)
	report.ExplainAll(r.Recs, s.windowed, s.window)
	listed, unlisted := map[string]bool{}, 0
	for _, n := range s.nodes {
		listed[n.Name] = true
	}
	for _, p := range s.pods {
		if !listed[p.Node] {
			unlisted++
		}
	}
	if unlisted > 0 { // spec 001: they are charged flat, so namespaces + idle exceed the cluster total by their cost
		fmt.Fprintf(warn, "phrontis: %d pod(s) on nodes missing from the node list are costed at flat prices, on top of the cluster total\n", unlisted)
	}
	st := report.BuildStorage(s.volumes, ebsPrices(ctx, pr, s.volumes, s.keys, warn))
	lb := report.BuildLoadBalancers(s.lbs, lbPrices(ctx, pr, s.lbs, s.keys, warn))
	pool := report.BuildNodePool(s.pods, r.Recs, nodeOffers(ctx, pr, s.keys, warn), r.Monthly)
	var kp *karpenter
	if pool.Type != "" {
		if y, err := nodePoolYAML(pool); err != nil {
			fmt.Fprintf(warn, "phrontis: no NodePool YAML: %v; skipping the Karpenter section\n", err)
		} else {
			kp = &karpenter{pool, y}
		}
	}
	var warnings []string
	if seen.Len() > 0 {
		warnings = strings.Split(strings.TrimSpace(seen.String()), "\n")
	}
	e := export{r, s.kctx, prices, np, s.samples, s.window.String(), basis(s.windowed), at, st, lb, kp, warnings, buildSpot(ctx, pr, s, r), report.BuildSecurity(s.pods), nil, nil, nil, nil, nil}
	if prom != nil { // spec 040: only a live --prometheus report; prom is nil in every other mode
		e.Network = buildNetwork(ctx, *prom, pr, s.keys, s.pods, warn)
	}
	e.Consolidation = report.BuildConsolidation(flatPriced(s.nodes, prices), s.pods, s.pdbs)
	e.Unevictable = report.BuildUnevictable(flatPriced(s.nodes, prices), s.pods, s.pdbs)
	e.Abandoned = report.BuildAbandoned(s.pods, r.Workloads) // nil unless pods carry a Window's CPU stats
	if a, ok := pr.(*adjusted); ok {
		e.PricesAdjusted = a.labels()
	}
	return e
}

// export is the --json document; its field names are the contract web/src/report.ts reads.
type export struct {
	report.Report
	Context        string
	Prices         report.Prices // the flat rates; nodes listed in NodePrices with another Source were priced individually
	NodePrices     []NodePrice
	Samples        int
	Window         string
	Basis          string // what usage means: "peak" or "p95 CPU, peak memory"
	At             time.Time
	Storage        report.Storage        // PersistentVolumes by namespace, billed per GiB; separate from the compute figures above
	LoadBalancers  report.LoadBalancers  // hourly charge only; separate from the compute figures above
	NodePool       *karpenter            `json:",omitempty"` // simulated Karpenter NodePool; absent without a single AWS region's prices
	Warnings       []string              `json:",omitempty"` // why prices fell back to "flat" (lookup failed or timed out); absent when none did
	Spot           *spotReport           `json:",omitempty"` // which workloads can run on spot and what it saves; absent when there are no workloads
	Security       report.SecurityReport // configuration checks, not a vulnerability scan (spec 023)
	Consolidation  *report.Consolidation `json:",omitempty"` // S29: nodes that could be emptied; absent with fewer than 2 nodes
	Abandoned      *report.Abandoned     `json:",omitempty"` // S27: idle workloads; absent without a usage window or when none is idle
	PricesAdjusted []string              `json:",omitempty"` // S23: the org's discounts that changed node prices, e.g. "aws -10%"; absent when none applied
	Unevictable    *report.Unevictable   `json:",omitempty"` // S26: pods that block consolidation; absent when none do
	Network        []networkEgress       `json:",omitempty"` // spec 040: outbound traffic per namespace, an upper bound, in no total; absent without --prometheus or on a non-AWS cluster
}

// karpenter is the simulation plus the NodePool manifest built from it (built once, shown by both the CLI and the dashboard).
type karpenter struct {
	report.NodePool
	YAML string
}

// NodePrice is what one node costs per hour in total and where that figure came from ("flat" for the fallback).
type NodePrice struct {
	Node, Source string
	Hourly       float64
}

type pricer interface {
	Hourly(ctx context.Context, k pricing.Key) (float64, string, error)
	EBSMonthly(ctx context.Context, region, volType string) (float64, error)
	ELBHourly(ctx context.Context, region string) (map[string]float64, error)
	DataTransferOut(ctx context.Context, region string) (float64, error)
	EC2Types(ctx context.Context, region string) (map[string]pricing.Type, error)
}

// karpenterCategories is Karpenter's documented c/m/r instance categories: drops burstable (t), GPU, storage and metal-only families.
var karpenterCategories = regexp.MustCompile(`^[cmr][0-9]`)

// nodeOffers lists the instance types a Karpenter pool could use, priced at on-demand list price. Like lbPrices, it needs
// exactly one AWS region among the priced nodes; otherwise (or on a lookup error) there are no offers. pr may be nil.
// shortcut: Graviton left out — images must be multi-arch; add when the agent reports node arch.
func nodeOffers(ctx context.Context, pr pricer, keys map[string]pricing.Key, warn io.Writer) []report.Offer {
	regions := map[string]bool{}
	for _, k := range keys {
		if k.Provider == "aws" {
			regions[k.Region] = true
		}
	}
	if pr == nil || len(regions) != 1 {
		return nil
	}
	region := slices.Collect(maps.Keys(regions))[0]
	types, err := pr.EC2Types(ctx, region)
	if err != nil {
		fmt.Fprintf(warn, "phrontis: no instance types in %s: %v; skipping the Karpenter section\n", region, err)
		return nil
	}
	var out []report.Offer
	for _, name := range slices.Sorted(maps.Keys(types)) {
		t, fam := types[name], strings.SplitN(name, ".", 2)[0]
		if t.Arm || !karpenterCategories.MatchString(fam) || strings.Contains(name, ".metal") { // also m7i.metal-24xl
			continue
		}
		out = append(out, report.Offer{Type: name, Family: fam, Hourly: t.Hourly,
			Capacity: report.Resources{MilliCPU: int64(t.VCPU) * 1000, MemBytes: int64(t.MemGiB * (1 << 30))}})
	}
	return out
}

// nodePoolYAML is the NodePool for a simulation: the cheapest families, amd64, spot allowed only if some pods are spot-safe.
// Phrontis does not know the cluster's EC2NodeClass, so the user points nodeClassRef at theirs.
func nodePoolYAML(n report.NodePool) (string, error) {
	y, err := nodepolicy.NodePool(nodepolicy.Policy{Name: "phrontis", NodeClass: "default", Families: n.Families,
		Arch: []string{"amd64"}, Spot: n.SpotShare > 0})
	return "# Set nodeClassRef.name to your EC2NodeClass before you apply.\n" + string(y), err
}

// renderKarpenter prints the KARPENTER section; nothing when there is no simulation.
func renderKarpenter(out io.Writer, k *karpenter) {
	if k == nil {
		return
	}
	fmt.Fprintf(out, "\nKARPENTER (simulated from the requests above with the rightsizing applied; nothing is applied to your cluster)\n")
	fmt.Fprintf(out, "%.2f/month now → %.2f/month simulated: %d × %s, %.0f%% of it on spot-safe nodes\n", k.Current, k.Monthly, k.Nodes, k.Type, 100*k.SpotShare)
	fmt.Fprintln(out, "on-demand list prices; spot savings are in the SPOT section")
	if k.Unplaced > 0 {
		fmt.Fprintf(out, "%d pod(s) fit no instance type and are left out of the simulation\n", k.Unplaced)
	}
	if len(k.OnDemand) > 0 {
		fmt.Fprintf(out, "keep on-demand (single replica or not a Deployment; nodeSelector karpenter.sh/capacity-type: on-demand): %s\n", strings.Join(k.OnDemand, ", "))
	}
	fmt.Fprintf(out, "\n%s", k.YAML)
}

// ebsPrices looks up USD per GiB-month for each EBS type in vols. The region is the one AWS region of the priced nodes;
// with none or several, EBS volumes stay unpriced (a PV does not say which region it lives in). pr may be nil.
// AWS quotes per "GB" but bills the provisioned GiB, so the per-GB rate is applied to GiB.
func ebsPrices(ctx context.Context, pr pricer, vols []report.Volume, keys map[string]pricing.Key, warn io.Writer) map[string]float64 {
	regions := map[string]bool{}
	for _, k := range keys {
		if k.Provider == "aws" {
			regions[k.Region] = true
		}
	}
	out := map[string]float64{}
	if pr == nil || len(regions) != 1 {
		return out
	}
	region := slices.Collect(maps.Keys(regions))[0]
	for _, v := range vols {
		if _, done := out[v.Type]; v.Type == "" || done {
			continue
		}
		p, err := pr.EBSMonthly(ctx, region, v.Type)
		if err != nil {
			fmt.Fprintf(warn, "phrontis: no EBS price for %s in %s: %v; leaving those volumes unpriced\n", v.Type, region, err)
			p = -1
		}
		out[v.Type] = p
	}
	for t, p := range out {
		if p < 0 {
			delete(out, t) // after the loop, so each failing type warns once
		}
	}
	return out
}

// lbPrices looks up USD per load-balancer-hour by kind. Like ebsPrices, it needs exactly one AWS region among the priced
// nodes; otherwise (or on a lookup error) every load balancer stays unpriced. pr may be nil.
func lbPrices(ctx context.Context, pr pricer, lbs []report.LoadBalancer, keys map[string]pricing.Key, warn io.Writer) map[string]float64 {
	regions := map[string]bool{}
	for _, k := range keys {
		if k.Provider == "aws" {
			regions[k.Region] = true
		}
	}
	if pr == nil || len(lbs) == 0 || len(regions) != 1 {
		return nil
	}
	region := slices.Collect(maps.Keys(regions))[0]
	m, err := pr.ELBHourly(ctx, region)
	if err != nil {
		fmt.Fprintf(warn, "phrontis: no load balancer price in %s: %v; leaving load balancers unpriced\n", region, err)
		return nil
	}
	return m
}

// priceNodes sets nodes[i].Prices from pr for every node that has a key, looking each distinct key up once.
// A failed lookup warns once and leaves those nodes on the flat prices. pr may be nil (--cloud-prices=false).
func priceNodes(ctx context.Context, pr pricer, nodes []report.Node, keys map[string]pricing.Key, flat report.Prices, warn io.Writer) []NodePrice {
	type res struct {
		hourly float64
		source string
		err    error
	}
	seen := map[pricing.Key]res{}
	out := make([]NodePrice, len(nodes))
	for i := range nodes {
		n := &nodes[i]
		out[i] = NodePrice{n.Name, "flat", flat.Monthly(n.Capacity) / report.HoursPerMonth}
		k, ok := keys[n.Name]
		if !ok || pr == nil {
			continue
		}
		r, done := seen[k]
		if !done {
			r.hourly, r.source, r.err = pr.Hourly(ctx, k)
			if r.err == nil && report.Split(r.hourly, n.Capacity, flat) == (report.Prices{}) {
				r.err = fmt.Errorf("unusable price %v", r.hourly)
			}
			if r.err != nil {
				fmt.Fprintf(warn, "phrontis: no cloud price for %s %s in %s: %v; using flat prices\n", k.Provider, k.Type, k.Region, r.err)
			}
			seen[k] = r
		}
		if r.err == nil {
			n.Prices = report.Split(r.hourly, n.Capacity, flat)
			out[i] = NodePrice{n.Name, r.source, r.hourly}
		}
	}
	return out
}

func writeJSON(out io.Writer, e export) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(e)
}

// listPaged reads a list in pages of 500 so a huge cluster never comes back as one response.
func listPaged[T any](ctx context.Context, list func(context.Context, metav1.ListOptions) (runtime.Object, error), opts metav1.ListOptions) ([]T, error) {
	var items []T
	err := pager.New(pager.ListPageFunc(list)).EachListItem(ctx, opts, func(o runtime.Object) error {
		items = append(items, *any(o).(*T))
		return nil
	})
	return items, err
}

// collect also returns the price key of every node that has one, by node name, the PersistentVolumes and the load balancers.
func collect(ctx context.Context, cs kubernetes.Interface, mc metrics.Interface, window time.Duration) ([]report.Node, []report.Pod, map[string]pricing.Key, []report.Volume, []report.LoadBalancer, []report.PDB, int, error) {
	nodeList, err := listPaged[corev1.Node](ctx, func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) {
		return cs.CoreV1().Nodes().List(ctx, o)
	}, metav1.ListOptions{})
	if err != nil {
		return nil, nil, nil, nil, nil, nil, 0, fmt.Errorf("list nodes: %w", err)
	}
	podList, err := listPaged[corev1.Pod](ctx, func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) {
		return cs.CoreV1().Pods("").List(ctx, o)
	}, metav1.ListOptions{FieldSelector: "status.phase=Running"})
	if err != nil {
		return nil, nil, nil, nil, nil, nil, 0, fmt.Errorf("list pods: %w", err)
	}
	owners, memLim, err := ownerIndex(ctx, cs)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, 0, err
	}

	usage, samples := map[string]report.Resources{}, 0
	for deadline := nowFn().Add(window); ; sleepFn(sampleEvery) {
		pm, err := listPaged[metricsv1beta1.PodMetrics](ctx, func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) {
			return mc.MetricsV1beta1().PodMetricses("").List(ctx, o)
		}, metav1.ListOptions{})
		if err != nil && samples > 0 {
			fmt.Fprintf(os.Stderr, "phrontis: metrics sample failed, keeping %d sample(s): %v\n", samples, err)
			break
		}
		if err != nil {
			if prom == nil { // with --prometheus the usage comes from Prometheus: no metrics-server is fine
				fmt.Fprintf(os.Stderr, "phrontis: no usage metrics (is metrics-server installed?): %v\n  costing by requests only, no rightsizing\n", err)
			}
			break
		}
		samples++
		for _, m := range pm {
			for _, c := range m.Containers {
				k := m.Namespace + "/" + m.Name + "/" + c.Name
				usage[k] = report.Max(usage[k], report.Resources{MilliCPU: c.Usage.Cpu().MilliValue(), MemBytes: c.Usage.Memory().Value()})
			}
		}
		if !nowFn().Before(deadline) {
			break
		}
	}

	var nodes []report.Node
	keys := map[string]pricing.Key{}
	for _, n := range nodeList {
		if k, ok := pricing.KeyOf(n.Spec.ProviderID, n.Labels); ok {
			keys[n.Name] = k
		}
		nodes = append(nodes, report.Node{Name: n.Name, Capacity: report.Resources{
			MilliCPU: n.Status.Capacity.Cpu().MilliValue(), MemBytes: n.Status.Capacity.Memory().Value()}, Unschedulable: n.Spec.Unschedulable})
	}
	var pods []report.Pod
	var podLabels []map[string]string // parallel to podList, to resolve PDB selectors
	for _, p := range podList {
		if p.Spec.NodeName == "" {
			continue
		}
		wl, static := workload(&p, owners)
		rp := report.Pod{Namespace: p.Namespace, Name: p.Name, Workload: wl, Node: p.Spec.NodeName, Static: static, Labels: report.KeepLabels(p.Labels)}
		oom := map[string]bool{}
		for _, s := range slices.Concat(p.Status.InitContainerStatuses, p.Status.ContainerStatuses) {
			for _, t := range []*corev1.ContainerStateTerminated{s.State.Terminated, s.LastTerminationState.Terminated} {
				oom[s.Name] = oom[s.Name] || t != nil && t.Reason == "OOMKilled"
			}
		}
		var manifest map[string]int64 // its ReplicaSet's, StatefulSet's or DaemonSet's, if any; else Limit stands in
		if c := metav1.GetControllerOf(&p); c != nil {
			manifest = memLim[p.Namespace+"/"+c.Kind+"/"+c.Name]
		}
		rp.HostNetwork, rp.HostPID, rp.HostIPC = p.Spec.HostNetwork, p.Spec.HostPID, p.Spec.HostIPC
		rp.EvictRead, rp.NoEvict, rp.SafeEvict = true, p.Annotations[report.NoEvictAnnotation] == "false", p.Annotations[report.NoEvictAnnotation] == "true"
		for i, c := range slices.Concat(p.Spec.Containers, sidecars(p.Spec)) {
			u, ok := usage[p.Namespace+"/"+p.Name+"/"+c.Name]
			rp.Containers = append(rp.Containers, report.Container{Name: c.Name, Usage: u, HasUsage: ok, OOMKilled: oom[c.Name],
				Request:          report.Resources{MilliCPU: c.Resources.Requests.Cpu().MilliValue(), MemBytes: c.Resources.Requests.Memory().Value()},
				Limit:            report.Resources{MilliCPU: c.Resources.Limits.Cpu().MilliValue(), MemBytes: c.Resources.Limits.Memory().Value()},
				ManifestMemLimit: manifest[c.Name]})
			rc := &rp.Containers[len(rp.Containers)-1]
			if rc.Sidecar = i >= len(p.Spec.Containers); rc.Sidecar && manifest != nil {
				_, ok := manifest[c.Name]
				rc.Injected = !ok
			}
			posture(rc, p.Spec.SecurityContext, c)
		}
		for _, v := range p.Spec.Volumes {
			rp.HostPath = rp.HostPath || v.HostPath != nil
			rp.EmptyDir = rp.EmptyDir || v.EmptyDir != nil
			if v.PersistentVolumeClaim != nil {
				rp.PVCs = append(rp.PVCs, v.PersistentVolumeClaim.ClaimName)
			}
		}
		pods = append(pods, rp)
		podLabels = append(podLabels, p.Labels)
	}
	pdbs, err := budgets(ctx, cs, pods, podLabels)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, 0, err
	}
	vols, err := volumes(ctx, cs)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, 0, err
	}
	lbs, err := loadBalancers(ctx, cs)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, 0, err
	}
	return nodes, pods, keys, vols, lbs, pdbs, samples, nil
}

// budgets lists the PodDisruptionBudgets and resolves each selector to the workloads of the pods (same namespace) it matches.
// A PDB that matches no collected pod is dropped; one with an empty selector matches every pod of its namespace.
func budgets(ctx context.Context, cs kubernetes.Interface, pods []report.Pod, labels []map[string]string) ([]report.PDB, error) {
	list, err := cs.PolicyV1().PodDisruptionBudgets("").List(ctx, metav1.ListOptions{})
	if apierrors.IsForbidden(err) {
		return nil, nil // older ClusterRole without PDB access: every workload is judged "no PDB", which is the safe answer
	}
	if err != nil {
		return nil, fmt.Errorf("list poddisruptionbudgets: %w", err)
	}
	var out []report.PDB
	for _, b := range list.Items {
		sel, err := metav1.LabelSelectorAsSelector(b.Spec.Selector)
		if err != nil {
			continue // an invalid selector protects nothing
		}
		pdb := report.PDB{Namespace: b.Namespace, Name: b.Name, Strict: b.Status.ExpectedPods > 0 && b.Status.DisruptionsAllowed == 0}
		for i, p := range pods {
			if p.Namespace == b.Namespace && sel.Matches(k8slabels.Set(labels[i])) && !slices.Contains(pdb.Workloads, p.Workload) {
				pdb.Workloads = append(pdb.Workloads, p.Workload)
			}
		}
		if len(pdb.Workloads) > 0 {
			out = append(out, pdb)
		}
	}
	return out, nil
}

// listLimitRanges reads each namespace's LimitRange items so a recommendation can say that its size would be rejected.
// Forbidden (an older ClusterRole) or any other failure only drops the notes, with one warning.
func listLimitRanges(ctx context.Context, cs kubernetes.Interface, warn io.Writer) map[string][]corev1.LimitRangeItem {
	list, err := cs.CoreV1().LimitRanges("").List(ctx, metav1.ListOptions{})
	if err != nil {
		fmt.Fprintf(warn, "phrontis: cannot list LimitRanges (%v); recommendations are not checked against them\n", err)
		return nil
	}
	out := map[string][]corev1.LimitRangeItem{}
	for _, l := range list.Items {
		out[l.Namespace] = append(out[l.Namespace], l.Spec.Limits...)
	}
	return out
}

// listHPAs reads the HorizontalPodAutoscalers so recommendations can say which workloads an HPA scales.
// Forbidden (an older ClusterRole) or any other failure only drops the marks, with one warning.
func listHPAs(ctx context.Context, cs kubernetes.Interface, warn io.Writer) []report.HPA {
	list, err := cs.AutoscalingV2().HorizontalPodAutoscalers("").List(ctx, metav1.ListOptions{})
	if err != nil {
		fmt.Fprintf(warn, "phrontis: cannot list HorizontalPodAutoscalers (%v); recommendations carry no HPA marks\n", err)
		return nil
	}
	var out []report.HPA
	for _, h := range list.Items {
		a := report.HPA{Namespace: h.Namespace, Name: h.Name, Target: h.Spec.ScaleTargetRef.Kind + "/" + h.Spec.ScaleTargetRef.Name}
		for _, m := range h.Spec.Metrics {
			var n corev1.ResourceName
			switch {
			case m.Resource != nil:
				n = m.Resource.Name
			case m.ContainerResource != nil:
				n = m.ContainerResource.Name
			}
			if (n == corev1.ResourceCPU || n == corev1.ResourceMemory) && !slices.Contains(a.Resources, string(n)) {
				a.Resources = append(a.Resources, string(n))
			}
			if u := utilization(m); u > 0 {
				c := ""
				if m.ContainerResource != nil {
					c = m.ContainerResource.Container
				}
				// A ContainerResource target is the more specific one: it wins over a pod-wide Resource target.
				if n == corev1.ResourceCPU && (c != "" || a.CPUContainer == "") {
					a.CPUTarget, a.CPUContainer = u, c
				} else if n == corev1.ResourceMemory && (c != "" || a.MemContainer == "") {
					a.MemTarget, a.MemContainer = u, c
				}
			}
		}
		a.MinReplicas = 1 // the API default
		if h.Spec.MinReplicas != nil {
			a.MinReplicas = int(*h.Spec.MinReplicas)
		}
		a.Replicas, a.MaxReplicas = int(h.Status.CurrentReplicas), int(h.Spec.MaxReplicas)
		out = append(out, a)
	}
	return out
}

// utilization is the averageUtilization percent of a Resource or ContainerResource metric; 0 for an averageValue target.
func utilization(m autoscalingv2.MetricSpec) int {
	var t autoscalingv2.MetricTarget
	switch {
	case m.Resource != nil:
		t = m.Resource.Target
	case m.ContainerResource != nil:
		t = m.ContainerResource.Target
	}
	if t.AverageUtilization == nil {
		return 0
	}
	return int(*t.AverageUtilization)
}

const ebsDriver = "ebs.csi.aws.com"

// volumes lists the PersistentVolumes. An EBS CSI volume's type is its StorageClass's "type" parameter, or gp3 when the
// class sets none (the driver's default); a missing class leaves the type empty. The in-tree awsElasticBlockStore driver is gone from Kubernetes.
func volumes(ctx context.Context, cs kubernetes.Interface) ([]report.Volume, error) {
	pvs, err := cs.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list persistentvolumes: %w", err)
	}
	scs, err := cs.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list storageclasses: %w", err)
	}
	classes := map[string]map[string]string{}
	for _, c := range scs.Items {
		classes[c.Name] = c.Parameters
	}
	var out []report.Volume
	for _, pv := range pvs.Items {
		v := report.Volume{Name: pv.Name, Bytes: pv.Spec.Capacity.Storage().Value(), Class: pv.Spec.StorageClassName, Created: pv.CreationTimestamp.Time}
		if c := pv.Spec.ClaimRef; c != nil && pv.Status.Phase == corev1.VolumeBound {
			v.Namespace, v.Claim = c.Namespace, c.Name
		}
		if pv.Spec.CSI != nil && pv.Spec.CSI.Driver == ebsDriver {
			if params, ok := classes[pv.Spec.StorageClassName]; ok {
				if v.Type = params["type"]; v.Type == "" {
					v.Type = "gp3"
				}
			}
		}
		out = append(out, v)
	}
	return out, nil
}

// loadBalancers lists Services of type LoadBalancer that have an ingress (a pending one bills nothing yet).
// Kind follows the Kubernetes/EKS docs: a loadBalancerClass of service.k8s.aws/nlb or eks.amazonaws.com/nlb is an NLB, any other
// class belongs to another controller (unknown). With no class, the service.beta.kubernetes.io/aws-load-balancer-type
// annotation decides: nlb, external and nlb-ip are NLB; absent is the AWS cloud provider's Classic default; else unknown.
func loadBalancers(ctx context.Context, cs kubernetes.Interface) ([]report.LoadBalancer, error) {
	svcs, err := cs.CoreV1().Services("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list services: %w", err)
	}
	var out []report.LoadBalancer
	for _, s := range svcs.Items {
		if s.Spec.Type == corev1.ServiceTypeLoadBalancer && len(s.Status.LoadBalancer.Ingress) > 0 {
			out = append(out, report.LoadBalancer{Name: s.Name, Namespace: s.Namespace, Kind: lbKind(s)})
		}
	}
	return out, nil
}

func lbKind(s corev1.Service) string {
	if s.Spec.LoadBalancerClass != nil && *s.Spec.LoadBalancerClass != "" {
		if c := *s.Spec.LoadBalancerClass; c == "service.k8s.aws/nlb" || c == "eks.amazonaws.com/nlb" {
			return "nlb"
		}
		return "unknown"
	}
	switch t, ok := s.Annotations["service.beta.kubernetes.io/aws-load-balancer-type"]; {
	case !ok:
		return "classic"
	case t == "nlb" || t == "external" || t == "nlb-ip":
		return "nlb"
	}
	return "unknown"
}

// sidecars are the native sidecars of s: init containers that run as long as the pod (spec 031).
func sidecars(s corev1.PodSpec) (out []corev1.Container) {
	for _, c := range s.InitContainers {
		if c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			out = append(out, c)
		}
	}
	return out
}

// ownerIndex maps "ns/ReplicaSet/x" and "ns/Job/x" to their controller, so pods roll up to a Deployment or CronJob.
// memLim maps "ns/Kind/x" of each ReplicaSet, StatefulSet and DaemonSet to its template's memory limit per container: the
// manifest's, which the webhook may have raised in the pod but caps memory by (report.MemCap). A container the template
// sets none for has its namespace's LimitRange default: the API sets it before the webhook sees the pod (#147).
// An older ClusterRole without StatefulSets, DaemonSets or LimitRanges leaves those out, and the pod's Limit stands in.
func ownerIndex(ctx context.Context, cs kubernetes.Interface) (idx map[string]string, memLim map[string]map[string]int64, err error) {
	idx, memLim = map[string]string{}, map[string]map[string]int64{}
	lrs, err := cs.CoreV1().LimitRanges("").List(ctx, metav1.ListOptions{})
	if err != nil && !apierrors.IsForbidden(err) {
		return nil, nil, fmt.Errorf("list limitranges: %w", err)
	}
	def := map[string]int64{} // the largest of several: the API picks any, so the cap is never stricter than the webhook's
	for _, lr := range lrs.Items {
		for _, it := range lr.Spec.Limits {
			if it.Type == corev1.LimitTypeContainer {
				def[lr.Namespace] = max(def[lr.Namespace], it.Default.Memory().Value())
			}
		}
	}
	tmpl := func(ns, kind, name string, t corev1.PodTemplateSpec) {
		k := ns + "/" + kind + "/" + name
		memLim[k] = map[string]int64{}
		for _, c := range slices.Concat(t.Spec.Containers, sidecars(t.Spec)) {
			memLim[k][c.Name] = cmp.Or(c.Resources.Limits.Memory().Value(), def[ns])
		}
	}
	rss, err := cs.AppsV1().ReplicaSets("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("list replicasets: %w", err)
	}
	for _, o := range rss.Items {
		if c := metav1.GetControllerOf(&o); c != nil {
			idx[o.Namespace+"/ReplicaSet/"+o.Name] = c.Kind + "/" + c.Name
		}
		tmpl(o.Namespace, "ReplicaSet", o.Name, o.Spec.Template)
	}
	sts, err := cs.AppsV1().StatefulSets("").List(ctx, metav1.ListOptions{})
	if err != nil && !apierrors.IsForbidden(err) {
		return nil, nil, fmt.Errorf("list statefulsets: %w", err)
	}
	for _, o := range sts.Items {
		tmpl(o.Namespace, "StatefulSet", o.Name, o.Spec.Template)
	}
	dss, err := cs.AppsV1().DaemonSets("").List(ctx, metav1.ListOptions{})
	if err != nil && !apierrors.IsForbidden(err) {
		return nil, nil, fmt.Errorf("list daemonsets: %w", err)
	}
	for _, o := range dss.Items {
		tmpl(o.Namespace, "DaemonSet", o.Name, o.Spec.Template)
	}
	jobs, err := cs.BatchV1().Jobs("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("list jobs: %w", err)
	}
	for _, o := range jobs.Items {
		if c := metav1.GetControllerOf(&o); c != nil {
			idx[o.Namespace+"/Job/"+o.Name] = c.Kind + "/" + c.Name
		}
	}
	return idx, memLim, nil
}

// workload names the pod's top controller; static is true for mirror pods (kubelet manifests, owned by the Node).
func workload(p *corev1.Pod, owners map[string]string) (name string, static bool) {
	c := metav1.GetControllerOf(p)
	if c == nil || c.Kind == "Node" {
		return "Pod/" + p.Name, c != nil
	}
	self := c.Kind + "/" + c.Name
	if up, ok := owners[p.Namespace+"/"+self]; ok {
		return up, false
	}
	return self, false
}

func basis(windowed bool) string {
	if windowed {
		return "p95 CPU, peak memory"
	}
	return "peak"
}

func render(out io.Writer, r report.Report, p report.Prices, np []NodePrice, samples int, window time.Duration, windowed bool) {
	idlePct := 0.0
	if r.Monthly > 0 {
		idlePct = 100 * r.IdleMonthly / r.Monthly
	}
	fmt.Fprintf(out, "cluster  %d nodes · %s vCPU · %.1f GiB · %.2f/month · idle %.2f (%.0f%%)\n",
		r.Nodes, cores(r.Capacity.MilliCPU), float64(r.Capacity.MemBytes)/(1<<30), r.Monthly, r.IdleMonthly, idlePct)
	bySource := map[string]int{}
	for _, x := range np {
		bySource[x.Source]++
	}
	if len(bySource) == 0 || len(bySource) == 1 && bySource["flat"] > 0 {
		fmt.Fprintf(out, "prices   %.4f per vCPU-hour, %.4f per GiB-hour (flat; set --cpu-hour/--gib-hour from your bill)\n", p.CPUHour, p.GiBHour)
	} else {
		var parts []string
		for _, s := range slices.Sorted(maps.Keys(bySource)) {
			parts = append(parts, fmt.Sprintf("%d %s", bySource[s], s))
		}
		fmt.Fprintf(out, "prices   nodes: %s; flat rate %.4f per vCPU-hour, %.4f per GiB-hour where \"flat\"\n", strings.Join(parts, ", "), p.CPUHour, p.GiBHour)
		fmt.Fprintln(out, "         cloud prices are USD list prices, before discounts or savings plans")
	}
	fmt.Fprintf(out, "usage    %s of %d sample(s) over %s", basis(windowed), samples, window)
	if window < 24*time.Hour {
		fmt.Fprint(out, "; size on days of data, not minutes")
	}
	fmt.Fprint(out, "\n")
	idleHead, idleCols := "", func(report.Line) string { return "" }
	if shareIdle {
		fmt.Fprintf(out, "idle     shared per node by cost; %.2f of nodes with no charged pods is in no row\n", r.IdleUnshared)
		idleHead = "\tIDLE SHARE\tWITH IDLE"
		idleCols = func(l report.Line) string { return fmt.Sprintf("\t%.2f\t%.2f", l.IdleShare, l.Monthly+l.IdleShare) }
	}
	fmt.Fprintln(out)

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAMESPACE\tPODS\tCOST/MONTH"+idleHead+"\tCPU USED/REQ\tMEM USED/REQ")
	for _, l := range r.Namespaces {
		fmt.Fprintf(w, "%s\t%d\t%.2f%s\t%s\t%s\n", l.Namespace, l.Pods, l.Monthly, idleCols(l), percent(l.CPUPct), percent(l.MemPct))
	}
	w.Flush()
	fmt.Fprintln(out)
	w = tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "WORKLOAD\tNAMESPACE\tPODS\tCOST/MONTH"+idleHead+"\tCPU USED/REQ\tMEM USED/REQ")
	for _, l := range r.Workloads {
		fmt.Fprintf(w, "%s\t%s\t%d\t%.2f%s\t%s\t%s\n", l.Workload, l.Namespace, l.Pods, l.Monthly, idleCols(l), percent(l.CPUPct), percent(l.MemPct))
	}
	w.Flush()

	if len(r.Recs) == 0 {
		fmt.Fprintln(out, "\nno rightsizing changes worth making")
		return
	}
	var total float64
	head := "peak +20% CPU, +30% memory"
	if windowed {
		head = "p95 CPU +20%, peak memory +30%"
	}
	fmt.Fprintf(out, "\nRIGHTSIZING (%s; memory never below an OOM kill)\n", head)
	w = tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "WORKLOAD\tNAMESPACE\tCONTAINER\tPODS\tCPU REQ → REC\tMEM REQ → REC\tSAVING/MONTH\tNOTE")
	for _, x := range r.Recs {
		total += x.MonthlySaving
	}
	shown := top(r.Recs)
	for _, x := range shown {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s → %s\t%s → %s\t%.2f\t%s\n", x.Workload, x.Namespace, x.Container, x.Pods,
			milli(x.Request.MilliCPU), milli(x.Recommended.MilliCPU), mebi(x.Request.MemBytes), mebi(x.Recommended.MemBytes), x.MonthlySaving, note(x))
	}
	w.Flush()
	fmt.Fprintln(out, "\nWHY")
	for _, x := range shown {
		fmt.Fprintf(out, "%s/%s/%s: %s\n", x.Namespace, x.Workload, x.Container, x.Explanation)
	}
	if n := len(r.Recs) - len(shown); n > 0 {
		fmt.Fprintf(out, "\n%d more recommendation(s) with a smaller saving not shown (--top %d); the net saving below covers them too\n", n, topN)
	}
	fmt.Fprintf(out, "\nnet saving %.2f/month; it reaches the bill only once nodes scale down to fit\n", total)
}

// renderRisks prints the workloads at risk; nothing when there are none.
func renderRisks(out io.Writer, risks []report.Risk) {
	if len(risks) == 0 {
		return
	}
	fmt.Fprintf(out, "\nAT RISK (%d)\n", len(risks))
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "WORKLOAD\tNAMESPACE\tCONTAINER\tREASON")
	for _, x := range risks {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", x.Workload, x.Namespace, cmp.Or(x.Container, "-"), x.Detail)
	}
	w.Flush()
}

// percent prints a share of the request; "-" when nothing measured has one.
func percent(p *float64) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", *p)
}

// note says why a rec is unusual and which limit must rise with it (a request above its limit is rejected).
func note(x report.Rec) string {
	var n []string
	switch {
	case x.Injected:
		n = append(n, "sidecar, not in the manifest")
	case x.Sidecar:
		n = append(n, "sidecar")
	}
	if x.OOMKilled {
		n = append(n, "OOM-killed")
	}
	if x.Throttled {
		n = append(n, "CPU-throttled")
	}
	if x.Limit.MilliCPU > 0 && x.Recommended.MilliCPU > x.Limit.MilliCPU {
		n = append(n, "CPU limit "+milli(x.Limit.MilliCPU)+" → "+milli(x.Recommended.MilliCPU))
	}
	if x.Limit.MemBytes > 0 && x.Recommended.MemBytes > x.Limit.MemBytes {
		n = append(n, "mem limit "+mebi(x.Limit.MemBytes)+" → "+mebi(x.Recommended.MemBytes))
	}
	if h := x.HPA; h != nil {
		if len(h.Advice) == 0 {
			n = append(n, "HPA: "+h.Name+hpaRes(h))
		}
		for _, a := range h.Advice { // the advice names the HPA itself
			n = append(n, a.Text(h.Name))
		}
	}
	if x.LimitRange {
		n = append(n, "the namespace's LimitRange rejects this size")
	}
	return strings.Join(n, "; ")
}

// renderStorage prints the STORAGE section; nothing when the cluster has no volumes.
func renderStorage(out io.Writer, s report.Storage) {
	if s.Total.Volumes == 0 {
		return
	}
	fmt.Fprintln(out, "\nSTORAGE (PersistentVolumes, provisioned GiB; separate from compute above)")
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAMESPACE\tVOLUMES\tGiB\tCOST/MONTH\tNOTE")
	row := func(name string, l report.StorageLine) {
		note := ""
		if l.Unpriced > 0 {
			note = fmt.Sprintf("%d unpriced", l.Unpriced)
		}
		fmt.Fprintf(w, "%s\t%d\t%.1f\t%.2f\t%s\n", name, l.Volumes, l.GiB, l.Monthly, note)
	}
	for _, l := range s.Namespaces {
		row(l.Namespace, l)
	}
	if s.Unclaimed.Volumes > 0 {
		row("(unclaimed: no PVC bound)", s.Unclaimed)
	}
	row("total", s.Total)
	w.Flush()
}

// renderLoadBalancers prints the LOAD BALANCERS section; nothing when there are none.
func renderLoadBalancers(out io.Writer, s report.LoadBalancers) {
	if s.Total.Count == 0 {
		return
	}
	fmt.Fprintln(out, "\nLOAD BALANCERS (hourly charge only; LCU and data processed are usage-based and not included; separate from compute above)")
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAMESPACE\tLOAD BALANCERS\tCOST/MONTH\tNOTE")
	row := func(name string, l report.LBLine) {
		note := ""
		if l.Unpriced > 0 {
			note = fmt.Sprintf("%d unpriced", l.Unpriced)
		}
		fmt.Fprintf(w, "%s\t%d\t%.2f\t%s\n", name, l.Count, l.Monthly, note)
	}
	for _, l := range s.Namespaces {
		row(l.Namespace, l)
	}
	row("total", s.Total)
	w.Flush()
}

func cores(m int64) string { return fmt.Sprintf("%.1f", float64(m)/1000) }
func milli(m int64) string {
	if m == 0 {
		return "none"
	}
	return fmt.Sprintf("%dm", m)
}
func mebi(b int64) string {
	if b == 0 {
		return "none"
	}
	return fmt.Sprintf("%dMi", (b+(1<<20)-1)>>20)
}

func hpaRes(h *report.HPARef) string {
	if len(h.Resources) == 0 {
		return ""
	}
	return " (" + strings.Join(h.Resources, ", ") + ")"
}
