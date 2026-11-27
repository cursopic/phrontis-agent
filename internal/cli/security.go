package cli

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/cursopic/phrontis/internal/report"
)

// posture fills the security fields of one container from its spec (spec 023). RunAsNonRoot is the effective value:
// the container's own setting, else the pod's.
func posture(c *report.Container, ps *corev1.PodSecurityContext, spec corev1.Container) {
	c.Image = spec.Image
	sc := spec.SecurityContext
	c.RunAsNonRoot = ps != nil && ps.RunAsNonRoot != nil && *ps.RunAsNonRoot
	if sc == nil {
		c.AllowPrivEsc = true
		return
	}
	c.Privileged = sc.Privileged != nil && *sc.Privileged
	c.AllowPrivEsc = sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation
	if sc.RunAsNonRoot != nil {
		c.RunAsNonRoot = *sc.RunAsNonRoot
	}
	if sc.Capabilities != nil {
		for _, x := range sc.Capabilities.Add {
			c.AddedCaps = append(c.AddedCaps, string(x))
		}
	}
}

// securityRun is `phrontis --security`: the same table the dashboard shows, from the live cluster.
func securityRun(out io.Writer, kctx string) error {
	cs, mc, _, err := clients(kctx)
	if err != nil {
		return err
	}
	_, pods, _, _, _, _, _, err := collect(context.Background(), cs, mc, time.Duration(0))
	if err != nil {
		return err
	}
	renderSecurity(out, report.BuildSecurity(pods))
	return nil
}

func renderSecurity(out io.Writer, s report.SecurityReport) {
	fmt.Fprintln(out, "SECURITY: configuration checks. Not a vulnerability scan.")
	if s.Unread > 0 {
		fmt.Fprintln(out, "Update the agent to see security checks.")
	}
	if len(s.Findings) == 0 {
		fmt.Fprintln(out, "No findings.")
		return
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "LEVEL\tCHECK\tNAMESPACE\tWORKLOAD\tCONTAINER\t")
	for _, f := range s.Findings {
		wl := f.Workload
		if f.System {
			wl += " (system)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t\n", f.Level, f.Check, f.Namespace, wl, f.Container)
	}
	w.Flush()
}
