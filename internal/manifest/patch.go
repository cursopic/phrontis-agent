package manifest

import (
	"fmt"
	"strings"

	"github.com/cursopic/phrontis/internal/report"
)

// note is the last sentence of the patch's second comment line, as podSpec[kind].note in web/src/report.ts.
func note(kind string) string {
	if kind == "CronJob" {
		return "Takes effect from the next run."
	}
	return "Applying it restarts the pods."
}

// PatchFile is the file name the patch downloads as: patchFile() in web/src/report.ts.
func PatchFile(x report.Rec) string {
	_, name, _ := strings.Cut(x.Workload, "/")
	return name + ".patch.yaml"
}

// Patch is patch() in web/src/report.ts: a strategic-merge patch that sets the recommended requests on one
// container, byte for byte. ok is false when the kind gets no patch or the
// container is an injected sidecar. Both are checked against testdata/patch-golden.json.
func Patch(x report.Rec) (string, bool) {
	v, ok := ValuesFor(x)
	if !ok {
		return "", false
	}
	kind, name, _ := KindName(x.Workload)
	path := podSpec[kind]
	pad := strings.Repeat("  ", len(path))
	cpuReq, memReq := pad+"      cpu: "+v.CPU, pad+"      memory: "+v.Memory
	var raise []string
	if v.LimitCPU {
		raise = append(raise, cpuReq)
	}
	if v.LimitMemory {
		raise = append(raise, memReq)
	}
	second := "# Sets requests only; limits stay as they are. " + note(kind)
	if len(raise) > 0 {
		second = "# Sets requests, and raises a limit to its new request where it would be exceeded. " + note(kind)
	}
	lines := []string{
		fmt.Sprintf("# kubectl -n %s patch %s %s --patch-file %s.patch.yaml", x.Namespace, strings.ToLower(kind), name, name),
		second,
	}
	for i, k := range path {
		lines = append(lines, strings.Repeat("  ", i)+k+":")
	}
	list := "containers:"
	if x.Sidecar {
		list = "initContainers:"
	}
	lines = append(lines, pad+list, pad+"- name: "+x.Container, pad+"  resources:", pad+"    requests:", cpuReq, memReq)
	if len(raise) > 0 {
		lines = append(lines, pad+"    limits:")
		lines = append(lines, raise...)
	}
	return strings.Join(append(lines, ""), "\n"), true
}
