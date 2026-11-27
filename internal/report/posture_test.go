package report

import "testing"

// good is a container that passes every check.
func good() Container {
	return Container{Name: "app", Image: "reg.io/app:1.2", RunAsNonRoot: true,
		Request: Resources{100, 1 << 28}, Limit: Resources{0, 1 << 29}}
}

func checks(p Pod) map[string]bool {
	m := map[string]bool{}
	for _, f := range BuildSecurity([]Pod{p}).Findings {
		m[f.Check] = true
	}
	return m
}

func TestPosture(t *testing.T) {
	cases := []struct {
		check string
		level string
		bad   func(*Pod)
		fix   func(*Pod)
	}{
		{"privileged", "high", func(p *Pod) { p.Containers[0].Privileged = true }, func(p *Pod) { p.Containers[0].Privileged = false }},
		{"host-namespaces", "high", func(p *Pod) { p.HostPID = true }, func(p *Pod) { p.HostPID = false }},
		{"host-path", "high", func(p *Pod) { p.HostPath = true }, func(p *Pod) { p.HostPath = false }},
		{"extra-capabilities", "medium", func(p *Pod) { p.Containers[0].AddedCaps = []string{"NET_BIND_SERVICE", "sys_admin"} }, func(p *Pod) { p.Containers[0].AddedCaps = []string{"NET_BIND_SERVICE"} }},
		{"privilege-escalation", "medium", func(p *Pod) { p.Containers[0].AllowPrivEsc = true }, func(p *Pod) { p.Containers[0].AllowPrivEsc = false }},
		{"run-as-root", "medium", func(p *Pod) { p.Containers[0].RunAsNonRoot = false }, func(p *Pod) { p.Containers[0].RunAsNonRoot = true }},
		{"latest-tag", "low", func(p *Pod) { p.Containers[0].Image = "nginx" }, func(p *Pod) { p.Containers[0].Image = "nginx@sha256:abc" }},
		{"no-limits", "low", func(p *Pod) { p.Containers[0].Limit = Resources{} }, func(p *Pod) { p.Containers[0].Limit.MemBytes = 1 }},
		{"no-requests", "low", func(p *Pod) { p.Containers[0].Request.MilliCPU = 0 }, func(p *Pod) { p.Containers[0].Request.MilliCPU = 1 }},
	}
	for _, tc := range cases {
		t.Run(tc.check, func(t *testing.T) {
			p := Pod{Namespace: "ns", Name: "p", Workload: "Deployment/a", Containers: []Container{good()}}
			if len(checks(p)) != 0 {
				t.Fatalf("baseline pod has findings: %v", BuildSecurity([]Pod{p}).Findings)
			}
			tc.bad(&p)
			fs := BuildSecurity([]Pod{p}).Findings
			if len(fs) != 1 || fs[0].Check != tc.check || fs[0].Level != tc.level {
				t.Fatalf("want one %s/%s finding, got %v", tc.check, tc.level, fs)
			}
			tc.fix(&p)
			if checks(p)[tc.check] {
				t.Errorf("%s still fires after the fix", tc.check)
			}
		})
	}
}

func TestPostureLatestTag(t *testing.T) {
	for img, want := range map[string]bool{
		"nginx": true, "nginx:latest": true, "nginx:1.25": false, "reg.io:5000/app": true, "reg.io:5000/app:2": false,
		"app@sha256:abc": false, "app:latest@sha256:abc": false,
	} {
		if latestTag(img) != want {
			t.Errorf("latestTag(%q) = %v, want %v", img, !want, want)
		}
	}
}

func TestPostureOldSample(t *testing.T) {
	p := Pod{Namespace: "ns", Name: "p", Workload: "Deployment/a", Containers: []Container{{Name: "app", Request: Resources{100, 1}, Limit: Resources{0, 1}}}}
	s := BuildSecurity([]Pod{p})
	if len(s.Findings) != 0 || s.Unread != 1 {
		t.Errorf("an old sample must give no posture findings and count as unread: %+v", s)
	}
	p.Containers[0].Limit = Resources{} // limits and requests are still checked without the new fields
	if got := BuildSecurity([]Pod{p}).Findings; len(got) != 1 || got[0].Check != "no-limits" {
		t.Errorf("want no-limits, got %v", got)
	}
}

func TestPostureSystemAndDedupe(t *testing.T) {
	c := good()
	c.Privileged = true
	pods := []Pod{
		{Namespace: "kube-system", Name: "a", Workload: "DaemonSet/x", Containers: []Container{c}},
		{Namespace: "kube-system", Name: "b", Workload: "DaemonSet/x", Containers: []Container{c}},
		{Namespace: "ns", Name: "etcd", Static: true, Containers: []Container{c}},
	}
	fs := BuildSecurity(pods).Findings
	if len(fs) != 2 || !fs[0].System || !fs[1].System {
		t.Errorf("want two system findings (replicas of one workload count once), got %v", fs)
	}
}
