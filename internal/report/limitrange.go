package report

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// MarkLimitRanges sets Rec.LimitRange on every rec whose new size the LimitRanges of its namespace reject. Call it after
// MarkHPA, which can take a request change back. A limit below the new request rises to it, as the report's note says.
// shortcut: Pod-type items are not checked; they need the sum of the pod's containers.
func MarkLimitRanges(recs []Rec, items map[string][]corev1.LimitRangeItem) {
	for i := range recs {
		r := &recs[i]
		lim := r.Limit
		if lim.MilliCPU > 0 {
			lim.MilliCPU = max(lim.MilliCPU, r.Recommended.MilliCPU)
		}
		if lim.MemBytes > 0 {
			lim.MemBytes = max(lim.MemBytes, r.Recommended.MemBytes)
		}
		r.LimitRange = r.Recommended != r.Request && !Fits(items[r.Namespace], r.Recommended, lim)
	}
}

// Fits reports whether a container's new requests and limits pass the Container LimitRange items (min, max,
// maxLimitRequestRatio), as the API's LimitRanger will check them.
func Fits(items []corev1.LimitRangeItem, req, lim Resources) bool {
	for _, it := range items {
		if it.Type != corev1.LimitTypeContainer {
			continue
		}
		for _, n := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
			r, l := Qty(n, req), Qty(n, lim)
			if q, ok := it.Min[n]; ok && (r.Cmp(q) < 0 || !l.IsZero() && l.Cmp(q) < 0) {
				return false
			}
			if q, ok := it.Max[n]; ok && (r.Cmp(q) > 0 || l.Cmp(q) > 0) {
				return false
			}
			if q, ok := it.MaxLimitRequestRatio[n]; ok && !l.IsZero() && l.AsApproximateFloat64() > q.AsApproximateFloat64()*r.AsApproximateFloat64() {
				return false
			}
		}
	}
	return true
}

// Qty is r's value for n as a Kubernetes quantity.
func Qty(n corev1.ResourceName, r Resources) resource.Quantity {
	if n == corev1.ResourceCPU {
		return *resource.NewMilliQuantity(r.MilliCPU, resource.DecimalSI)
	}
	return *resource.NewQuantity(r.MemBytes, resource.BinarySI)
}
