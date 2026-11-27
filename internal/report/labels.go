package report

import (
	"cmp"
	"slices"
	"strings"
)

// LabelLine is the cost of the pods that carry one value of a label key; Value "" is the pods without that key or with an empty value.
type LabelLine struct {
	Value   string
	Pods    int
	Monthly float64
}

// maxLabelKeys caps how many keys a report carries (the most widely used first): a cluster can have hundreds.
const maxLabelKeys = 20

// noisyLabel: keys that name one revision or one pod, so grouping by them gives one line per rollout or per pod.
func noisyLabel(k string) bool {
	switch k {
	case "pod-template-hash", "controller-revision-hash", "pod-template-generation", "controller-uid", "job-name",
		"batch.kubernetes.io/job-name", "batch.kubernetes.io/controller-uid", "statefulset.kubernetes.io/pod-name",
		"apps.kubernetes.io/pod-index", "security.istio.io/tlsMode", "service.istio.io/canonical-revision":
		return true
	}
	return false
}

// KeepLabels returns the labels worth sending in a sample (nil when none are).
func KeepLabels(in map[string]string) map[string]string {
	var out map[string]string
	for k, v := range in {
		if noisyLabel(k) {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[k] = v
	}
	return out
}

// byLabel splits the cost of the pods (cost[i] is pods[i]'s) by each label key, an unlabeled bucket (Value "") included,
// so for every key the lines add up to the namespace lines' total. Keys are kept for at most maxLabelKeys, widest first.
func byLabel(pods []Pod, cost []float64) map[string][]LabelLine {
	by := map[string]map[string]*LabelLine{}
	for i, p := range pods {
		for k, v := range p.Labels {
			if noisyLabel(k) {
				continue
			}
			if by[k] == nil {
				by[k] = map[string]*LabelLine{}
			}
			if v == "" {
				continue // an empty value goes to the unlabeled bucket, so the key has one "" line only
			}
			l := by[k][v]
			if l == nil {
				l = &LabelLine{Value: v}
				by[k][v] = l
			}
			l.Pods++
			l.Monthly += cost[i]
		}
	}
	if len(by) == 0 {
		return nil
	}
	keys := make([]string, 0, len(by))
	for k := range by {
		keys = append(keys, k)
	}
	labeled := func(k string) (n int) {
		for _, l := range by[k] {
			n += l.Pods
		}
		return
	}
	slices.SortFunc(keys, func(a, b string) int { return cmp.Or(cmp.Compare(labeled(b), labeled(a)), strings.Compare(a, b)) })
	keys = keys[:min(len(keys), maxLabelKeys)]
	out := map[string][]LabelLine{}
	for _, k := range keys {
		m := by[k]
		var rest LabelLine
		for i, p := range pods {
			if p.Labels[k] == "" {
				rest.Pods++
				rest.Monthly += cost[i]
			}
		}
		lines := make([]LabelLine, 0, len(m)+1)
		for _, l := range m {
			lines = append(lines, *l)
		}
		slices.SortFunc(lines, func(a, b LabelLine) int {
			return cmp.Or(cmp.Compare(b.Monthly, a.Monthly), strings.Compare(a.Value, b.Value))
		})
		if rest.Pods > 0 {
			lines = append(lines, rest) // last, whatever it costs: the UI shows it as "Unlabeled"
		}
		out[k] = lines
	}
	return out
}
