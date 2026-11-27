package pricing

import (
	"math"
	"strings"
)

// MaxDiscount is the largest percent an org may take off list prices (S23).
const MaxDiscount = 90

// Override takes Percent (an integer, 1..MaxDiscount) off the compute price of one cloud ("aws", "azure", "gcp") or one
// instance family (see Family). Default off: no override, no change.
type Override struct {
	Scope   string `json:"scope"` // "cloud" or "family"
	Name    string `json:"name"`  // lower case
	Percent int    `json:"percent"`
}

// Family is the instance family of a node type, lower case: "m5.large" is "m5", "n2-standard-4" is "n2",
// "Standard_D4s_v5" is "d4s_v5".
// shortcut: Azure families keep their size number ("d4s_v5" is one family per size); group them when users ask.
func Family(typ string) string {
	t := strings.TrimPrefix(strings.ToLower(typ), "standard_")
	if i := strings.IndexAny(t, ".-"); i >= 0 {
		t = t[:i]
	}
	return t
}

// Percent is the discount for k: a family override wins over a cloud override; 0 when none applies.
func Percent(ov []Override, k Key) int {
	p := 0
	for _, o := range ov {
		switch {
		case o.Scope == "family" && o.Name == Family(k.Type):
			return o.Percent
		case o.Scope == "cloud" && o.Name == k.Provider:
			p = o.Percent
		}
	}
	return p
}

// Adjust takes pct percent off a price in whole micro-units, rounding half up once. It is an estimate, not an invoice.
func Adjust(price float64, pct int) float64 {
	if pct <= 0 || pct > MaxDiscount {
		return price
	}
	micro := int64(math.Round(price * 1e6))
	return float64((micro*int64(100-pct)+50)/100) / 1e6
}
