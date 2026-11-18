package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompareCLI(t *testing.T) {
	d := t.TempDir()
	w := func(n, s string) string { p := filepath.Join(d, n); os.WriteFile(p, []byte(s), 0o644); return p }
	b := w("b.json", `{"Context":"c","Samples":1,"Monthly":1000,"IdleMonthly":300,"Storage":{"Total":{"Monthly":50}},"LoadBalancers":{"Total":{"Monthly":20}}}`)
	a := w("a.json", `{"Context":"d","Samples":1,"Monthly":700,"IdleMonthly":100,"Storage":{"Total":{"Monthly":50}},"LoadBalancers":{"Total":{"Monthly":20}}}`)
	var out bytes.Buffer
	if err := compare(&out, b, a); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Total") || !strings.Contains(out.String(), "-300.00") {
		t.Errorf("output:\n%s", out.String())
	}
	if compare(&out, b, filepath.Join(d, "none.json")) == nil {
		t.Error("missing file should fail")
	}
	if !strings.Contains(out.String(), "different clusters") {
		t.Errorf("no context warning:\n%s", out.String())
	}
	for _, bad := range []string{`{}`, `{"foo":1}`, `{"Monthly":5}`} {
		if err := compare(&out, w("x.json", bad), a); err == nil || !strings.Contains(err.Error(), "not a phrontis --json report") {
			t.Errorf("%s: err %v", bad, err)
		}
	}
	u := w("u.json", `{"Context":"c","Samples":1,"Monthly":1,"LoadBalancers":{"Total":{"Unpriced":1}}}`)
	out.Reset()
	if err := compare(&out, u, u); err != nil || !strings.Contains(out.String(), "1 load balancer(s) without a price") {
		t.Errorf("unpriced note: %v\n%s", err, out.String())
	}
}
