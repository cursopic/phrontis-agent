package perms

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type chartRole struct {
	Rules []struct {
		APIGroups     []string `yaml:"apiGroups"`
		Resources     []string `yaml:"resources"`
		ResourceNames []string `yaml:"resourceNames"`
		Verbs         []string `yaml:"verbs"`
	} `yaml:"rules"`
}

func chartRules(t *testing.T) chartRole {
	t.Helper()
	b, err := os.ReadFile("../../deploy/helm/phrontis-agent/templates/rbac.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range strings.Split(string(b), "\n---") {
		if !strings.Contains(doc, "kind: ClusterRole\n") {
			continue
		}
		// Helm placeholders only appear in names and labels; drop those lines so the document is plain YAML.
		doc = regexp.MustCompile(`(?m)^.*\{\{.*$`).ReplaceAllString(doc, "")
		var r chartRole
		if err := yaml.Unmarshal([]byte(doc), &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	t.Fatal("no ClusterRole in chart")
	return chartRole{}
}

func TestMatchesChart(t *testing.T) {
	c := chartRules(t)
	if len(c.Rules) != len(Rules) {
		t.Fatalf("chart has %d rules, perms has %d", len(c.Rules), len(Rules))
	}
	for i, cr := range c.Rules {
		if strings.Join(cr.Verbs, ",") != strings.Join(Verbs, ",") {
			t.Errorf("rule %d verbs %v, want only %v", i, cr.Verbs, Verbs)
		}
		r := Rules[i]
		if len(cr.APIGroups) != 1 || cr.APIGroups[0] != r.Group ||
			strings.Join(cr.Resources, ",") != strings.Join(r.Resources, ",") ||
			strings.Join(cr.ResourceNames, ",") != strings.Join(r.Names, ",") {
			t.Errorf("rule %d: chart %+v differs from %+v", i, cr, r)
		}
	}
}

func TestGeneratedFilesAreCurrent(t *testing.T) {
	var b bytes.Buffer
	Markdown(&b)
	if got, err := os.ReadFile("../../docs/permissions.md"); err != nil || string(got) != b.String() {
		t.Error("docs/permissions.md is stale: run `go run ./cmd/phrontis permissions --md > docs/permissions.md`")
	}
	if got, err := os.ReadFile("../../web/src/permissions.json"); err != nil || string(got) != string(JSON()) {
		t.Error("web/src/permissions.json is stale: run `go run ./cmd/phrontis permissions --json > web/src/permissions.json`")
	}
}
