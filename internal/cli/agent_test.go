package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsfake "k8s.io/metrics/pkg/client/clientset/versioned/fake"
)

func agentFakes() (*fake.Clientset, *metricsfake.Clientset) {
	cs := fake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "uid-1"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: map[string]string{
			"node.kubernetes.io/instance-type": "m5.large", "topology.kubernetes.io/region": "eu-central-1"}},
			Spec: corev1.NodeSpec{ProviderID: "aws:///eu-central-1a/i-0abc"}, Status: corev1.NodeStatus{Capacity: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("4Gi")}}},
		pod("db-0", "StatefulSet", "db", "n1", "500m"),
		pv("pv-1", "", "ebs.csi.aws.com", "Available", "", "", "1Gi"),
		svc("web", "", "", "", true),
		&autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "db"},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "StatefulSet", Name: "db"}}},
	)
	mc := &metricsfake.Clientset{}
	mc.AddReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &metricsv1beta1.PodMetricsList{}, nil
	})
	return cs, mc
}

// controllerPod is a pod of the workload autoscaler's controller, as its chart labels and runs it.
func controllerPod(ns string, ready corev1.ConditionStatus, labels map[string]string, mode string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "ctl-" + string(ready) + "-" + mode, Labels: labels},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "autoscaler",
			Args: []string{"autoscaler", "--mode=" + mode, "--namespace=" + ns, "--tls=/tls"}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: ready}}}}
}

var autoscalerLabels = map[string]string{"app.kubernetes.io/name": "phrontis-autoscaler", "app.kubernetes.io/instance": "x"}

// The meter bills workload Apply hours only with this sign (#129): a ready controller pod in Apply mode, in any
// namespace. A controller in dry-run or kill-switch bills 0 even if its mode row never arrived.
func TestControllerRunning(t *testing.T) {
	ready, notReady := corev1.ConditionTrue, corev1.ConditionFalse
	for _, c := range []struct {
		name string
		pods []runtime.Object
		want bool
	}{
		{"none", nil, false},
		{"not ready (crash loop)", []runtime.Object{controllerPod("phrontis", notReady, autoscalerLabels, "apply")}, false},
		{"ready, other app", []runtime.Object{controllerPod("phrontis", ready, map[string]string{"app.kubernetes.io/name": "x"}, "apply")}, false},
		{"ready, dry-run", []runtime.Object{controllerPod("phrontis", ready, autoscalerLabels, "dry-run")}, false},
		{"ready, kill switch", []runtime.Object{controllerPod("phrontis", ready, autoscalerLabels, "kill-switch")}, false},
		{"ready", []runtime.Object{controllerPod("ops", notReady, autoscalerLabels, "apply"), controllerPod("ops", ready, autoscalerLabels, "apply")}, true},
	} {
		got, err := controllerRunning(context.Background(), fake.NewClientset(c.pods...))
		if err != nil || got != c.want {
			t.Errorf("%s: %v (%v), want %v", c.name, got, err, c.want)
		}
	}
}

func TestAgentShips(t *testing.T) {
	cs, mc := agentFakes()
	cs.Tracker().Add(controllerPod("phrontis", corev1.ConditionTrue, autoscalerLabels, "apply")) // no node: not in Pods
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer pt_k" || r.URL.Path != "/v1/samples" {
			t.Errorf("call %d: auth %q path %q", n, r.Header.Get("Authorization"), r.URL.Path)
		}
		// Decode exactly as the API does, so a field the API does not know fails here.
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		var s Sample
		if err := dec.Decode(&s); err != nil {
			t.Errorf("call %d: API would reject the body: %v", n, err)
		}
		if len(s.Nodes) != 1 || len(s.Pods) != 1 || len(s.Volumes) != 1 || len(s.LoadBalancers) != 1 || len(s.HPAs) != 1 || s.Keys["n1"].Type != "m5.large" || !s.WorkloadController || s.ClusterUID != "uid-1" {
			t.Errorf("call %d: sample = %+v", n, s)
		}
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError) // logged, then the next tick ships again
			return
		}
		w.WriteHeader(http.StatusAccepted)
		cancel()
	}))
	defer srv.Close()

	var log strings.Builder
	if err := runAgent(ctx, cs, mc, srv.URL+"/v1/samples", "pt_k", 10*time.Millisecond, srv.Client(), &log); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || !strings.Contains(log.String(), "500 Internal Server Error") {
		t.Fatalf("calls=%d log=%q", calls.Load(), log.String())
	}
}

func TestAgentStopsOnBadKey(t *testing.T) {
	cs, mc := agentFakes()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runAgent(ctx, cs, mc, srv.URL, "pt_bad", time.Millisecond, srv.Client(), &strings.Builder{}); !errors.Is(err, ErrBadKey) {
		t.Fatalf("err = %v, want ErrBadKey", err)
	}
}

// #148: a sample without the cluster UID is logged, so the reason nodes apply and off refuse shows in the agent's log.
func TestAgentLogsNoClusterUID(t *testing.T) {
	cs, mc := agentFakes()
	if err := cs.CoreV1().Namespaces().Delete(context.Background(), "kube-system", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) }))
	defer srv.Close()
	var log strings.Builder
	if err := shipOnce(context.Background(), cs, mc, srv.URL, "pt_k", srv.Client(), &log, &log); err != nil || !strings.Contains(log.String(), "no cluster UID") {
		t.Fatalf("err = %v, log = %q", err, log.String())
	}
}

// An old chart without HPA access warns once per agent process, not every tick.
func TestOnceWriter(t *testing.T) {
	var b strings.Builder
	o := &onceWriter{w: &b}
	o.Write([]byte("a"))
	o.Write([]byte("b"))
	if b.String() != "a" {
		t.Fatalf("got %q", b.String())
	}
}
