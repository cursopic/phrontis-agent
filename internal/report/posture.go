package report

import (
	"cmp"
	"slices"
	"strings"
)

// Finding is one configuration check that failed (spec 023). Container is empty for pod-level checks.
type Finding struct {
	Namespace, Workload, Container string
	Check, Level                   string // Level: high, medium, low
	System                         bool   // kube-system or a static pod: shown, but marked
}

// SecurityReport is the findings plus how many containers the agent sent without posture fields (an older agent).
type SecurityReport struct {
	Findings []Finding
	Unread   int
}

// baselineCaps are the capabilities the Pod Security Standards Baseline profile allows to be added.
var baselineCaps = []string{"AUDIT_WRITE", "CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "KILL", "MKNOD", "NET_BIND_SERVICE", "SETFCAP", "SETGID", "SETPCAP", "SETUID", "SYS_CHROOT"}

var levelRank = map[string]int{"high": 0, "medium": 1, "low": 2}

// latestTag: no tag and no digest, or the tag latest.
func latestTag(image string) bool {
	if strings.Contains(image, "@") {
		return false
	}
	_, tag, ok := strings.Cut(image[strings.LastIndex(image, "/")+1:], ":")
	return !ok || tag == "latest"
}

// BuildSecurity runs the nine checks over the pods. A container with no Image came from an agent that did not read
// posture fields: only its limits and requests are checked, so an old sample never gives a false finding.
func BuildSecurity(pods []Pod) SecurityReport {
	var s SecurityReport
	seen := map[Finding]bool{}
	for _, p := range pods {
		wl := p.Workload
		if wl == "" {
			wl = "Pod/" + p.Name
		}
		add := func(c, check, level string) {
			f := Finding{p.Namespace, wl, c, check, level, p.Static || p.Namespace == "kube-system"}
			if !seen[f] {
				seen[f] = true
				s.Findings = append(s.Findings, f)
			}
		}
		if p.HostNetwork || p.HostPID || p.HostIPC {
			add("", "host-namespaces", "high")
		}
		if p.HostPath {
			add("", "host-path", "high")
		}
		for _, c := range p.Containers {
			if c.Image == "" {
				s.Unread++
			} else {
				if c.Privileged {
					add(c.Name, "privileged", "high")
				}
				if slices.ContainsFunc(c.AddedCaps, func(x string) bool { return !slices.Contains(baselineCaps, strings.ToUpper(x)) }) {
					add(c.Name, "extra-capabilities", "medium")
				}
				if c.AllowPrivEsc {
					add(c.Name, "privilege-escalation", "medium")
				}
				if !c.RunAsNonRoot {
					add(c.Name, "run-as-root", "medium")
				}
				if latestTag(c.Image) {
					add(c.Name, "latest-tag", "low")
				}
			}
			if c.Limit.MemBytes == 0 {
				add(c.Name, "no-limits", "low")
			}
			if c.Request.MilliCPU == 0 || c.Request.MemBytes == 0 {
				add(c.Name, "no-requests", "low")
			}
		}
	}
	slices.SortFunc(s.Findings, func(a, b Finding) int {
		return cmp.Or(cmp.Compare(levelRank[a.Level], levelRank[b.Level]), cmp.Compare(a.Check, b.Check), cmp.Compare(a.Namespace, b.Namespace),
			cmp.Compare(a.Workload, b.Workload), cmp.Compare(a.Container, b.Container))
	})
	return s
}
