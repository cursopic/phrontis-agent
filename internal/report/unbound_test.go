package report

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

func TestUnboundVolumes(t *testing.T) {
	const g = 1 << 30
	day1 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	s := BuildStorage([]Volume{
		{"small", "", "", 1 * g, "gp3", "fast", day1},
		{"big", "", "", 20 * g, "gp3", "fast", day1},
		{"odd", "", "", 50 * g, "mystery", "", time.Time{}},
		{"used", "shop", "data", 9 * g, "gp3", "fast", day1},
	}, map[string]float64{"gp3": 0.1})
	if len(s.Unbound) != 3 || s.Unbound[0].Name != "big" || s.Unbound[1].Name != "small" || s.Unbound[2].Name != "odd" {
		t.Fatalf("unbound = %+v", s.Unbound)
	}
	if b := s.Unbound[0]; b.GiB != 20 || b.Class != "fast" || !b.Created.Equal(day1) || math.Abs(b.Monthly-2) > 1e-9 {
		t.Errorf("big = %+v", b)
	}
	if s.Unbound[2].Monthly != 0 || s.Unclaimed.Unpriced != 1 {
		t.Errorf("unpriced volume must cost 0 and be counted: %+v %+v", s.Unbound[2], s.Unclaimed)
	}
}

// An export or agent sample from before S86 has no Unbound list, Class or Created: it still parses and the totals stay.
func TestStorageOldPayload(t *testing.T) {
	var s Storage
	if err := json.Unmarshal([]byte(`{"Namespaces":null,"Unclaimed":{"Volumes":2,"GiB":21,"Monthly":2},"Total":{"Volumes":2}}`), &s); err != nil {
		t.Fatal(err)
	}
	if s.Unclaimed.Volumes != 2 || s.Unbound != nil {
		t.Errorf("old storage = %+v", s)
	}
	var v Volume
	if err := json.Unmarshal([]byte(`{"Name":"pv","Namespace":"","Claim":"","Bytes":1,"Type":"gp3"}`), &v); err != nil || v.Class != "" || !v.Created.IsZero() {
		t.Errorf("old volume = %+v, %v", v, err)
	}
}
