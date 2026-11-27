package cli

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/cursopic/phrontis/internal/pricing"
	"github.com/cursopic/phrontis/internal/report"
)

// spotPricer is the part of pricing.Client that looks up spot prices; other pricers (and tests) may lack it.
type spotPricer interface {
	SpotHourly(ctx context.Context, k pricing.Key) pricing.SpotPrice
}

// spotReport is the free-plan spot section (spec 011 scenario 1): which workloads can run on spot, and what that saves.
type spotReport struct {
	Priced    bool    // false: no spot prices, so no money is shown; Note says why
	Note      string  // Priced: the price label ("spot (highest zone), price at ..."); otherwise the reason
	Saving    float64 // per month, over Workloads; 0 when not Priced
	Kept      int     // workloads that stay on-demand
	Workloads []report.SpotLine
}

// buildSpot judges every workload and, where the nodes have a spot price, prices the safe ones. nil: nothing to judge.
// shortcut: AWS on-demand nodes only; nodes already on spot, and other clouds, count as saving nothing.
func buildSpot(ctx context.Context, pr pricer, s snapshot, r report.Report) *spotReport {
	verdicts := report.SpotSafety(s.pods, s.pdbs)
	if len(verdicts) == 0 {
		return nil
	}
	out := &spotReport{Note: "cloud prices are off, so there is no AWS spot price (use --cloud-prices)"}
	sp, haveSpot := pr.(spotPricer)
	if pr != nil && !haveSpot {
		out.Note = "this price source has no spot prices"
	}
	discount, seen := map[string]float64{}, map[pricing.Key]float64{} // seen: -1 = no spot price for that type
	for node, k := range s.keys {
		if !haveSpot || k.Provider != "aws" || k.Spot {
			continue
		}
		d, ok := seen[k]
		if !ok {
			d = -1
			od, _, err := pr.Hourly(ctx, k)
			if p := sp.SpotHourly(ctx, k); p.Reason != "" {
				if !out.Priced {
					out.Note = p.Reason
				}
			} else if err == nil && od > 0 {
				d = max(0, 1-p.Hourly/od)
				out.Priced, out.Note = true, p.Label()
			}
			seen[k] = d
		}
		if d >= 0 {
			discount[node] = d
		}
	}
	out.Workloads = report.SpotSavings(verdicts, r.Workloads, s.pods, discount)
	out.Kept = len(verdicts) - len(out.Workloads)
	for _, l := range out.Workloads {
		out.Saving += l.Saving
	}
	return out
}

func renderSpot(out io.Writer, s *spotReport) {
	if s == nil {
		return
	}
	fmt.Fprintf(out, "\nSPOT (workloads that can run on spot; nothing is moved)\n")
	switch {
	case len(s.Workloads) == 0:
		fmt.Fprintf(out, "no workload can run on spot yet; %d stay on-demand\n", s.Kept)
	case s.Priced:
		fmt.Fprintf(out, "%d workload(s) can run on spot, saving about %.2f/month (%s); %d stay on-demand\n", len(s.Workloads), s.Saving, s.Note, s.Kept)
	default:
		fmt.Fprintf(out, "%d workload(s) can run on spot; savings not shown: %s; %d stay on-demand\n", len(s.Workloads), s.Note, s.Kept)
	}
	for _, l := range s.Workloads {
		if s.Priced {
			fmt.Fprintf(out, "  %s/%s  %.2f/month now, saves %.2f\n", l.Namespace, l.Workload, l.Monthly, l.Saving)
		} else {
			fmt.Fprintf(out, "  %s/%s  %.2f/month now\n", l.Namespace, l.Workload, l.Monthly)
		}
	}
}

// flatPriced is nodes with the flat prices on those nodes that have none of their own, as report.Build prices them.
func flatPriced(nodes []report.Node, flat report.Prices) []report.Node {
	out := slices.Clone(nodes)
	for i := range out {
		if out[i].Prices == (report.Prices{}) {
			out[i].Prices = flat
		}
	}
	return out
}

func renderConsolidation(out io.Writer, c *report.Consolidation) {
	if c == nil {
		return
	}
	fmt.Fprintf(out, "\nCONSOLIDATION (nothing is moved)\n")
	if c.Count == 0 {
		fmt.Fprintln(out, "no node can be emptied")
	} else {
		fmt.Fprintf(out, "%d can be emptied, saving about %.2f/month\n", c.Count, c.Saving)
	}
	for _, v := range c.Verdicts {
		fmt.Fprintf(out, "  %s  cpu %.0f%% mem %.0f%%  %.2f/month  ", v.Node, 100*v.CPU, 100*v.Mem, v.Monthly)
		if v.Empty && len(v.To) == 0 {
			fmt.Fprintln(out, "runs no pods to move")
		} else if v.Empty {
			fmt.Fprintf(out, "pods go to %s\n", strings.Join(v.To, ", "))
		} else {
			fmt.Fprintf(out, "stays: %s\n", v.Reason)
		}
	}
	if c.OldAgent {
		fmt.Fprintln(out, "agent too old to read eviction annotations")
	}
	fmt.Fprintln(out, "Estimate. Ignores affinity, taints and node selectors.")
}

func renderAbandoned(out io.Writer, a *report.Abandoned) {
	if a == nil {
		return
	}
	fmt.Fprintf(out, "\nABANDONED WORKLOADS (CPU stayed near zero over each workload's own samples, at least 1 day; nothing is changed)\n")
	fmt.Fprintf(out, "%d workloads, %.2f/month\n", len(a.Workloads), a.Monthly)
	if a.Short {
		fmt.Fprintf(out, "some history is shorter than %d days: judged on what exists\n", report.AbandonedDays)
	}
	for _, l := range a.Workloads {
		fmt.Fprintf(out, "  %s  %s  %d pod(s)  peak %dm  %.1f days  %.2f/month\n", l.Namespace, l.Workload, l.Pods, l.PeakCPU, l.SpanHours/24, l.Monthly)
	}
	fmt.Fprintln(out, "CPU only: the agent sees no traffic. Check before removing.")
}
