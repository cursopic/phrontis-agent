package report

import "testing"

func TestCompare(t *testing.T) {
	tests := []struct {
		name          string
		before, after Totals
		total         Delta
	}{
		{"saving", Totals{1000, 300, 50, 20}, Totals{700, 100, 50, 20}, Delta{"Total", 1070, 770, -300, -300.0 / 1070 * 100}},
		{"increase", Totals{100, 0, 10, 0}, Totals{150, 0, 10, 5}, Delta{"Total", 110, 165, 55, 50}},
		{"empty before", Totals{}, Totals{Compute: 10}, Delta{"Total", 0, 10, 10, 0}},
	}
	for _, tc := range tests {
		got := Compare(tc.before, tc.after)
		if len(got) != 5 {
			t.Fatalf("%s: %d lines", tc.name, len(got))
		}
		if g := got[4]; g != tc.total {
			t.Errorf("%s: total %+v, want %+v", tc.name, g, tc.total)
		}
	}
	if c := Compare(Totals{Compute: 1000, Idle: 300}, Totals{Compute: 700, Idle: 100})[1]; c.Diff != -200 {
		t.Errorf("idle diff %v", c.Diff)
	}
}
