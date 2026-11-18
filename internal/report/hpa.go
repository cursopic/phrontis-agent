package report

import (
	"slices"
	"strings"
)

// HPA is a HorizontalPodAutoscaler: the workload it scales (Kind/Name, same form as Rec.Workload) and the resources
// ("cpu", "memory") its metrics target. A utilization target is a share of the request, so those requests steer its replica count.
type HPA struct {
	Namespace, Name, Target string
	Resources               []string
}

// HPARef is the mark on a Rec: the HPA that scales the rec's workload.
type HPARef struct {
	Name      string
	Resources []string // "cpu" and/or "memory"; empty when the HPA scales on other metrics only
}

// MarkHPA sets Rec.HPA on every rec whose workload an HPA scales. With several HPAs on one workload the first by name wins.
func MarkHPA(recs []Rec, hpas []HPA) {
	// One sort and one map: hpas comes from an agent sample, so the cost must not grow as recs × hpas.
	byWorkload := map[[2]string]HPA{}
	for _, h := range slices.SortedFunc(slices.Values(hpas), func(a, b HPA) int { return strings.Compare(a.Name, b.Name) }) {
		k := [2]string{h.Namespace, h.Target}
		if _, ok := byWorkload[k]; !ok {
			byWorkload[k] = h
		}
	}
	for i, r := range recs {
		if h, ok := byWorkload[[2]string{r.Namespace, r.Workload}]; ok {
			recs[i].HPA = &HPARef{h.Name, h.Resources}
		}
	}
}
