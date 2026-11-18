package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	metrics "k8s.io/metrics/pkg/client/clientset/versioned"

	"github.com/cursopic/phrontis/internal/pricing"
	"github.com/cursopic/phrontis/internal/report"
)

// sample is the POST /v1/samples body; it must match store.Sample (TestAgentShips decodes it strictly).
type sample struct {
	Nodes         []report.Node
	Pods          []report.Pod
	Keys          map[string]pricing.Key
	Volumes       []report.Volume
	LoadBalancers []report.LoadBalancer
	PDBs          []report.PDB
	HPAs          []report.HPA `json:",omitempty"` // omitted when none, so an API older than #229 still takes the sample

	WorkloadController bool   `json:",omitempty"`
	ClusterUID         string `json:",omitempty"`
}

var ErrBadKey = errors.New("the API rejected the key (401): check PHRONTIS_API_KEY")

// agent runs in the cluster: one sample every interval, POSTed to the API, until SIGTERM.
func agent(kctx, api string, every time.Duration) error {
	if every <= 0 {
		return errors.New("--every must be positive")
	}
	base, key, err := APITarget(api)
	if err != nil {
		return err
	}
	cs, mc, _, err := clients(kctx)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(os.Stderr, "phrontis agent: sending a sample to %s every %s\n", base, every)
	return runAgent(ctx, cs, mc, base+"/v1/samples", key, every, &http.Client{Timeout: 30 * time.Second}, os.Stderr)
}

// APITarget checks --api and $PHRONTIS_API_KEY and returns the base URL without a trailing slash.
func APITarget(api string) (base, key string, err error) {
	key = os.Getenv("PHRONTIS_API_KEY")
	if key == "" {
		return "", "", errors.New("--api needs the cluster's key in PHRONTIS_API_KEY")
	}
	u, err := url.Parse(api)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", "", fmt.Errorf("--api %q: want an http(s) base URL", api)
	}
	if u.Scheme == "http" {
		fmt.Fprintln(os.Stderr, "phrontis: warning: the API key travels over plain http")
	}
	return strings.TrimSuffix(api, "/"), key, nil
}

// runAgent ships until ctx ends. A failed sample is logged and dropped; only a rejected key stops it,
// so a wrong key shows up as a crash-looping pod instead of a quiet log.
// shortcut: samples taken while the API is down are lost; add a retry queue if gaps hide peaks.
// shortcut: one sample must fit the API's 8 MiB body or every tick gets 413; gzip or split it when a cluster outgrows that.
func runAgent(ctx context.Context, cs kubernetes.Interface, mc metrics.Interface, endpoint, key string, every time.Duration, hc *http.Client, log io.Writer) error {
	tick := time.NewTicker(every)
	defer tick.Stop()
	hpaWarn := &onceWriter{w: log} // an old chart without HPA access would warn every tick
	for {
		if err := shipOnce(ctx, cs, mc, endpoint, key, hc, log, hpaWarn); errors.Is(err, ErrBadKey) {
			return err
		} else if err != nil && ctx.Err() == nil {
			fmt.Fprintln(log, "phrontis agent:", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

func shipOnce(ctx context.Context, cs kubernetes.Interface, mc metrics.Interface, endpoint, key string, hc *http.Client, log, hpaWarn io.Writer) error {
	nodes, pods, keys, vols, lbs, pdbs, _, err := collect(ctx, cs, mc, 0)
	if err != nil {
		return err
	}
	ctl, err := controllerRunning(ctx, cs)
	if err != nil {
		return err
	}
	s := sample{Nodes: nodes, Pods: pods, Keys: keys, Volumes: vols, LoadBalancers: lbs, PDBs: pdbs, WorkloadController: ctl,
		HPAs: listHPAs(ctx, cs, hpaWarn)}
	// The kube-system UID names the cluster (#134). Without it (say, the RBAC of an older chart) the sample still feeds
	// the report.
	if ns, err := cs.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{}); err == nil {
		s.ClusterUID = string(ns.UID)
	} else {
		fmt.Fprintln(log, "phrontis agent: no cluster UID in this sample, so the node policy cannot be checked against this cluster:", err)
	}
	return PostJSON(ctx, hc, endpoint, key, s)
}

// controllerRunning reports whether a ready pod labelled phrontis-autoscaler runs with --mode=apply, in any namespace
// (#129). It reads pods only, which the agent already lists.
func controllerRunning(ctx context.Context, cs kubernetes.Interface) (bool, error) {
	list, err := cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=phrontis-autoscaler"})
	if err != nil {
		return false, fmt.Errorf("list autoscaler pods: %w", err)
	}
	for _, p := range list.Items {
		apply := false
		for _, c := range p.Spec.Containers {
			apply = apply || slices.Contains(c.Args, "--mode=apply")
		}
		for _, c := range p.Status.Conditions {
			if apply && c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				return true, nil
			}
		}
	}
	return false, nil
}

// PostJSON POSTs v with the cluster key; the API answers 202 when it stored it.
func PostJSON(ctx context.Context, hc *http.Client, endpoint, key string, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16)) // drain so the connection is reused
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return ErrBadKey
	case resp.StatusCode != http.StatusAccepted:
		return fmt.Errorf("API answered %s", resp.Status)
	}
	return nil
}

// onceWriter passes the first write through and drops the rest.
type onceWriter struct {
	w    io.Writer
	done bool
}

func (o *onceWriter) Write(p []byte) (int, error) {
	if o.done {
		return len(p), nil
	}
	o.done = true
	return o.w.Write(p)
}
