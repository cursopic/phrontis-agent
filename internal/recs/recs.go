// Package recs is a local copy of a cluster's recommendations (spec 009). Callers read it
// with Get on the request path, which never calls out; Run refreshes it from the API every 5 minutes.
package recs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/cursopic/phrontis/internal/report"
)

// Every is how often Run refreshes the cache (spec 009).
const Every = 5 * time.Minute

type key struct{ namespace, workload, container string }

// Cache holds the last set the API returned. Set URL and APIKey; until the first good refresh it holds nothing, and a
// container with no recommendation is not mutated.
type Cache struct {
	URL    string // <api>/v1/clusters/{id}/recommendations
	APIKey string // the cluster key
	mu     sync.RWMutex
	sizes  map[key]report.Resources
}

// Get returns the container's recommended requests; ok is false when there are none.
func (c *Cache) Get(namespace, workload, container string) (report.Resources, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	r, ok := c.sizes[key{namespace, workload, container}]
	return r, ok
}

// Refresh fetches the set and replaces the cache with it. On any error the cache keeps the last good set: a failed
// fetch must never read as "no recommendations" (or as an empty set). The one exception is a refused key (401): the
// cluster was revoked, so the cache empties and nothing is mutated.
func (c *Cache) Refresh(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second) // the API sizes a 7-day window within 25 s
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		c.mu.Lock()
		c.sizes = nil
		c.mu.Unlock()
		return fmt.Errorf("recommendations: %s: the key was refused, the cache is empty", resp.Status)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("recommendations: %s: keeping the last good set", resp.Status)
	}
	var sizes []report.Size
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&sizes); err != nil {
		return fmt.Errorf("recommendations: keeping the last good set: %w", err)
	}
	m := make(map[key]report.Resources, len(sizes))
	for _, s := range sizes {
		m[key{s.Namespace, s.Workload, s.Container}] = s.Requests
	}
	c.mu.Lock()
	c.sizes = m
	c.mu.Unlock()
	return nil
}

// Run refreshes at once and then every 5 minutes until ctx is done. after is the injected clock (time.After outside
// tests); each failed refresh goes to logf.
func (c *Cache) Run(ctx context.Context, after func(time.Duration) <-chan time.Time, logf func(format string, args ...any)) {
	for {
		if err := c.Refresh(ctx); err != nil && ctx.Err() == nil {
			logf("recs: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-after(Every):
		}
	}
}
