package report

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// HPA is a HorizontalPodAutoscaler: the workload it scales (Kind/Name, same form as Rec.Workload) and the resources
// ("cpu", "memory") its metrics target. A utilization target is a share of the request, so those requests steer its replica count.
type HPA struct {
	Namespace, Name, Target string
	Resources               []string
	// Utilization targets in percent (0: none, or an averageValue target) and the replica counts; absent from older agents.
	CPUTarget, MemTarget, MinReplicas, Replicas, MaxReplicas int `json:",omitempty"`
	// The container a ContainerResource target names; empty for a Resource (pod-wide) target.
	CPUContainer, MemContainer string `json:",omitempty"`
}

// HPARef is the mark on a Rec: the HPA that scales the rec's workload.
type HPARef struct {
	Name      string
	Resources []string       // "cpu" and/or "memory"; empty when the HPA scales on other metrics only
	Advice    []TargetAdvice `json:",omitempty"` // raising the target saves more than shrinking the request (spec 020)
}

// TargetAdvice says to raise one utilization target of the HPA to HPATarget; the request stays as it is.
type TargetAdvice struct {
	Resource string  // "cpu" or "memory"
	From     int     // current target, percent
	Saving   float64 // estimated monthly saving
}

// Text is the advice as one line; the CLI and the web print the same words.
func (a TargetAdvice) Text(hpa string) string {
	res := "CPU"
	if a.Resource == "memory" {
		res = "memory"
	}
	return fmt.Sprintf("Raise HPA %s %s target %d%% → %d%%: saves ~$%.0f/mo", hpa, res, a.From, HPATarget, a.Saving)
}

// HPATarget is the utilization the advice names.
const HPATarget = 70

// adviseBelow: only a target under this gets advice.
const adviseBelow = 60

// advise: the saving from raising the target, only when it beats the saving from shrinking the request (own).
// A ContainerResource target (container set) is advice for that container only.
func advise(target, replicas, min int, cost, own float64, container, rec string) (float64, bool) {
	if target <= 0 || target >= adviseBelow || replicas <= min || (container != "" && container != rec) {
		return 0, false
	}
	sv := cost * (1 - float64(target)/HPATarget)
	return sv, sv > own
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
	changed := false
	for i, r := range recs {
		h, ok := byWorkload[[2]string{r.Namespace, r.Workload}]
		if !ok {
			continue
		}
		rec := &recs[i]
		ref := &HPARef{Name: h.Name, Resources: h.Resources}
		memSaving := rec.MonthlySaving - rec.CPUSaving
		if sv, ok := advise(h.CPUTarget, h.Replicas, h.MinReplicas, rec.CPUCost, rec.CPUSaving, h.CPUContainer, rec.Container); ok {
			ref.Advice = append(ref.Advice, TargetAdvice{"cpu", h.CPUTarget, sv})
			rec.MonthlySaving += sv - rec.CPUSaving
			rec.Recommended.MilliCPU, changed = rec.Request.MilliCPU, true
		}
		if sv, ok := advise(h.MemTarget, h.Replicas, h.MinReplicas, rec.MemCost, memSaving, h.MemContainer, rec.Container); ok {
			ref.Advice = append(ref.Advice, TargetAdvice{"memory", h.MemTarget, sv})
			rec.MonthlySaving += sv - memSaving
			rec.Recommended.MemBytes, changed = rec.Request.MemBytes, true
		}
		rec.HPA = ref
	}
	if changed { // same order as Build
		slices.SortFunc(recs, func(a, b Rec) int {
			return cmp.Or(cmp.Compare(b.MonthlySaving, a.MonthlySaving), cmp.Compare(a.Namespace+a.Workload+a.Container, b.Namespace+b.Workload+b.Container))
		})
	}
}
