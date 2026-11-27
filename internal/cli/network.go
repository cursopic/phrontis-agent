package cli

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/cursopic/phrontis/internal/pricing"
	"github.com/cursopic/phrontis/internal/report"
)

// networkBasis marks every figure as a ceiling, never a bill (spec 040).
const networkBasis = "upper-bound"

// networkCaption is shown under the table. It needs a Lawyer check before release (owner rule for cost labels).
const networkCaption = "All traffic is priced at the internet rate; same-zone traffic is free, so the real cost is lower."

// networkEgress is one namespace's outbound traffic over the --history window and its monthly price at the internet rate.
type networkEgress struct {
	Namespace string
	GiB       float64
	Monthly   float64
	Basis     string
}

// networkQuery sums the bytes each pod sent over the window. The container filter of the CPU queries is left out:
// cAdvisor reports network per pod, with no container label. Loopback and series without a namespace or pod are excluded
// at the source; hostNetwork pods are dropped in Go, because their series is the whole node's traffic.
func (s promSource) networkQuery() string {
	sel := `namespace!="",pod!="",interface!="lo"`
	if s.selector != "" {
		sel += "," + s.selector
	}
	return fmt.Sprintf(`sum by (namespace, pod) (increase(container_network_transmit_bytes_total{%s}[%ds])) unless on (namespace, pod) kube_pod_info{host_network="true"}`, sel, int(s.history.Seconds()))
}

// coverageQuery gives the seconds since Prometheus's oldest sample of "up" inside the window (1h resolution).
func (s promSource) coverageQuery() string {
	sel := ""
	if s.selector != "" {
		sel = "{" + s.selector + "}"
	}
	return fmt.Sprintf(`time() - min(min_over_time(timestamp(up%s)[%ds:1h]))`, sel, int(s.history.Seconds()))
}

// buildNetwork prices outbound traffic per namespace at the AWS first-tier internet rate of the cluster's one region.
// Anything it cannot do (not AWS, several regions, no price, no answer) is one warning line and no section. It is never part of a total.
var genSuffix = regexp.MustCompile(`^(?:[a-z0-9]{6,10}-)?[a-z0-9]{5}$`)

func buildNetwork(ctx context.Context, s promSource, pr pricer, keys map[string]pricing.Key, pods []report.Pod, warn io.Writer) []networkEgress {
	regions, other := map[string]bool{}, false
	for _, k := range keys {
		if k.Provider == "aws" {
			regions[k.Region] = true
		} else {
			other = true
		}
	}
	skip := func(why string) []networkEgress {
		fmt.Fprintf(warn, "phrontis: no network egress estimate: %s\n", why)
		return nil
	}
	switch {
	case len(regions) == 0:
		return skip("it is priced for AWS only")
	case other || len(regions) > 1:
		return skip("it needs one AWS region and no other cloud")
	case pr == nil:
		return skip("it needs cloud prices")
	}
	region := slices.Collect(maps.Keys(regions))[0]
	price, err := pr.DataTransferOut(ctx, region)
	if err != nil {
		return skip(fmt.Sprintf("no data transfer price in %s: %v", region, err))
	}
	series, err := s.query(ctx, s.networkQuery())
	if err != nil {
		return skip(err.Error())
	}
	// Short retention: scale by the hours Prometheus really holds, or the month would come out too low.
	hours := s.history.Hours()
	cov, err := s.query(ctx, s.coverageQuery())
	if err != nil || len(cov) == 0 {
		return skip("cannot tell how much history Prometheus holds")
	}
	if have := cov[0].val / 3600; have < hours-1 { // 1h: the subquery step
		if have < 1 {
			return skip("Prometheus holds less than 1 hour of data")
		}
		fmt.Fprintf(warn, "phrontis: Prometheus holds %.1f days, less than --history; network egress is scaled to that\n", have/24)
		hours = have
	}
	// A host-network pod reports the whole node NIC. Drop the current ones by name, and pods that vanished in the window by
	// their workload's name prefix (a rolled DaemonSet leaves old pod names behind); kube_pod_info in the query covers the
	// rest when kube-state-metrics runs.
	host, live, hostPrefix := map[[2]string]bool{}, map[[2]string]bool{}, map[string][]string{}
	for _, p := range pods {
		live[[2]string{p.Namespace, p.Name}] = true
		if p.HostNetwork {
			host[[2]string{p.Namespace, p.Name}] = true
			if _, name, ok := strings.Cut(p.Workload, "/"); ok && name != "" {
				hostPrefix[p.Namespace] = append(hostPrefix[p.Namespace], name+"-")
			}
		}
	}
	// A live pod is decided exactly; only a vanished one falls back to its name: "<workload>-" plus the generated suffix of a
	// DaemonSet pod (5 chars) or a ReplicaSet pod (hash-5 chars), so "web-api-x" never matches DaemonSet "web".
	isHost := func(ns, pod string) bool {
		if live[[2]string{ns, pod}] {
			return host[[2]string{ns, pod}]
		}
		return slices.ContainsFunc(hostPrefix[ns], func(pre string) bool {
			rest, ok := strings.CutPrefix(pod, pre)
			return ok && genSuffix.MatchString(rest)
		})
	}
	perMonth := report.HoursPerMonth / hours
	byNS := map[string]float64{}
	for _, x := range series {
		if x.ns != "" && x.val > 0 && !isHost(x.ns, x.pod) {
			byNS[x.ns] += x.val
		}
	}
	var out []networkEgress
	for ns, v := range byNS {
		gib := v / (1 << 30)
		out = append(out, networkEgress{ns, gib, gib * perMonth * price, networkBasis})
	}
	slices.SortFunc(out, func(a, b networkEgress) int {
		return cmp.Or(cmp.Compare(b.Monthly, a.Monthly), cmp.Compare(a.Namespace, b.Namespace))
	})
	return out
}

func renderNetwork(out io.Writer, n []networkEgress, window string) {
	if len(n) == 0 {
		return
	}
	fmt.Fprintf(out, "\nNETWORK EGRESS, UPPER BOUND (not in the cluster total)\n%s\n", networkCaption)
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "NAMESPACE\tGiB OVER %s\tCOST/MONTH\n", window)
	for _, x := range n {
		fmt.Fprintf(w, "%s\t%.2f\t%.2f\n", x.Namespace, x.GiB, x.Monthly)
	}
	w.Flush()
}
