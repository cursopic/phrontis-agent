package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/cursopic/phrontis/internal/manifest"
	"github.com/cursopic/phrontis/internal/mcp"
	"github.com/cursopic/phrontis/internal/report"
)

// serveMCP runs `phrontis mcp`: a read-only MCP server on stdin/stdout. Every tool call reads the cluster afresh
// through the kubeconfig (list/get only) and returns parts of the same document `--json` prints.
func serveMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	kctx := fs.String("context", "", "kubeconfig context (default: current)")
	cpuHour := fs.Float64("cpu-hour", DefaultPrices.CPUHour, "price of one vCPU for one hour")
	gibHour := fs.Float64("gib-hour", DefaultPrices.GiBHour, "price of one GiB of memory for one hour")
	cloud := fs.Bool("cloud-prices", true, "price nodes from AWS/Azure public list prices (USD); false = flat prices only")
	cluster := fs.String("cluster", "", "Phrontis cluster id (Clusters page); the open_pr_link tool needs it")
	appURL := fs.String("app-url", defaultAppURL, "dashboard address that open_pr_link links to")
	fs.Parse(args)
	if err := checkAppURL(*appURL); err != nil {
		return err
	}
	prices := report.Prices{CPUHour: *cpuHour, GiBHour: *gibHour}
	pr := newPricer(*cloud)
	build := func() (export, error) {
		cs, mc, label, err := clients(*kctx)
		if err != nil {
			return export{}, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		nodes, pods, keys, vols, lbs, pdbs, samples, err := collect(ctx, cs, mc, 0)
		if err != nil {
			return export{}, err
		}
		return assemble(ctx, snapshot{label, nodes, pods, keys, vols, lbs, samples, 0, false, pdbs, listHPAs(ctx, cs, os.Stderr), listLimitRanges(ctx, cs, os.Stderr)}, prices, pr, os.Stderr, time.Now().UTC()), nil
	}
	return mcpServer(build, *cluster, *appURL).Serve(os.Stdin, os.Stdout)
}

const defaultAppURL = "https://app.phrontis.ai"

// checkAppURL accepts https, or http for exactly localhost or 127.0.0.1 (port allowed), with no userinfo, path, query or fragment.
func checkAppURL(s string) error {
	u, err := url.Parse(s)
	bad := fmt.Errorf("--app-url %q: use https://host or http://localhost[:port], with no user, path, query or fragment", s)
	if err != nil || u.Opaque != "" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || strings.ContainsAny(s, "?#") {
		return bad
	}
	if u.Scheme == "https" || u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1") {
		return nil
	}
	return bad
}

// findRec returns the recommendation for ns, workload and container in a fresh report.
func findRec(build func() (export, error), ns, workload, container string) (report.Rec, error) {
	e, err := build()
	if err != nil {
		return report.Rec{}, err
	}
	for _, r := range e.Recs {
		if r.Namespace == ns && r.Workload == workload && r.Container == container {
			return r, nil
		}
	}
	return report.Rec{}, fmt.Errorf("No recommendation for %s/%s/%s", ns, workload, container)
}

var recSchema = func(extra map[string]any) map[string]any {
	props := map[string]any{
		"namespace": map[string]any{"type": "string", "description": "Namespace of the recommendation"},
		"workload":  map[string]any{"type": "string", "description": "Kind/name, for example Deployment/api"},
		"container": map[string]any{"type": "string", "description": "Container name"},
	}
	for k, v := range extra {
		props[k] = v
	}
	return map[string]any{"type": "object", "additionalProperties": false, "required": []string{"namespace", "workload", "container"}, "properties": props}
}

// mcpServer exposes the report built by build as seven read-only tools. cluster and appURL (already checked) only shape open_pr_link.
func mcpServer(build func() (export, error), cluster, appURL string) *mcp.Server {
	none := map[string]any{"type": "object", "additionalProperties": false}
	return &mcp.Server{
		Name: "phrontis", Version: "0.1.0",
		Instructions: "Read-only Kubernetes cost report. Prices are USD per month unless a field says otherwise; usage is a single live sample.",
		Tools: []mcp.Tool{
			{Name: "cost_summary", Description: "Cluster totals: node count, capacity, monthly cost, idle (unallocated) monthly cost, prices used, per-node prices and any price warnings.",
				InputSchema: none, Call: func(json.RawMessage) (any, error) {
					e, err := build()
					if err != nil {
						return nil, err
					}
					return summary{e.Context, e.Nodes, e.Capacity, e.Monthly, e.IdleMonthly, e.Prices, e.NodePrices, e.Samples, e.Window, e.Basis, e.At, e.Warnings}, nil
				}},
			{Name: "cost_by_namespace", Description: "Monthly cost of every namespace (requests plus burst above request), sorted as the report sorts them.",
				InputSchema: none, Call: func(json.RawMessage) (any, error) {
					e, err := build()
					if err != nil {
						return nil, err
					}
					return e.Namespaces, nil
				}},
			{Name: "rightsizing", Description: "Rightsizing recommendations per container: current request, recommended request and monthly saving (negative when the request must grow). Optional namespace filter. Read-only: nothing is applied.",
				InputSchema: map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
					"namespace": map[string]any{"type": "string", "description": "Only recommendations in this namespace"}}},
				Call: func(raw json.RawMessage) (any, error) {
					var a struct{ Namespace string }
					if len(raw) > 0 {
						if err := json.Unmarshal(raw, &a); err != nil {
							return nil, fmt.Errorf("invalid arguments: %w", err)
						}
					}
					e, err := build()
					if err != nil {
						return nil, err
					}
					recs := []report.Rec{}
					for _, r := range e.Recs {
						if a.Namespace == "" || r.Namespace == a.Namespace {
							recs = append(recs, r)
						}
					}
					return recs, nil
				}},
			{Name: "spot_candidates", Description: "Which workloads can run on spot (Priced, Note, Saving per month, Kept on-demand, Workloads with a verdict each). When spot prices are not available, Priced is false, Saving is 0 and Note says why. Read-only: nothing is applied.",
				InputSchema: none, Call: func(json.RawMessage) (any, error) {
					e, err := build()
					if err != nil {
						return nil, err
					}
					if e.Spot == nil {
						return spotReport{Note: "no workloads to judge", Workloads: []report.SpotLine{}}, nil
					}
					return e.Spot, nil
				}},
			{Name: "node_savings", Description: "Node cost before and after: what the nodes cost now (Current) against the cheapest simulated Karpenter pool (Simulated), with Saving per month. Without one AWS region's prices, or when no pod fits a pool, there is no simulation: Available is false and Note says so. Read-only: nothing is applied.",
				InputSchema: none, Call: func(json.RawMessage) (any, error) {
					e, err := build()
					if err != nil {
						return nil, err
					}
					p := e.NodePool
					if p == nil {
						return nodeSavings{Current: e.Monthly, Note: "no node simulation: it needs cloud prices for nodes in exactly one AWS region and at least one pod that fits a pool (see cost_summary Warnings)"}, nil
					}
					return nodeSavings{true, "", p.Current, p.Monthly, p.Current - p.Monthly, p.Type, p.Nodes, p.Families, p.SpotShare, p.Unplaced}, nil
				}},
			{Name: "patch", Description: "The YAML patch that sets the recommended requests on one container, and its file name (the same as Download in the dashboard). Read-only: nothing is applied. A person opens the PR from the link.",
				InputSchema: recSchema(nil), Call: func(raw json.RawMessage) (any, error) {
					var a struct{ Namespace, Workload, Container string }
					if err := json.Unmarshal(raw, &a); err != nil {
						return nil, fmt.Errorf("invalid arguments: %w", err)
					}
					x, err := findRec(build, a.Namespace, a.Workload, a.Container)
					if err != nil {
						return nil, err
					}
					y, ok := manifest.Patch(x)
					if !ok {
						return nil, fmt.Errorf("%s: no patch for this kind, so set the requests in its manifest.", x.Workload)
					}
					return map[string]string{"File": manifest.PatchFile(x), "Patch": y}, nil
				}},
			{Name: "open_pr_link", Description: "A link to the Phrontis dashboard that opens the Patch of one recommendation with the Open PR form filled in (optional repo as owner/name and path to the manifest in it). Read-only: nothing is applied. A person opens the PR from the link. The --cluster id is not checked against the kubeconfig: a wrong id links to another cluster's recommendation.",
				InputSchema: recSchema(map[string]any{
					"repo": map[string]any{"type": "string", "description": "GitHub repo as owner/name, for example acme/infra"},
					"path": map[string]any{"type": "string", "description": "Manifest path in the repo, for example k8s/api.yaml"}}),
				Call: func(raw json.RawMessage) (any, error) {
					var a struct{ Namespace, Workload, Container, Repo, Path string }
					if err := json.Unmarshal(raw, &a); err != nil {
						return nil, fmt.Errorf("invalid arguments: %w", err)
					}
					if cluster == "" {
						return nil, errors.New("Set --cluster to the Phrontis cluster id (Clusters page)")
					}
					repo, path := a.Repo, a.Path
					if repo == "" {
						repo = "a/b"
					}
					if path == "" {
						path = "a.yaml"
					}
					if f, msg := manifest.CheckPRInput(repo, path); (f == "repo" && a.Repo != "") || (f == "path" && a.Path != "") {
						return nil, fmt.Errorf("%s: %s", f, msg)
					}
					x, err := findRec(build, a.Namespace, a.Workload, a.Container)
					if err != nil {
						return nil, err
					}
					if _, ok := manifest.Patch(x); !ok {
						return nil, fmt.Errorf("%s: no patch for this kind, so set the requests in its manifest.", x.Workload)
					}
					q := url.Values{"cluster": {cluster}, "pr": {strings.Join([]string{x.Namespace, x.Workload, x.Container}, "/")}}
					if a.Repo != "" {
						q.Set("repo", a.Repo)
					}
					if a.Path != "" {
						q.Set("path", a.Path)
					}
					return map[string]string{"Link": appURL + "/?" + q.Encode()}, nil
				}},
		},
	}
}

// nodeSavings is the before/after of the node simulation (Saving is negative when the simulated pool costs more).
type nodeSavings struct {
	Available         bool
	Note              string `json:",omitempty"`
	Current           float64
	Simulated, Saving float64
	Type              string `json:",omitempty"`
	Nodes             int
	Families          []string `json:",omitempty"`
	SpotShare         float64
	Unplaced          int
}

// summary is the totals part of the --json document (same field names).
type summary struct {
	Context              string
	Nodes                int
	Capacity             report.Resources
	Monthly, IdleMonthly float64
	Prices               report.Prices
	NodePrices           []NodePrice
	Samples              int
	Window, Basis        string
	At                   time.Time
	Warnings             []string `json:",omitempty"`
}
