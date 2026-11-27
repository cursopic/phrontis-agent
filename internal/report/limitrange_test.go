package report

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestMarkLimitRanges(t *testing.T) {
	list := func(cpu, mem string) corev1.ResourceList {
		return corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(mem)}
	}
	item := corev1.LimitRangeItem{Type: corev1.LimitTypeContainer, Min: list("50m", "64Mi"), Max: list("2", "2Gi"), MaxLimitRequestRatio: list("4", "4")}
	pod := corev1.LimitRangeItem{Type: corev1.LimitTypePod, Min: list("8", "8Gi")}
	r := func(cpu, mib int64) Resources { return Resources{MilliCPU: cpu, MemBytes: mib << 20} }
	for _, tc := range []struct {
		name          string
		ns            string
		req, rec, lim Resources
		want          bool
	}{
		{"fits", "a", r(1000, 1024), r(500, 512), r(1000, 1024), false},
		{"at the bounds", "a", r(1000, 1024), r(50, 64), r(200, 256), false},
		{"below the CPU min", "a", r(1000, 1024), r(49, 512), Resources{}, true},
		{"below the memory min", "a", r(1000, 1024), r(500, 63), Resources{}, true},
		{"above the memory max", "a", r(1000, 1024), r(500, 2049), Resources{}, true},
		{"ratio: the kept CPU limit is now 5 times the request", "a", r(1000, 1024), r(200, 512), r(1000, 1024), true},
		{"a limit below the new request rises with it", "a", r(100, 128), r(1000, 128), r(400, 256), false},
		{"no change of size", "a", r(10, 10), r(10, 10), Resources{}, false},
		{"another namespace", "b", r(1000, 1024), r(10, 32), Resources{}, false},
	} {
		recs := []Rec{{Namespace: tc.ns, Request: tc.req, Recommended: tc.rec, Limit: tc.lim}}
		MarkLimitRanges(recs, map[string][]corev1.LimitRangeItem{"a": {pod, item}})
		if recs[0].LimitRange != tc.want {
			t.Errorf("%s: got %v", tc.name, recs[0].LimitRange)
		}
	}
}
