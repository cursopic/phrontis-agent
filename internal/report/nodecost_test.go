package report

import (
	"reflect"
	"testing"
	"time"
)

func TestNodeCostBeforeAfter(t *testing.T) {
	at := func(d, h int) time.Time { return time.Date(2026, 11, d, h, 0, 0, 0, time.UTC) }
	day := func(d int, c float64) DayCost { return DayCost{at(d, 0), c} }
	// Node A ($1/h) runs 10th 00:00 to 13th 00:00; node B ($2/h) joins 11th 12:00 (count 1 -> 2) and leaves 12th 06:00.
	// Nothing runs on the 14th or later: those days must be absent.
	spans := []NodeSpan{{at(10, 0), at(13, 0), 1}, {at(11, 12), at(12, 6), 2}}
	cases := []struct {
		name          string
		enabled       time.Time
		before, after []DayCost
	}{
		{"split mid-day", at(11, 18),
			[]DayCost{day(10, 24), day(11, 18+6*2)},
			[]DayCost{day(11, 6+6*2), day(12, 24+12)}},
		{"enabled at midnight", at(12, 0),
			[]DayCost{day(10, 24), day(11, 24+24)},
			[]DayCost{day(12, 24+12)}},
		{"enabled after all data", at(20, 0),
			[]DayCost{day(10, 24), day(11, 48), day(12, 36)},
			nil},
	}
	for _, c := range cases {
		b, a := NodeCostBeforeAfter(spans, c.enabled)
		if !reflect.DeepEqual(b, c.before) || !reflect.DeepEqual(a, c.after) {
			t.Errorf("%s: before %v after %v, want %v %v", c.name, b, a, c.before, c.after)
		}
	}
}
