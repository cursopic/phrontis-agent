package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/cursopic/phrontis/internal/report"
)

// snap is what compare needs from a saved --json report.
type snap struct {
	report.Totals
	context, sources          string
	unpricedVols, unpricedLBs int
}

// readSnap reads a saved `phrontis --json` report; JSON that is not one is an error, not a table of zeros.
func readSnap(path string) (snap, error) {
	var e export
	var probe struct{ Monthly *float64 }
	b, err := os.ReadFile(path)
	if err == nil {
		if err = json.Unmarshal(b, &e); err == nil {
			err = json.Unmarshal(b, &probe)
		}
	}
	if err != nil {
		return snap{}, fmt.Errorf("%s: %w", path, err)
	}
	if probe.Monthly == nil || (e.Context == "" && e.At.IsZero() && e.Samples == 0) {
		return snap{}, fmt.Errorf("%s is not a phrontis --json report", path)
	}
	set := map[string]bool{}
	for _, n := range e.NodePrices {
		set[n.Source] = true
	}
	src := slices.Sorted(maps.Keys(set))
	return snap{
		Totals:       report.Totals{Compute: e.Monthly, Idle: e.IdleMonthly, Storage: e.Storage.Total.Monthly, LoadBalancers: e.LoadBalancers.Total.Monthly},
		context:      e.Context,
		sources:      strings.Join(src, ","),
		unpricedVols: e.Storage.Total.Unpriced,
		unpricedLBs:  e.LoadBalancers.Total.Unpriced,
	}, nil
}

// compare prints the before/after cost table of two saved --json reports.
func compare(out io.Writer, beforePath, afterPath string) error {
	b, err := readSnap(beforePath)
	if err != nil {
		return err
	}
	a, err := readSnap(afterPath)
	if err != nil {
		return err
	}
	if b.context != a.context || b.sources != a.sources {
		fmt.Fprintln(out, "Warning: before and after come from different clusters/price sources; compare with care")
	}
	fmt.Fprintln(out, "Cost before and after the switch (USD per month, estimate: nodes priced at flat defaults are rough)")
	fmt.Fprintf(out, "%-16s %10s %10s %10s %7s\n", "", "before", "after", "change", "%")
	for _, d := range report.Compare(b.Totals, a.Totals) {
		fmt.Fprintf(out, "%-16s %10.2f %10.2f %+10.2f %+6.1f%%\n", d.Name, d.Before, d.After, d.Diff, d.Pct)
	}
	if v := max(b.unpricedVols, a.unpricedVols); v > 0 {
		fmt.Fprintf(out, "Note: up to %d volume(s) without a price, not in the totals\n", v)
	}
	if l := max(b.unpricedLBs, a.unpricedLBs); l > 0 {
		fmt.Fprintf(out, "Note: up to %d load balancer(s) without a price, not in the totals\n", l)
	}
	return nil
}
