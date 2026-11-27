package cli

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/cursopic/phrontis/internal/report"
)

func TestPosture(t *testing.T) {
	yes, no := true, false
	var c report.Container
	// pod runAsNonRoot true, container false: the container wins, so it runs as root
	posture(&c, &corev1.PodSecurityContext{RunAsNonRoot: &yes}, corev1.Container{Image: "a:1", SecurityContext: &corev1.SecurityContext{RunAsNonRoot: &no}})
	if c.RunAsNonRoot || c.Image != "a:1" {
		t.Errorf("container false must beat pod true: %+v", c)
	}
	c = report.Container{}
	posture(&c, &corev1.PodSecurityContext{RunAsNonRoot: &yes}, corev1.Container{SecurityContext: &corev1.SecurityContext{
		Privileged: &yes, AllowPrivilegeEscalation: &no, Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"SYS_ADMIN"}}}})
	if !c.RunAsNonRoot || !c.Privileged || c.AllowPrivEsc || len(c.AddedCaps) != 1 {
		t.Errorf("pod value, privileged, escalation off, one cap: %+v", c)
	}
	c = report.Container{}
	posture(&c, nil, corev1.Container{})
	if c.RunAsNonRoot || !c.AllowPrivEsc {
		t.Errorf("no security context: root, escalation allowed: %+v", c)
	}
}

func TestRenderSecurity(t *testing.T) {
	var b strings.Builder
	renderSecurity(&b, report.SecurityReport{Unread: 1, Findings: []report.Finding{{Namespace: "kube-system", Workload: "DaemonSet/x", Container: "c", Check: "privileged", Level: "high", System: true}}})
	for _, w := range []string{"Not a vulnerability scan", "Update the agent to see security checks.", "privileged", "DaemonSet/x (system)"} {
		if !strings.Contains(b.String(), w) {
			t.Errorf("missing %q in %q", w, b.String())
		}
	}
}
