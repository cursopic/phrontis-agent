// Package manifest has a recommendation's requests as quantities and as a patch. The values are the ones web/src/report.ts
// patch() and prSteps() show, so the PR and the copied patch never differ.
package manifest

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/cursopic/phrontis/internal/report"
)

// Values are the quantities to write. LimitCPU/LimitMemory: the limit rises to the new request, which would exceed it.
type Values struct {
	CPU, Memory           string
	LimitCPU, LimitMemory bool
}

// podSpec is where each controller keeps its pod spec, as podSpec in web/src/report.ts.
var podSpec = map[string][]string{
	"Deployment":  {"spec", "template", "spec"},
	"StatefulSet": {"spec", "template", "spec"},
	"DaemonSet":   {"spec", "template", "spec"},
	"CronJob":     {"spec", "jobTemplate", "spec", "template", "spec"},
}

// KindName splits "Kind/name"; ok is false for a kind that gets no patch (Job, Pod, ReplicaSet, custom).
func KindName(workload string) (kind, name string, ok bool) {
	kind, name, _ = strings.Cut(workload, "/")
	_, ok = podSpec[kind]
	return kind, name, ok && name != ""
}

// ValuesFor is patch() in Go: exact Mi rounded up, and a limit raised only when the new request exceeds it. ok is false for
// a kind that gets no patch and for an injected sidecar, which is in no manifest of its workload.
func ValuesFor(x report.Rec) (Values, bool) {
	if _, _, ok := KindName(x.Workload); !ok || x.Injected {
		return Values{}, false
	}
	return Values{
		CPU:         fmt.Sprintf("%dm", x.Recommended.MilliCPU),
		Memory:      fmt.Sprintf("%dMi", (x.Recommended.MemBytes+(1<<20)-1)>>20),
		LimitCPU:    x.Limit.MilliCPU > 0 && x.Recommended.MilliCPU > x.Limit.MilliCPU,
		LimitMemory: x.Limit.MemBytes > 0 && x.Recommended.MemBytes > x.Limit.MemBytes,
	}, true
}

var repoRE = regexp.MustCompile(`^[A-Za-z0-9-]{1,39}/[A-Za-z0-9._-]{1,100}$`)

// CheckPRInput returns the field and the message of the first invalid input, or "" when both are fine.
func CheckPRInput(repo, path string) (field, msg string) {
	if !repoRE.MatchString(repo) || strings.HasSuffix(repo, ".") {
		return "repo", "Use owner/name, for example acme/infra."
	}
	p := strings.ToLower(path)
	switch {
	case path == "" || len(path) > 1024 || strings.HasPrefix(path, "/") || strings.Contains(path, `\`) ||
		strings.ContainsAny(path, "\x00?#"):
		return "path", "Use a path relative to the repo root, for example k8s/api.yaml."
	case path == ".." || strings.HasPrefix(path, "../") || strings.HasSuffix(path, "/..") || strings.Contains(path, "/../"):
		return "path", "The path cannot contain \"..\"."
	case !strings.HasSuffix(p, ".yaml") && !strings.HasSuffix(p, ".yml"):
		return "path", "The path must end in .yaml or .yml."
	}
	return "", ""
}
