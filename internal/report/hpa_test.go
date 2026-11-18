package report

import "testing"

func TestMarkHPA(t *testing.T) {
	recs := []Rec{{Namespace: "a", Workload: "Deployment/web"}, {Namespace: "b", Workload: "Deployment/web"}, {Namespace: "a", Workload: "StatefulSet/db"}}
	MarkHPA(recs, []HPA{
		{Namespace: "a", Name: "web-z", Target: "Deployment/web", Resources: []string{"memory"}},
		{Namespace: "a", Name: "web-hpa", Target: "Deployment/web", Resources: []string{"cpu"}},
		{Namespace: "a", Name: "db", Target: "Deployment/db"}, // kind differs from the rec's workload
	})
	if h := recs[0].HPA; h == nil || h.Name != "web-hpa" || len(h.Resources) != 1 || h.Resources[0] != "cpu" {
		t.Errorf("a/web: %+v", h)
	}
	if recs[1].HPA != nil || recs[2].HPA != nil {
		t.Errorf("other namespace or kind must stay unmarked: %+v %+v", recs[1].HPA, recs[2].HPA)
	}
}
