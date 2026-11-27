// Package perms lists in plain words what the agent's ClusterRole lets it read.
// The list is checked against deploy/helm/phrontis-agent/templates/rbac.yaml by a test, so it cannot drift from the chart.
package perms

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Verbs are the only verbs the agent ever gets.
var Verbs = []string{"get", "list", "watch"}

// Rule is one ClusterRole rule: what the agent can read and why it needs it.
type Rule struct {
	Group     string   `json:"group"` // "" is the core API group
	Resources []string `json:"resources"`
	Names     []string `json:"names,omitempty"` // when set, only these objects
	Reason    string   `json:"reason"`
}

// Rules mirrors the chart's ClusterRole rule by rule.
var Rules = []Rule{
	{"", []string{"nodes", "pods", "persistentvolumes", "services", "limitranges"}, nil,
		"Node size and price, what each pod requests, volumes and load balancers, and the defaults a namespace sets."},
	{"", []string{"namespaces"}, []string{"kube-system"},
		"Only kube-system. Its UID names the cluster, so the API key can be checked against it."},
	{"apps", []string{"replicasets", "statefulsets", "daemonsets"}, nil,
		"Find the workload that owns each pod, so cost is shown per workload."},
	{"policy", []string{"poddisruptionbudgets"}, nil,
		"Flag pods that cannot be evicted safely."},
	{"autoscaling", []string{"horizontalpodautoscalers"}, nil,
		"Mark recommendations for workloads that an autoscaler already scales."},
	{"batch", []string{"jobs"}, nil,
		"Find the workload that owns each job pod."},
	{"storage.k8s.io", []string{"storageclasses"}, nil,
		"Price volumes by storage class."},
	{"metrics.k8s.io", []string{"pods"}, nil,
		"Measured CPU and memory use, the basis of every recommendation."},
}

// GroupName is the API group as shown to a person.
func (r Rule) GroupName() string {
	if r.Group == "" {
		return "core"
	}
	return r.Group
}

func (r Rule) resources(sep string) string {
	s := strings.Join(r.Resources, sep)
	if len(r.Names) > 0 {
		s += " (only " + strings.Join(r.Names, ", ") + ")"
	}
	return s
}

// Markdown renders docs/permissions.md; a test keeps the file equal to this output.
func Markdown(w io.Writer) {
	fmt.Fprint(w, "# What the agent can access\n\n"+
		"The agent is read-only. Its ClusterRole gives it "+strings.Join(Verbs, ", ")+" and nothing else.\n"+
		"It cannot create, change or delete anything, and it cannot read Secrets, ConfigMaps or pod logs.\n\n"+
		"This page is generated from the Helm chart by `phrontis permissions --md`. A test fails if they differ.\n\n"+
		"| API group | Resources | Why |\n|---|---|---|\n")
	for _, r := range Rules {
		fmt.Fprintf(w, "| %s | %s | %s |\n", r.GroupName(), r.resources(", "), r.Reason)
	}
}

// Text renders the list for the terminal.
func Text(w io.Writer) {
	fmt.Fprintf(w, "The agent is read-only: %s only. It cannot read Secrets, ConfigMaps or pod logs.\n\n", strings.Join(Verbs, ", "))
	for _, r := range Rules {
		fmt.Fprintf(w, "%s [%s]\n  %s\n", r.resources(", "), r.GroupName(), r.Reason)
	}
}

// JSON renders web/src/permissions.json, which the dashboard page imports.
func JSON() []byte {
	b, _ := json.MarshalIndent(Rules, "", "  ")
	return append(b, '\n')
}
