package recs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cursopic/phrontis/internal/report"
)

// The cache refreshes every 5 minutes of the injected clock, sends the cluster key, keeps the last good set when a
// fetch fails or returns garbage, empties when the key is refused, and takes an empty set from the API as an empty set.
func TestCacheRefresh(t *testing.T) {
	type reply struct {
		code int
		body string
	}
	var next atomic.Value
	var auth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.Store(r.Header.Get("Authorization"))
		x := next.Load().(reply)
		w.WriteHeader(x.code)
		w.Write([]byte(x.body))
	}))
	defer srv.Close()
	c := &Cache{URL: srv.URL, APIKey: "pt_test"}
	if _, ok := c.Get("shop", "Deployment/web", "app"); ok {
		t.Fatal("a cache never refreshed must hold nothing")
	}

	ticks := make(chan chan time.Time)
	after := func(d time.Duration) <-chan time.Time { // called right after each refresh
		if d != 5*time.Minute {
			t.Errorf("refresh period = %s, want 5m", d)
		}
		tick := make(chan time.Time)
		ticks <- tick
		return tick
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	next.Store(reply{200, `[{"Namespace":"shop","Workload":"Deployment/web","Container":"app","Requests":{"MilliCPU":120,"MemBytes":268435456}}]`})
	go func() { c.Run(ctx, after, t.Logf); close(done) }()
	tick := <-ticks // the first refresh runs at once, before any wait
	web := report.Resources{MilliCPU: 120, MemBytes: 256 << 20}
	if got, ok := c.Get("shop", "Deployment/web", "app"); !ok || got != web {
		t.Fatalf("after the first refresh = %+v %v, want %+v", got, ok, web)
	}
	if a := auth.Load(); a != "Bearer pt_test" {
		t.Errorf("Authorization = %q, want the cluster key", a)
	}
	if _, ok := c.Get("shop", "Deployment/web", "sidecar"); ok {
		t.Error("a container without a rec must have none")
	}

	for _, bad := range []reply{{500, ""}, {200, `[{"Namespace":"shop"`}, {200, ""}} {
		next.Store(bad)
		tick <- time.Time{}
		tick = <-ticks
		if got, ok := c.Get("shop", "Deployment/web", "app"); !ok || got != web {
			t.Errorf("after a failed refresh (%d %q) = %+v %v, want the last good %+v", bad.code, bad.body, got, ok, web)
		}
	}

	next.Store(reply{401, ""}) // the cluster key was revoked: stop mutating
	tick <- time.Time{}
	tick = <-ticks
	if _, ok := c.Get("shop", "Deployment/web", "app"); ok {
		t.Error("a refused key must empty the cache")
	}

	next.Store(reply{200, `[{"Namespace":"shop","Workload":"Deployment/web","Container":"app","Requests":{"MilliCPU":120,"MemBytes":268435456}}]`})
	tick <- time.Time{}
	tick = <-ticks
	next.Store(reply{200, `[]`}) // the API says: no recommendations now
	tick <- time.Time{}
	<-ticks
	if _, ok := c.Get("shop", "Deployment/web", "app"); ok {
		t.Error("an empty set from the API must replace the cache")
	}
	cancel()
	<-done
}
