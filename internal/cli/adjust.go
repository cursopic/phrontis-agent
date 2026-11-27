package cli

import (
	"context"
	"fmt"
	"slices"

	"github.com/cursopic/phrontis/internal/pricing"
)

// adjusted is a Pricer that takes the org's discounts (S23) off compute prices: node hourly prices and the instance
// types the Karpenter simulation offers. Storage and load balancer prices stay at list. It records which discounts hit.
type adjusted struct {
	pricer
	ov   []pricing.Override
	used map[string]bool
}

// NewAdjustedPricer wraps pr; with no overrides, or no pricer, it returns pr unchanged (default off).
func NewAdjustedPricer(pr Pricer, ov []pricing.Override) Pricer {
	if pr == nil || len(ov) == 0 {
		return pr
	}
	return &adjusted{pricer: pr, ov: ov, used: map[string]bool{}}
}

func (a *adjusted) take(k pricing.Key, price float64) float64 {
	pct := pricing.Percent(a.ov, k)
	if pct > 0 {
		a.used[fmt.Sprintf("%s -%d%%", a.name(k), pct)] = true
	}
	return pricing.Adjust(price, pct)
}

// name is the override that won for k, as the user typed it.
func (a *adjusted) name(k pricing.Key) string {
	for _, o := range a.ov {
		if o.Scope == "family" && o.Name == pricing.Family(k.Type) {
			return o.Name
		}
	}
	return k.Provider
}

func (a *adjusted) Hourly(ctx context.Context, k pricing.Key) (float64, string, error) {
	h, src, err := a.pricer.Hourly(ctx, k)
	if err != nil {
		return h, src, err
	}
	return a.take(k, h), src, nil
}

func (a *adjusted) EC2Types(ctx context.Context, region string) (map[string]pricing.Type, error) {
	types, err := a.pricer.EC2Types(ctx, region)
	if err != nil {
		return nil, err
	}
	out := make(map[string]pricing.Type, len(types))
	for n, t := range types {
		t.Hourly = a.take(pricing.Key{Provider: "aws", Type: n}, t.Hourly)
		out[n] = t
	}
	return out, nil
}

// SpotHourly takes the same discount off the spot price, so the spot saving ratio 1 - spot/on-demand stays unchanged.
func (a *adjusted) SpotHourly(ctx context.Context, k pricing.Key) pricing.SpotPrice {
	sp, ok := a.pricer.(spotPricer)
	if !ok {
		return pricing.SpotPrice{Reason: "this price source has no spot prices"}
	}
	p := sp.SpotHourly(ctx, k)
	if p.Reason == "" {
		p.Hourly = pricing.Adjust(p.Hourly, pricing.Percent(a.ov, k))
	}
	return p
}

// labels lists the discounts that changed a price, sorted, such as ["aws -10%", "m5 -25%"].
func (a *adjusted) labels() []string {
	l := make([]string, 0, len(a.used))
	for s := range a.used {
		l = append(l, s)
	}
	slices.Sort(l)
	return l
}
