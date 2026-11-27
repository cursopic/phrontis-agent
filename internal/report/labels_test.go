package report

import (
	"math"
	"testing"
)

func TestBuildByLabel(t *testing.T) {
	nodes := []Node{{Name: "n", Capacity: Resources{4000, 8 << 30}}}
	c := []Container{{Name: "c", Request: Resources{1000, 1 << 30}}}
	pods := []Pod{
		{Namespace: "a", Name: "1", Workload: "Deployment/x", Node: "n", Containers: c, Labels: map[string]string{"team": "pay", "app": "x"}},
		{Namespace: "a", Name: "2", Workload: "Deployment/x", Node: "n", Containers: c, Labels: map[string]string{"team": "pay"}},
		{Namespace: "b", Name: "3", Workload: "Deployment/y", Node: "n", Containers: c, Labels: map[string]string{"team": "web", "pod-template-hash": "abc"}},
		{Namespace: "b", Name: "4", Workload: "Deployment/z", Node: "n", Containers: c},
	}
	r := Build(nodes, pods, Prices{CPUHour: 0.03, GiBHour: 0.004})
	if _, ok := r.Labels["pod-template-hash"]; ok {
		t.Error("per-revision label must not be a key")
	}
	total := 0.0
	for _, l := range r.Namespaces {
		total += l.Monthly
	}
	for k, lines := range r.Labels {
		sum, pc := 0.0, 0
		for _, l := range lines {
			sum += l.Monthly
			pc += l.Pods
		}
		if math.Abs(sum-total) > 1e-9 || pc != len(pods) {
			t.Errorf("%s: lines %.6f over %d pods, want %.6f over %d", k, sum, pc, total, len(pods))
		}
	}
	team := r.Labels["team"]
	if len(team) != 3 || team[0].Value != "pay" || team[0].Pods != 2 || team[1].Value != "web" || team[2].Value != "" || team[2].Pods != 1 {
		t.Errorf("team = %+v, want pay(2), web(1), unlabeled(1) last", team)
	}
	app := r.Labels["app"]
	if len(app) != 2 || app[0].Value != "x" || app[1].Value != "" || app[1].Pods != 3 {
		t.Errorf("app = %+v, want x(1) then unlabeled(3)", app)
	}
	if got := Build(nodes, pods[3:], Prices{}).Labels; got != nil {
		t.Errorf("no labels at all: %v", got)
	}
}

func TestKeepLabels(t *testing.T) {
	if KeepLabels(map[string]string{"pod-template-hash": "x"}) != nil {
		t.Error("only noisy labels: want nil")
	}
	if got := KeepLabels(map[string]string{"team": "a", "pod-template-hash": "x"}); len(got) != 1 || got["team"] != "a" {
		t.Errorf("got %v", got)
	}
}

func TestEmptyLabelValueIsUnlabeled(t *testing.T) {
	// team="" and no team label share one "" line, so the UI never gets two rows with the same key.
	nodes := []Node{{Name: "n", Capacity: Resources{4000, 8 << 30}}}
	c := []Container{{Name: "c", Request: Resources{1000, 1 << 30}}}
	pods := []Pod{
		{Namespace: "a", Name: "1", Node: "n", Containers: c, Labels: map[string]string{"team": "pay"}},
		{Namespace: "a", Name: "2", Node: "n", Containers: c, Labels: map[string]string{"team": ""}},
		{Namespace: "a", Name: "3", Node: "n", Containers: c},
	}
	team := Build(nodes, pods, Prices{CPUHour: 0.03, GiBHour: 0.004}).Labels["team"]
	if len(team) != 2 || team[0].Value != "pay" || team[1].Value != "" || team[1].Pods != 2 {
		t.Errorf("team = %+v, want pay(1), unlabeled(2)", team)
	}
}
