package report

import (
	"slices"
	"time"
)

// NodeSpan is one node running from From to To at Hourly USD per hour. Hourly is the node's price from the report
// (cli.NodePrice.Hourly), so the cost here matches the report's node prices.
type NodeSpan struct {
	From, To time.Time
	Hourly   float64
}

// DayCost is the node cost of one UTC day.
type DayCost struct {
	Day  time.Time // 00:00 UTC
	Cost float64
}

// NodeCostBeforeAfter splits spans at enabled and returns the cost per UTC day before it and from it on.
// The split day appears in both lists with its part before and after the exact enable time.
// Days no span touches are absent, not zero.
func NodeCostBeforeAfter(spans []NodeSpan, enabled time.Time) (before, after []DayCost) {
	enabled = enabled.UTC()
	return dayCosts(spans, func(from, to time.Time) (time.Time, time.Time) { return from, minTime(to, enabled) }),
		dayCosts(spans, func(from, to time.Time) (time.Time, time.Time) { return maxTime(from, enabled), to })
}

func dayCosts(spans []NodeSpan, clip func(from, to time.Time) (time.Time, time.Time)) []DayCost {
	cost := map[time.Time]float64{}
	var days []time.Time
	for _, s := range spans {
		from, to := clip(s.From.UTC(), s.To.UTC())
		for day := from.Truncate(24 * time.Hour); day.Before(to); day = day.AddDate(0, 0, 1) {
			if h := minTime(to, day.AddDate(0, 0, 1)).Sub(maxTime(from, day)).Hours(); h > 0 {
				if _, ok := cost[day]; !ok {
					days = append(days, day)
				}
				cost[day] += h * s.Hourly
			}
		}
	}
	slices.SortFunc(days, time.Time.Compare)
	var out []DayCost
	for _, d := range days {
		out = append(out, DayCost{d, cost[d]})
	}
	return out
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
