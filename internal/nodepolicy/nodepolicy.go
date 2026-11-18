// Package nodepolicy renders a Phrontis node policy (spec 010) as the Karpenter NodePool and EC2NodeClass that
// would carry it out. It only renders: nothing here talks to a cluster. Fields follow the CRDs of KarpenterVersion,
// vendored unchanged in testdata/crds with their source, and the docs at https://karpenter.sh/v1.14/concepts/.
package nodepolicy

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
	"k8s.io/apimachinery/pkg/util/validation"
)

// KarpenterVersion is the one Karpenter release the objects are rendered for: an LTS release, supported until
// July 2027. Bumping it means re-vendoring testdata/crds from the new tag and re-running TestServerDryRun.
const KarpenterVersion = "1.14.3"

// Policy is the node policy the owner sets (spec 010 scenario 2, screen na-policy). The yaml names are the policy
// file.
type Policy struct {
	Name      string   `yaml:"name"`      // NodePool name
	NodeClass string   `yaml:"nodeClass"` // name of the EC2NodeClass the pool launches nodes from
	Families  []string `yaml:"families"`  // instance families, e.g. m6i; at least one, so GPU, burstable and metal families stay out
	Arch      []string `yaml:"arch"`      // amd64 and/or arm64
	// Spot adds a spot-only NodePool <name>-spot next to the on-demand one: only pods with the webhook's opt-in
	// toleration land on it, and they fall back to on-demand while spot has no capacity.
	Spot     bool `yaml:"spot"`
	CPULimit int  `yaml:"cpuLimit"` // vCPU cap of each NodePool, so with Spot the policy may reach twice it; 0: no cap
	// DrainPercent is the most of the pool's nodes Karpenter may drain at once, in %; 0: DefaultDrainPercent.
	DrainPercent int    `yaml:"drainPercent"`
	Window       Window `yaml:"window"` // when drains are allowed (spec 010 scenario 3); zero: at any time
	// ExpireAfter is the most a node lives, e.g. 720h, or Never; "": DefaultExpireAfter. Expiry is forceful: it
	// ignores the drain budget and the window (https://karpenter.sh/v1.14/concepts/disruption/#expiration).
	ExpireAfter string `yaml:"expireAfter"`
}

// CapacityType is the node label Karpenter sets to spot, on-demand or reserved
// (https://karpenter.sh/v1.14/concepts/scheduling/#well-known-labels). The webhook prefers spot by it.
const CapacityType = "karpenter.sh/capacity-type"

const SpotSuffix = "-spot" // name suffix of the spot-only NodePool

// DefaultDrainPercent is the disruption budget of spec 010; Karpenter's own default is the same 10%.
const DefaultDrainPercent = 10

// DefaultExpireAfter is Karpenter's own default (CRD spec.template.spec.expireAfter), so writing it changes nothing.
const DefaultExpireAfter = "720h"

// Window is the owner's maintenance window: Karpenter drains nodes only while it is open (screen na-budget).
type Window struct {
	Schedule string        `yaml:"schedule"` // when it opens, cron "minute hour * * day-of-week" in UTC (Karpenter has no timezones), e.g. "0 1 * * 1-5"
	Duration time.Duration `yaml:"duration"` // how long it stays open, in whole minutes
}

const day, week = 24 * 60, 7 * 24 * 60 // minutes

// opens returns the minute of the day the window opens at and the minutes of the week (from Sunday 00:00 UTC) it
// opens at. Only a fixed time on chosen weekdays is accepted: that repeats every week, so the time the window is
// shut can be written as Karpenter budgets.
func (w Window) opens() (int, []int, error) {
	f := strings.Fields(w.Schedule)
	bad := fmt.Errorf(`window schedule %q: use cron "minute hour * * day-of-week" with day numbers 0-6 (0 is Sunday), e.g. "0 1 * * 1-5"`, w.Schedule)
	if len(f) != 5 || f[2] != "*" || f[3] != "*" {
		return 0, nil, bad
	}
	minute, err1 := num(f[0])
	hour, err2 := num(f[1])
	if err1 != nil || err2 != nil || minute < 0 || minute > 59 || hour < 0 || hour > 23 {
		return 0, nil, bad
	}
	items := strings.Split(f[4], ",")
	if f[4] == "*" {
		items = []string{"0-6"}
	}
	days := make([]bool, 7)
	for _, item := range items {
		lo, hi, isRange := strings.Cut(item, "-")
		a, err := num(lo)
		b := a
		if isRange && err == nil {
			b, err = num(hi)
		}
		if err != nil || a < 0 || b > 6 || a > b {
			return 0, nil, bad
		}
		for d := a; d <= b; d++ {
			days[d] = true
		}
	}
	at := hour*60 + minute
	var starts []int
	for d, on := range days {
		if on {
			starts = append(starts, d*day+at)
		}
	}
	return at, starts, nil
}

// num reads a cron number: digits only, because Atoi also takes "+5" and "-0".
func num(s string) (int, error) {
	if s == "" || strings.Trim(s, "0123456789") != "" {
		return 0, strconv.ErrSyntax
	}
	return strconv.Atoi(s)
}

// shut renders the budgets that stop every drain while the window is shut: nodes 0 from each closing to the next
// opening. Karpenter applies the most restrictive active budget (CRD spec.disruption.budgets), so a budget can only
// hold drains back, never allow them: "only inside the window" has to be written as "none outside it".
func (w Window) shut() ([]m, error) {
	if w == (Window{}) {
		return nil, nil
	}
	if w.Duration <= 0 || w.Duration%time.Minute != 0 {
		return nil, fmt.Errorf("window length %s: use a positive number of whole minutes", w.Duration)
	}
	at, starts, err := w.opens()
	if err != nil {
		return nil, err
	}
	open := int(w.Duration / time.Minute)
	closeAt := (at + open) % day
	var gaps []int
	days := map[int][]string{} // length of a shut spell -> weekdays it starts on
	for i, s := range starts {
		next := starts[(i+1)%len(starts)]
		if next <= s {
			next += week
		}
		gap := next - (s + open)
		if gap <= 0 { // opens again before it closes
			continue
		}
		if days[gap] == nil {
			gaps = append(gaps, gap)
		}
		days[gap] = append(days[gap], strconv.Itoa((s+open)/day%7))
	}
	var budgets []m
	for _, g := range gaps {
		slices.Sort(days[g])
		dow := strings.Join(days[g], ",")
		if len(days[g]) == 7 {
			dow = "*"
		}
		// The CRD's duration takes hours and minutes only; its pattern allows the trailing 0s of Duration.String.
		budgets = append(budgets, m{"nodes": "0", "schedule": fmt.Sprintf("%d %d * * %s", closeAt%60, closeAt/60, dow),
			"duration": strings.TrimSuffix((time.Duration(g) * time.Minute).String(), "0s")})
	}
	return budgets, nil
}

// NodeClass is the AWS side of the EC2NodeClass. It holds names only, never cloud keys: the role lives in the
// customer's account.
type NodeClass struct {
	Cluster string `yaml:"cluster"` // EKS cluster name; subnets and security groups are found by their karpenter.sh/discovery tag
	Role    string `yaml:"role"`    // IAM role name the nodes use
	AMI     string `yaml:"ami"`     // EKS AMI alias pinned to a release, e.g. al2023@v20240625
}

type m = map[string]any // yaml.v3 sorts map keys, so the output is stable

var (
	family = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	// Accelerated computing families (GPU, Inferentia, Trainium, FPGA, video), which spec 010 leaves out, per
	// https://docs.aws.amazon.com/ec2/latest/instancetypes/ac.html, checked 2026-11-15.
	accelerated = regexp.MustCompile(`^(dl|f|gr?|inf|p|trn|vt)[0-9]`)
	// Tag value characters allowed across AWS services (https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/Using_Tags.html,
	// checked 2026-11-15). They exclude '*', which a Karpenter selector reads as "any value".
	tagValue = regexp.MustCompile(`^[a-zA-Z0-9 +=._:/@-]{1,256}$`)
	// "latest" is refused because each new AMI release would replace every node (drift).
	pinnedAMI = regexp.MustCompile(`^v[0-9][0-9.]*$`)
	// The pattern of spec.template.spec.expireAfter in the pinned NodePool CRD.
	expireAfter = regexp.MustCompile(`^(([0-9]+(s|m|h))+|Never)$`)
)

func (p Policy) validate() error {
	var errs []error
	for _, n := range []string{p.Name, p.NodeClass} {
		for _, e := range validation.IsDNS1123Label(n) {
			errs = append(errs, fmt.Errorf("name %q: %s", n, e))
		}
	}
	if p.Spot && len(p.Name+SpotSuffix) > validation.DNS1123LabelMaxLength {
		errs = append(errs, fmt.Errorf("name %q: at most %d characters with spot, so the spot pool's name fits", p.Name, validation.DNS1123LabelMaxLength-len(SpotSuffix)))
	}
	if len(p.Families) == 0 {
		errs = append(errs, errors.New("pick at least one instance family"))
	}
	for _, f := range p.Families {
		if !family.MatchString(f) {
			errs = append(errs, fmt.Errorf("instance family %q is not a family name like m6i", f))
		} else if accelerated.MatchString(f) {
			errs = append(errs, fmt.Errorf("instance family %q is GPU or other accelerated computing, which node policies leave out", f))
		}
	}
	if len(p.Arch) == 0 {
		errs = append(errs, errors.New("pick at least one architecture"))
	}
	for _, a := range p.Arch {
		if a != "amd64" && a != "arm64" {
			errs = append(errs, fmt.Errorf("architecture %q: use amd64 or arm64", a))
		}
	}
	if p.CPULimit < 0 {
		errs = append(errs, fmt.Errorf("CPU limit %d is negative", p.CPULimit))
	}
	if p.DrainPercent < 0 || p.DrainPercent > 100 {
		errs = append(errs, fmt.Errorf("drain budget %d%%: use 1 to 100 percent of nodes, or 0 for the default %d%%", p.DrainPercent, DefaultDrainPercent))
	}
	if e := p.ExpireAfter; e != "" {
		if d, err := time.ParseDuration(e); !expireAfter.MatchString(e) || (e != "Never" && (err != nil || d <= 0)) {
			errs = append(errs, fmt.Errorf("expireAfter %q: use a positive duration like 720h, or Never", e))
		}
	}
	if _, err := p.Window.shut(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (c NodeClass) validate() error {
	var errs []error
	if !tagValue.MatchString(c.Cluster) {
		errs = append(errs, fmt.Errorf("EKS cluster name %q: use letters, digits, spaces and + - = . _ : / @ (no '*', which would match every cluster's subnets)", c.Cluster))
	}
	if c.Role == "" {
		errs = append(errs, errors.New("node IAM role is empty"))
	}
	// Linux families from the CRD: its Windows aliases accept only latest, so they cannot be pinned.
	if fam, ver, _ := strings.Cut(c.AMI, "@"); !slices.Contains([]string{"al2", "al2023", "bottlerocket"}, fam) {
		errs = append(errs, fmt.Errorf("AMI family %q: use al2, al2023 or bottlerocket; Windows AMIs cannot be pinned", fam))
	} else if !pinnedAMI.MatchString(ver) {
		errs = append(errs, fmt.Errorf("AMI %q: pin a release like al2023@v20240625, not latest", c.AMI))
	}
	return errors.Join(errs...)
}

// nodePools renders the on-demand NodePool and, with Spot, the spot-only one. A spot-only pool never falls back on its
// own, hence two pools: Karpenter tries pools by weight (https://karpenter.sh/v1.14/concepts/nodepools/#specweight) and
// relaxes a pod's preferred spot affinity when no pool can meet it (/v1.14/concepts/scheduling/#preferences, /v1.14/faq/).
func (p Policy) nodePools() []m {
	pools := []m{p.nodePool(p.Name, "on-demand")}
	if p.Spot {
		pools = append(pools, p.nodePool(p.Name+SpotSuffix, "spot"))
	}
	return pools
}

func (p Policy) nodePool(name, capacity string) m {
	shut, _ := p.Window.shut() // checked by validate
	tmpl := m{
		"nodeClassRef": m{"group": "karpenter.k8s.aws", "kind": "EC2NodeClass", "name": p.NodeClass},
		"expireAfter":  cmp.Or(p.ExpireAfter, DefaultExpireAfter), // written so the owner sees it in the diff
		"requirements": []m{
			{"key": "karpenter.k8s.aws/instance-family", "operator": "In", "values": slices.Clone(p.Families)},
			{"key": "kubernetes.io/arch", "operator": "In", "values": slices.Clone(p.Arch)},
			{"key": CapacityType, "operator": "In", "values": []string{capacity}},
		},
	}
	spec := m{
		"template": m{"spec": tmpl},
		// consolidateAfter is required once disruption is set; 1m is the docs' example. The drain budget is always
		// written, though it is Karpenter's default, so the owner sees it in the diff.
		"disruption": m{"consolidationPolicy": "WhenEmptyOrUnderutilized", "consolidateAfter": "1m",
			"budgets": append([]m{{"nodes": strconv.Itoa(cmp.Or(p.DrainPercent, DefaultDrainPercent)) + "%"}}, shut...)},
	}
	if p.CPULimit > 0 {
		spec["limits"] = m{"cpu": strconv.Itoa(p.CPULimit)}
	}
	if capacity == "spot" {
		// Karpenter does not taint spot nodes itself; pods that don't tolerate this taint (the webhook's opt-in one)
		// never get a node from this pool.
		tmpl["taints"] = []m{{"key": CapacityType, "value": "spot", "effect": "NoSchedule"}}
	} else if p.Spot {
		// On-demand goes first, so a pod tolerating every taint without opting in stays on it. An opted-in pod still
		// gets spot first: Karpenter holds its preferred spot affinity as required until no pool can meet it.
		spec["weight"] = 1
	}
	return m{"apiVersion": "karpenter.sh/v1", "kind": "NodePool", "metadata": m{"name": name}, "spec": spec}
}

// SpotOff renders <name>-spot for a policy that turned spot off, in the form that empties it within the pool's drain
// budget and window. limits.cpu 0 launches no new node, and the capacity type on-demand, which its spot nodes lack,
// drifts them: Karpenter checks spec.template.spec.requirements against each NodeClaim
// (https://karpenter.sh/v1.14/concepts/disruption/#drift) and replaces a drifted node first, on the on-demand pool.
// Deleting the pool instead would drain every node at once: budgets "only block" drift, emptiness and consolidation
// (/v1.14/concepts/disruption/#nodepool-disruption-budgets). A name too long for a spot pool renders nothing.
func SpotOff(p Policy) ([]byte, error) {
	if err := p.validate(); err != nil || len(p.Name+SpotSuffix) > validation.DNS1123LabelMaxLength {
		return nil, err
	}
	np := p.nodePool(p.Name+SpotSuffix, "spot")
	spec := np["spec"].(m)
	spec["limits"] = m{"cpu": "0"}
	reqs := spec["template"].(m)["spec"].(m)["requirements"].([]m)
	reqs[len(reqs)-1]["values"] = []string{"on-demand"} // the capacity type, last
	return encode(np)
}

func (c NodeClass) ec2NodeClass(name string) m {
	discovery := []m{{"tags": m{"karpenter.sh/discovery": c.Cluster}}}
	return m{"apiVersion": "karpenter.k8s.aws/v1", "kind": "EC2NodeClass", "metadata": m{"name": name}, "spec": m{
		"role":                       c.Role,
		"amiSelectorTerms":           []m{{"alias": c.AMI}},
		"subnetSelectorTerms":        discovery,
		"securityGroupSelectorTerms": discovery,
	}}
}

// NodePool renders only the policy's NodePools, for a cluster that already has its EC2NodeClass.
func NodePool(p Policy) ([]byte, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	return encode(p.nodePools()...)
}

// Render returns the policy's NodePools and the EC2NodeClass they launch from (named p.NodeClass) as one YAML stream.
func Render(p Policy, c NodeClass) ([]byte, error) {
	if err := errors.Join(p.validate(), c.validate()); err != nil {
		return nil, err
	}
	return encode(append(p.nodePools(), c.ec2NodeClass(p.NodeClass))...)
}

func encode(docs ...m) ([]byte, error) {
	var b bytes.Buffer
	e := yaml.NewEncoder(&b)
	e.SetIndent(2)
	for _, d := range docs {
		if err := e.Encode(d); err != nil {
			return nil, err
		}
	}
	if err := e.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
