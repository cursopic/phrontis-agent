package cli

import (
	"context"
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsfake "k8s.io/metrics/pkg/client/clientset/versioned/fake"

	"github.com/cursopic/phrontis/internal/report"
)

func initContainer(name, cpu string, sidecar bool) corev1.Container {
	c := corev1.Container{Name: name, Resources: corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")}}}
	if p := corev1.ContainerRestartPolicyAlways; sidecar {
		c.RestartPolicy = &p
	}
	return c
}

// Spec 031: a native sidecar is a container of the pod; a normal init container is not.
func TestCollectSidecars(t *testing.T) {
	mesh, logs, setup := initContainer("mesh", "100m", true), initContainer("logs", "50m", true), initContainer("setup", "900m", false)
	with := func(p *corev1.Pod) *corev1.Pod {
		p.Spec.InitContainers = []corev1.Container{setup, mesh, logs}
		p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "logs",
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"}}}}
		return p
	}
	inTemplate := logs // the template's limit differs from the pod's, as after the webhook
	inTemplate.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")}
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "db"}, Spec: appsv1.StatefulSetSpec{Template: corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{InitContainers: []corev1.Container{setup, inTemplate}, Containers: []corev1.Container{{Name: "app"}}}}}}
	// No StatefulSet "gone" in the cluster: its template is not known, so nothing is called injected.
	cs := fake.NewClientset(testNode(), sts, with(pod("db-0", "StatefulSet", "db", "n1", "250m")), with(pod("gone-0", "StatefulSet", "gone", "n1", "250m")))
	mc := &metricsfake.Clientset{}
	mc.AddReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &metricsv1beta1.PodMetricsList{Items: []metricsv1beta1.PodMetrics{{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "db-0"},
			Containers: []metricsv1beta1.ContainerMetrics{{Name: "mesh", Usage: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("7m"), corev1.ResourceMemory: resource.MustParse("30Mi")}}}}}}, nil
	})
	_, pods, _, _, _, _, _, err := collect(context.Background(), cs, mc, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]report.Container{
		"StatefulSet/db": {
			{Name: "mesh", Sidecar: true, Injected: true, AllowPrivEsc: true, Request: report.Resources{MilliCPU: 100}, Limit: report.Resources{MemBytes: 256 << 20},
				Usage: report.Resources{MilliCPU: 7, MemBytes: 30 << 20}, HasUsage: true},
			{Name: "logs", Sidecar: true, AllowPrivEsc: true, Request: report.Resources{MilliCPU: 50}, Limit: report.Resources{MemBytes: 256 << 20},
				OOMKilled: true, ManifestMemLimit: 128 << 20},
		},
		"StatefulSet/gone": {
			{Name: "mesh", Sidecar: true, AllowPrivEsc: true, Request: report.Resources{MilliCPU: 100}, Limit: report.Resources{MemBytes: 256 << 20}},
			{Name: "logs", Sidecar: true, AllowPrivEsc: true, Request: report.Resources{MilliCPU: 50}, Limit: report.Resources{MemBytes: 256 << 20}, OOMKilled: true},
		},
	}
	if len(pods) != 2 {
		t.Fatalf("pods = %+v", pods)
	}
	for _, p := range pods {
		if len(p.Containers) != 3 || p.Containers[0].Sidecar || !reflect.DeepEqual(p.Containers[1:], want[p.Workload]) {
			t.Errorf("%s: got %+v, want %+v", p.Workload, p.Containers, want[p.Workload])
		}
	}
}

func TestNoteSidecar(t *testing.T) {
	if got := note(report.Rec{Sidecar: true, OOMKilled: true}); got != "sidecar; OOM-killed" {
		t.Errorf("sidecar: %q", got)
	}
	if got := note(report.Rec{Sidecar: true, Injected: true}); got != "sidecar, not in the manifest" {
		t.Errorf("injected: %q", got)
	}
}
