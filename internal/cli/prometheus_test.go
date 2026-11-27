package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cursopic/phrontis/internal/report"
)

func TestParseHistory(t *testing.T) {
	for in, want := range map[string]time.Duration{"7d": 7 * 24 * time.Hour, "1h": time.Hour, "36h": 36 * time.Hour, "30d": 30 * 24 * time.Hour, "1.5d": 36 * time.Hour} {
		if got, err := parseHistory(in); err != nil || got != want {
			t.Errorf("%s = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"59m", "31d", "721h", "0d", "-2d", "d", "x", "7dd", ""} {
		if _, err := parseHistory(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestSetupPromFlags(t *testing.T) {
	defer func() { prom = nil }()
	for name, c := range map[string]struct {
		u, hist string
		set     bool
		window  time.Duration
		other   bool
	}{
		"agent mode":    {"http://p", "7d", false, 0, true},
		"history range": {"http://p", "45d", true, 0, false},
		"with window":   {"http://p", "7d", false, time.Minute, false},
		"history alone": {"", "7d", true, 0, false},
	} {
		if err := setupProm(c.u, "", "", c.hist, c.set, c.window, c.other); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := setupProm("", "", "", "7d", false, 0, true); err != nil || prom != nil {
		t.Errorf("no --prometheus must be a no-op: %v", err)
	}
	if err := setupProm("http://p", "", "", "7d", false, 0, false); err != nil || prom == nil || prom.history != 7*24*time.Hour {
		t.Errorf("valid flags: %v %+v", err, prom)
	}
}

func promBody(rows ...[4]string) string { // namespace, pod, container, value
	var rs []string
	for _, r := range rows {
		rs = append(rs, fmt.Sprintf(`{"metric":{"namespace":%q,"pod":%q,"container":%q},"value":[1,%q]}`, r[0], r[1], r[2], r[3]))
	}
	return `{"status":"success","data":{"resultType":"vector","result":[` + strings.Join(rs, ",") + `]}}`
}

func promPods() []report.Pod {
	return []report.Pod{
		{Namespace: "shop", Name: "web-7d9-a", Workload: "Deployment/web", Containers: []report.Container{{Name: "app"}, {Name: "sidecar"}}},
		{Namespace: "shop", Name: "db-0", Workload: "StatefulSet/db", Containers: []report.Container{{Name: "app", Usage: report.Resources{MilliCPU: 5}, HasUsage: true}}},
	}
}

func TestApplyHistory(t *testing.T) {
	const token = "s3cret-token-value"
	var auth, sel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		q := r.URL.Query().Get("query")
		if r.URL.Path != "/api/v1/query" || !strings.Contains(q, "[604800s") {
			t.Errorf("unexpected request %s %q", r.URL.Path, q)
		}
		sel = q
		if strings.HasPrefix(q, "quantile_over_time") {
			fmt.Fprint(w, promBody(
				[4]string{"shop", "web-7d9-a", "app", "0.25"},
				[4]string{"shop", "web-6c1-zz", "app", "0.4"}, // deleted pod of the same Deployment
				[4]string{"shop", "gone-5f-xy", "app", "9"},   // workload gone: dropped
				[4]string{"shop", "db-0", "app", "0.1234"}))
			return
		}
		fmt.Fprint(w, promBody(
			[4]string{"shop", "web-7d9-a", "app", "104857600"},
			[4]string{"shop", "web-6c1-zz", "app", "209715200"},
			[4]string{"shop", "db-0", "app", "1073741824"}))
	}))
	defer srv.Close()
	pods := promPods()
	src := promSource{url: srv.URL + "/", token: token, selector: `cluster="prod"`, history: 7 * 24 * time.Hour}
	n, err := src.applyHistory(context.Background(), pods)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer "+token {
		t.Errorf("Authorization = %q", auth)
	}
	if !strings.Contains(sel, `container!="POD",cluster="prod"}`) {
		t.Errorf("selector missing from %q", sel)
	}
	web, db := pods[0].Containers, pods[1].Containers[0]
	if u := web[0].Usage; !web[0].HasUsage || u.MilliCPU != 400 || u.MemBytes != 200<<20 { // folded: highest of both pods
		t.Errorf("web/app = %+v %v", u, web[0].HasUsage)
	}
	if web[1].HasUsage || web[1].Usage != (report.Resources{}) {
		t.Errorf("container without series must have no usage: %+v", web[1])
	}
	if !db.HasUsage || db.Usage.MilliCPU != 123 || db.Usage.MemBytes != 1<<30 {
		t.Errorf("db/app = %+v", db.Usage)
	}
	if n != 6 {
		t.Errorf("series used = %d, want 6", n)
	}
}

func TestApplyHistoryEmptyResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, promBody()) }))
	defer srv.Close()
	pods := promPods()
	n, err := promSource{url: srv.URL, history: time.Hour}.applyHistory(context.Background(), pods)
	if err != nil || n != 0 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	for _, p := range pods {
		for _, c := range p.Containers {
			if c.HasUsage {
				t.Errorf("%s/%s kept usage", p.Name, c.Name)
			}
		}
	}
}

func TestPromErrorsHideTokenAndBody(t *testing.T) {
	const token = "s3cret-token-value"
	dir := t.TempDir()
	file := filepath.Join(dir, "token")
	if err := os.WriteFile(file, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readToken(file)
	if err != nil || got != token {
		t.Fatalf("readToken = %q, %v", got, err)
	}
	unauthorized := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, "bad token "+r.Header.Get("Authorization")) // a body that echoes the token
	}))
	defer unauthorized.Close()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer slow.Close()
	old := promTimeout
	promTimeout = 50 * time.Millisecond
	defer func() { promTimeout = old }()
	for name, c := range map[string]struct{ url, want string }{
		"401":     {unauthorized.URL, "prometheus: HTTP 401"},
		"timeout": {slow.URL, "prometheus: no answer in 50ms"},
		"refused": {"http://127.0.0.1:1", "prometheus: request failed (cannot connect)"},
	} {
		_, err := promSource{url: c.url, token: token, history: time.Hour}.applyHistory(context.Background(), promPods())
		if err == nil || err.Error() != c.want {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
			continue
		}
		if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "bad token") || strings.Contains(err.Error(), "\n") {
			t.Errorf("%s: error leaks: %q", name, err)
		}
	}
	if _, err := readToken(filepath.Join(dir, "missing")); err == nil || strings.Contains(err.Error(), dir) {
		t.Errorf("missing token file: %v", err)
	}
}
