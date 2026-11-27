package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/cursopic/phrontis/internal/report"
)

func renderUnevictable(out io.Writer, u *report.Unevictable) {
	if u == nil {
		return
	}
	fmt.Fprintf(out, "\nUNEVICTABLE PODS (block consolidation)\n%d pods pin %d nodes, %.2f/month. Estimate. Local storage blocks only the Cluster Autoscaler, not Karpenter.\n", len(u.Pods), u.Nodes, u.Monthly)
	for _, p := range u.Pods {
		fmt.Fprintf(out, "  %s/%s  %s  %.2f/month  %s\n", p.Namespace, p.Pod, p.Node, p.Monthly, strings.Join(p.Reasons, ", "))
	}
}
