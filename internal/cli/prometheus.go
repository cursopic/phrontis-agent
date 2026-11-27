package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/cursopic/phrontis/internal/report"
)

// promTimeout bounds one query (spec 038); a var so a test need not wait a minute.
var promTimeout = 60 * time.Second

const (
	minHistory = time.Hour
	maxHistory = 30 * 24 * time.Hour
)

// parseHistory reads a Go duration plus a "d" suffix for days ("7d", "1.5d"), 1h to 30d.
func parseHistory(v string) (time.Duration, error) {
	var d time.Duration
	var err error
	if days, ok := strings.CutSuffix(v, "d"); ok {
		var f float64
		if f, err = strconv.ParseFloat(days, 64); err == nil {
			d = time.Duration(f * float64(24*time.Hour))
		}
	} else {
		d, err = time.ParseDuration(v)
	}
	if err != nil {
		return 0, fmt.Errorf("--history %q is not a duration like 7d or 36h", v)
	}
	if d < minHistory || d > maxHistory {
		return 0, errors.New("--history must be between 1h and 30d")
	}
	return d, nil
}

// promSource is where --prometheus reads history from; selector is added to both queries as is.
type promSource struct {
	url, token, selector string
	history              time.Duration
}

func (s promSource) queries() (cpu, mem string) {
	sel := `container!="",container!="POD"`
	if s.selector != "" {
		sel += "," + s.selector
	}
	h := fmt.Sprintf("%ds", int(s.history.Seconds()))
	cpu = fmt.Sprintf(`quantile_over_time(0.95, sum by (namespace, pod, container) (rate(container_cpu_usage_seconds_total{%s}[5m]))[%s:5m])`, sel, h)
	mem = fmt.Sprintf(`max_over_time(container_memory_working_set_bytes{%s}[%s])`, sel, h)
	return
}

type promSeries struct {
	ns, pod, container string
	val                float64
}

// query runs one instant query. Errors carry the HTTP code at most: never the body, the URL or the token.
func (s promSource) query(ctx context.Context, q string) ([]promSeries, error) {
	ctx, cancel := context.WithTimeout(ctx, promTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(s.url, "/")+"/api/v1/query?"+url.Values{"query": {q}}.Encode(), nil)
	if err != nil {
		return nil, errors.New("prometheus: bad URL")
	}
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	// No redirects: the token goes to the URL the user gave and nowhere else.
	cl := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := cl.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("prometheus: no answer in %s", promTimeout)
		}
		return nil, errors.New("prometheus: request failed (cannot connect)")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prometheus: HTTP %d", resp.StatusCode)
	}
	var body struct {
		Status string
		Data   struct {
			Result []struct {
				Metric map[string]string
				Value  [2]any
			}
		}
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 256<<20)).Decode(&body); err != nil || body.Status != "success" {
		return nil, errors.New("prometheus: HTTP 200 but not a query result")
	}
	var out []promSeries
	for _, r := range body.Data.Result {
		str, _ := r.Value[1].(string)
		v, err := strconv.ParseFloat(str, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			continue
		}
		out = append(out, promSeries{r.Metric["namespace"], r.Metric["pod"], r.Metric["container"], v})
	}
	return out, nil
}

// applyHistory replaces every container's usage with p95 CPU and peak memory read from Prometheus, and returns the
// number of series used. Like report.Window, usage is per workload and container, shared by its replicas: CPU is the
// highest p95 of its pods, memory the highest peak. A container with no series gets no usage.
func (s promSource) applyHistory(ctx context.Context, pods []report.Pod) (int, error) {
	cpuQ, memQ := s.queries()
	cpu, err := s.query(ctx, cpuQ)
	if err != nil {
		return 0, err
	}
	mem, err := s.query(ctx, memQ)
	if err != nil {
		return 0, err
	}
	wl := map[[2]string]string{}  // namespace/pod -> workload
	known := map[[2]string]bool{} // namespace/workload exists in the snapshot
	for _, p := range pods {
		wl[[2]string{p.Namespace, p.Name}] = p.Workload
		known[[2]string{p.Namespace, p.Workload}] = true
	}
	use := map[promKey]report.Resources{}
	has := map[promKey]bool{}
	n := 0
	fold := func(series []promSeries, set func(*report.Resources, float64)) {
		for _, x := range series {
			w, ok := wl[[2]string{x.ns, x.pod}]
			if !ok {
				w, ok = vanishedWorkload(x.ns, x.pod, known)
			}
			if !ok {
				continue
			}
			k := promKey{x.ns, w, x.container}
			r := use[k]
			set(&r, x.val)
			use[k], has[k] = r, true
			n++
		}
	}
	fold(cpu, func(r *report.Resources, v float64) { r.MilliCPU = max(r.MilliCPU, int64(math.Round(v*1000))) })
	fold(mem, func(r *report.Resources, v float64) { r.MemBytes = max(r.MemBytes, int64(v)) })
	for i := range pods {
		for j := range pods[i].Containers {
			c := &pods[i].Containers[j]
			k := promKey{pods[i].Namespace, pods[i].Workload, c.Name}
			c.Usage, c.HasUsage = use[k], has[k]
		}
	}
	return n, nil
}

// vanishedWorkload names the workload of a pod that no longer runs, from its name alone: a Deployment pod is
// <name>-<replicaset hash>-<suffix>, a StatefulSet pod <name>-<ordinal>, a DaemonSet or Job pod <name>-<suffix>.
// shortcut: a name that fits no workload still in the cluster is dropped; the owner of a deleted pod is gone with it.
func vanishedWorkload(ns, pod string, known map[[2]string]bool) (string, bool) {
	parts := strings.Split(pod, "-")
	for cut, kinds := range [][]string{{"StatefulSet", "DaemonSet", "Job"}, {"Deployment"}} {
		if len(parts) <= cut+1 {
			continue
		}
		base := strings.Join(parts[:len(parts)-1-cut], "-")
		for _, k := range kinds {
			if w := k + "/" + base; known[[2]string{ns, w}] {
				return w, true
			}
		}
	}
	return "", false
}

// readToken reads the bearer token from path; it is never printed.
func readToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("cannot read --prometheus-token-file")
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "", errors.New("--prometheus-token-file is empty")
	}
	return t, nil
}

// prom is the --prometheus source; nil without the flag.
var prom *promSource

// setupProm checks the flags and builds prom before anything is queried. other: a mode that cannot use it.
func setupProm(u, tokenFile, selector, history string, historySet bool, window time.Duration, other bool) error {
	if u == "" {
		if tokenFile != "" || selector != "" || historySet {
			return errors.New("--prometheus-token-file, --prometheus-selector and --history need --prometheus")
		}
		return nil
	}
	if other {
		return errors.New("--prometheus works only with one live report: not with --api, --security, --db or several contexts")
	}
	if window != 0 {
		return errors.New("use only one of --prometheus and --window")
	}
	h, err := parseHistory(history)
	if err != nil {
		return err
	}
	src := promSource{url: u, selector: selector, history: h}
	if tokenFile != "" {
		if src.token, err = readToken(tokenFile); err != nil {
			return err
		}
	}
	prom = &src
	return nil
}

func flagSet(name string) (set bool) {
	flag.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return
}

type promKey struct{ ns, wl, c string }
