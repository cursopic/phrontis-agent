package pricing

import "testing"

func TestAdjust(t *testing.T) {
	for _, c := range []struct {
		price float64
		pct   int
		want  float64
	}{
		{0.096, 0, 0.096}, {0.096, 10, 0.0864}, {0.096, 90, 0.0096}, {0.096, 91, 0.096}, {0.096, -5, 0.096},
		{0.000003, 50, 0.000002}, // 3 micro * 50% = 1.5 micro: rounded once, half up, to 2 micro
	} {
		if got := Adjust(c.price, c.pct); got != c.want {
			t.Errorf("Adjust(%v, %d) = %v, want %v", c.price, c.pct, got, c.want)
		}
	}
}

func TestFamilyAndPercent(t *testing.T) {
	for in, want := range map[string]string{"m5.large": "m5", "N2-standard-4": "n2", "Standard_D4s_v5": "d4s_v5", "": ""} {
		if got := Family(in); got != want {
			t.Errorf("Family(%q) = %q, want %q", in, got, want)
		}
	}
	ov := []Override{{"cloud", "aws", 10}, {"family", "m5", 25}}
	for _, c := range []struct {
		k    Key
		want int
	}{
		{Key{Provider: "aws", Type: "m5.xlarge"}, 25}, // family beats cloud
		{Key{Provider: "aws", Type: "c5.large"}, 10},
		{Key{Provider: "gcp", Type: "n2-standard-4"}, 0},
	} {
		if got := Percent(ov, c.k); got != c.want {
			t.Errorf("Percent(%+v) = %d, want %d", c.k, got, c.want)
		}
	}
	if Percent(nil, Key{Provider: "aws", Type: "m5.large"}) != 0 {
		t.Error("default must be off")
	}
}
