package manifest

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/cursopic/phrontis/internal/report"
)

// The same file is read by web/test/report.test.ts, so the two patch() implementations cannot drift.
func TestPatchGolden(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/patch-golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name  string
		Rec   report.Rec
		Patch *string
		File  string
	}
	if err := json.Unmarshal(raw, &cases); err != nil || len(cases) < 8 {
		t.Fatalf("golden: %d cases, %v", len(cases), err)
	}
	for _, c := range cases {
		got, ok := Patch(c.Rec)
		if ok != (c.Patch != nil) || (ok && got != *c.Patch) {
			t.Errorf("%s: got %q (ok %v), want %v", c.Name, got, ok, c.Patch)
		}
		if f := PatchFile(c.Rec); f != c.File {
			t.Errorf("%s: file %q, want %q", c.Name, f, c.File)
		}
	}
}
