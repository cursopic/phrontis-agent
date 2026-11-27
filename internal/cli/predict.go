package cli

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"slices"
	"strings"
	"text/tabwriter"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/yaml"

	"github.com/cursopic/phrontis/internal/report"
)

// prediction is what a manifest would cost; `phrontis predict --json` prints it as is.
type prediction struct {
	Workloads  []predicted
	Monthly    float64      // Jobs are not in it
	AtLeast    bool         // the manifest runs more than Monthly says: NoRequest or a NotCounted entry with Pods
	NoRequest  []string     // "web/app: no memory request"
	NotCounted []notCounted // every other object in the file
	Prices     report.Prices
}

type predicted struct {
	Workload, Namespace string
	Replicas            int64
	Resources           report.Resources // requests of all replicas
	Monthly             float64          // 0 for a Job: the manifest does not say how long it runs
	Hourly              float64          `json:",omitempty"` // a Job's cost per hour of running
}

type notCounted struct {
	Object, Namespace string
	Reason            string `json:",omitempty"`
	Pods              bool   // it runs pods, so the total is too low
}

func (n notCounted) String() string {
	if n.Reason != "" {
		return n.Object + " (" + n.Reason + ")"
	}
	return n.Object
}

// predict prints what the manifests in a file would cost each month. It needs no cluster and sends nothing anywhere.
func predict(args []string, in io.Reader, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("predict", flag.ExitOnError)
	file := fs.String("f", "", "manifest file, YAML or JSON (- = stdin)")
	nodes := fs.Int64("nodes", 0, "number of nodes a DaemonSet runs on (without it, DaemonSets are not counted)")
	cpuHour := fs.Float64("cpu-hour", DefaultPrices.CPUHour, "price of one vCPU for one hour")
	gibHour := fs.Float64("gib-hour", DefaultPrices.GiBHour, "price of one GiB of memory for one hour")
	asJSON := fs.Bool("json", false, "print JSON instead of a table")
	fs.Parse(args)
	nodesSet := false
	fs.Visit(func(f *flag.Flag) { nodesSet = nodesSet || f.Name == "nodes" })
	switch {
	case *file == "" || fs.NArg() > 0:
		return errors.New("usage: phrontis predict -f file.yaml (- = stdin) [--nodes N] [--cpu-hour N] [--gib-hour N] [--json]")
	case *nodes < 0:
		return errors.New("--nodes is negative")
	case !(*cpuHour >= 0 && *gibHour >= 0): // also NaN
		return errors.New("--cpu-hour and --gib-hour must be 0 or more")
	}
	if !nodesSet {
		*nodes = -1
	}
	if *file != "-" {
		f, err := os.Open(*file)
		if err != nil {
			return err
		}
		defer f.Close()
		in = f
	}
	p := prediction{Workloads: []predicted{}, NoRequest: []string{}, NotCounted: []notCounted{}, Prices: report.Prices{CPUHour: *cpuHour, GiBHour: *gibHour}}
	if err := p.read(in, *nodes); err != nil {
		return err
	}
	if len(p.NotCounted) > 0 {
		names := make([]string, len(p.NotCounted))
		for i, n := range p.NotCounted {
			names[i] = n.String()
		}
		fmt.Fprintln(errOut, "not counted:", strings.Join(names, ", "))
	}
	if len(p.Workloads) == 0 {
		return errors.New("no Deployment, StatefulSet, DaemonSet or Job to count")
	}
	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(p)
	}
	p.print(out)
	return nil
}

// docEnd is YAML's end-of-document line: the parser drops what follows it, so it becomes a separator.
var docEnd = regexp.MustCompile(`(?m)^\.\.\.[ \t\r]*$`)

func (p *prediction) read(in io.Reader, nodes int64) error {
	b, err := io.ReadAll(in)
	if err != nil {
		return err
	}
	dec := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(docEnd.ReplaceAll(b, []byte("---"))), 4096)
	for n := 1; ; n++ {
		var raw json.RawMessage
		err := dec.Decode(&raw)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err == nil {
			err = p.add(raw, nodes)
		}
		if err != nil {
			return fmt.Errorf("document %d: %w", n, err)
		}
	}
}

// add counts one object; nodes is -1 without --nodes.
func (p *prediction) add(raw json.RawMessage, nodes int64) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var o struct {
		metav1.TypeMeta
		Metadata metav1.ObjectMeta `json:"metadata"`
		Items    []json.RawMessage `json:"items"`
	}
	if err := utiljson.Unmarshal(raw, &o); err != nil {
		return err
	}
	if o.Kind == "" {
		return errors.New("the object has no kind")
	}
	if o.Kind == "List" {
		for _, it := range o.Items {
			if err := p.add(it, nodes); err != nil {
				return err
			}
		}
		return nil
	}
	name, ns := cmp.Or(o.Metadata.Name, o.Metadata.GenerateName), cmp.Or(o.Metadata.Namespace, "default")
	skip := func(reason string, pods bool) error {
		p.NotCounted = append(p.NotCounted, notCounted{strings.TrimSuffix(o.Kind+"/"+name, "/"), ns, reason, pods})
		p.AtLeast = p.AtLeast || pods
		return nil
	}

	var pod corev1.PodSpec
	var err error
	one := int32(1) // the Kubernetes default
	replicas := &one
	switch o.APIVersion + " " + o.Kind {
	case "apps/v1 Deployment":
		var d appsv1.Deployment
		err = utiljson.Unmarshal(raw, &d)
		pod, replicas = d.Spec.Template.Spec, cmp.Or(d.Spec.Replicas, replicas)
	case "apps/v1 StatefulSet":
		var s appsv1.StatefulSet
		err = utiljson.Unmarshal(raw, &s)
		pod, replicas = s.Spec.Template.Spec, cmp.Or(s.Spec.Replicas, replicas)
	case "apps/v1 DaemonSet":
		var d appsv1.DaemonSet
		err = utiljson.Unmarshal(raw, &d)
		pod = d.Spec.Template.Spec
	case "batch/v1 Job":
		var j batchv1.Job
		err = utiljson.Unmarshal(raw, &j)
		pod, replicas = j.Spec.Template.Spec, cmp.Or(j.Spec.Parallelism, replicas)
	default:
		return skip("", hasPods(raw))
	}
	if err != nil {
		return fmt.Errorf("%s/%s: %w", o.Kind, name, err)
	}
	fail := func(msg string) error { return fmt.Errorf("%s/%s: %s", o.Kind, name, msg) }
	switch {
	case len(pod.Containers) == 0:
		return fail("no containers")
	case *replicas < 0 && o.Kind == "Job":
		return fail("parallelism is negative")
	case *replicas < 0:
		return fail("replicas is negative")
	}
	// The pod is checked before it is skipped, so a file error does not depend on --nodes.
	per, gaps, err := podRequests(pod)
	if err != nil {
		return fail(err.Error())
	}
	if pod.Resources != nil {
		return skip("pod-level resources are not supported", true)
	}
	n := int64(*replicas)
	if o.Kind == "DaemonSet" {
		if n = nodes; n < 0 {
			return skip("set --nodes", true)
		}
	}
	if n > 0 && max(per.MilliCPU, per.MemBytes) > math.MaxInt64/n {
		return fail("requests are too large")
	}
	w := predicted{Workload: o.Kind + "/" + name, Namespace: ns, Replicas: n,
		Resources: report.Resources{MilliCPU: per.MilliCPU * n, MemBytes: per.MemBytes * n}}
	if cost := p.Prices.Monthly(w.Resources); o.Kind == "Job" {
		w.Hourly = cost / report.HoursPerMonth
	} else {
		w.Monthly = cost
	}
	p.Monthly += w.Monthly
	p.Workloads = append(p.Workloads, w)
	for _, g := range gaps {
		p.NoRequest = append(p.NoRequest, name+"/"+g)
	}
	p.AtLeast = p.AtLeast || len(gaps) > 0
	return nil
}

// podRequests sums what the scheduler reserves for the pod while it runs, and names the requests that are missing or 0.
func podRequests(pod corev1.PodSpec) (sum report.Resources, gaps []string, err error) {
	for _, c := range append(sidecars(pod), pod.Containers...) {
		get := func(r corev1.ResourceName, label string, value func(resource.Quantity) int64) int64 {
			q, ok := c.Resources.Requests[r]
			if !ok {
				q, ok = c.Resources.Limits[r] // the Kubernetes default
			}
			switch {
			case q.Sign() < 0:
				err = fmt.Errorf("container %s: the %s request is negative", c.Name, label)
			case q.CmpInt64(1<<50) > 0: // far past any node; a larger Quantity overflows int64 in silence
				err = fmt.Errorf("container %s: the %s request is too large", c.Name, label)
			case !ok:
				gaps = append(gaps, c.Name+": no "+label+" request")
			case q.IsZero():
				gaps = append(gaps, c.Name+": the "+label+" request is 0")
			}
			return value(q)
		}
		sum.MilliCPU += get(corev1.ResourceCPU, "CPU", func(q resource.Quantity) int64 { return q.MilliValue() })
		sum.MemBytes += get(corev1.ResourceMemory, "memory", func(q resource.Quantity) int64 { return q.Value() })
		if err == nil && min(sum.MilliCPU, sum.MemBytes) < 0 {
			err = errors.New("requests are too large")
		}
		if err != nil {
			return sum, nil, err
		}
	}
	return sum, gaps, nil
}

// hasPods says whether an object predict does not count runs pods.
func hasPods(raw json.RawMessage) bool {
	var o map[string]any
	if utiljson.Unmarshal(raw, &o) != nil {
		return false
	}
	return objHasPods(o)
}

func objHasPods(o map[string]any) bool {
	items, _, _ := unstructured.NestedSlice(o, "items") // a typed list, such as DeploymentList
	if slices.ContainsFunc(items, func(it any) bool { m, _ := it.(map[string]any); return objHasPods(m) }) {
		return true
	}
	for _, path := range [][]string{
		{"spec", "containers"}, // Pod
		{"spec", "template", "spec", "containers"},
		{"spec", "jobTemplate", "spec", "template", "spec", "containers"}, // CronJob
	} {
		if _, ok, _ := unstructured.NestedFieldNoCopy(o, path...); ok {
			return true
		}
	}
	return false
}

func (p prediction) print(out io.Writer) {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "WORKLOAD\tNAMESPACE\tREPLICAS\tCPU\tMEMORY\t$/MO\t$/HOUR (JOB)")
	for _, x := range p.Workloads {
		mo, hour := fmt.Sprintf("%.2f", x.Monthly), ""
		if strings.HasPrefix(x.Workload, "Job/") {
			mo, hour = "", fmt.Sprintf("%.4f", x.Hourly)
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%dm\t%dMi\t%s\t%s\n", x.Workload, x.Namespace, x.Replicas, x.Resources.MilliCPU, (x.Resources.MemBytes+(1<<20)-1)>>20, mo, hour)
	}
	w.Flush()
	atLeast := ""
	if p.AtLeast {
		atLeast = "at least "
	}
	fmt.Fprintf(out, "\ntotal  %s%.2f/month\n", atLeast, p.Monthly)
	for _, g := range p.NoRequest {
		fmt.Fprintln(out, g)
	}
	fmt.Fprintf(out, "flat prices: %g per vCPU-hour, %g per GiB-hour; requests only; init containers, storage, network and LimitRange defaults are not counted\n", p.Prices.CPUHour, p.Prices.GiBHour)
}
