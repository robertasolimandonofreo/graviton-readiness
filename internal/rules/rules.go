// Package rules decides, from a workload's configuration and what the registry says about its
// images, whether the workload can move to Graviton (arm64) nodes.
package rules

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/robertasolimandonofreo/graviton-readiness/internal/image"
	"github.com/robertasolimandonofreo/graviton-readiness/internal/inventory"
)

// Verdict is the overall answer for a workload.
type Verdict string

const (
	// Ready means no finding blocks it and every image is known to run on arm64.
	Ready Verdict = "ready"
	// ReadyWithCaveats is Ready with something to look at first.
	ReadyWithCaveats Verdict = "ready-with-caveats"
	// NotReady means at least one blocker.
	NotReady Verdict = "not-ready"
	// Unverified means nothing blocks it in the configuration, but the images were not (or could
	// not be) checked, so arm64 support is not confirmed.
	Unverified Verdict = "unverified"
)

// Finding is one rule that matched.
type Finding struct {
	Rule     string   `json:"rule"`
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
}

// ImageStatus is what is known about one image of the workload.
type ImageStatus struct {
	Container string `json:"container"`
	Image     string `json:"image"`
	Status    string `json:"status"`
	Detail    string `json:"detail,omitempty"`
}

// Assessment is the result for one workload.
type Assessment struct {
	Workload inventory.Workload `json:"-"`
	Score    int                `json:"score"`
	Verdict  Verdict            `json:"verdict"`
	Findings []Finding          `json:"findings"`
	Images   []ImageStatus      `json:"images,omitempty"`
}

// Score weights: a blocker costs a lot, a caveat a little, info nothing.
const (
	blockerCost = 40
	caveatCost  = 10
)

const (
	archKey     = "kubernetes.io/arch"
	archKeyBeta = "beta.kubernetes.io/arch"
	osKey       = "kubernetes.io/os"
	osKeyBeta   = "beta.kubernetes.io/os"
)

var instanceTypeKeys = []string{"node.kubernetes.io/instance-type", "beta.kubernetes.io/instance-type"}

const instanceFamilyKey = "karpenter.k8s.aws/instance-family"

// gravitonType matches the EC2 instance types and families that run on Graviton: a letter prefix,
// a generation, a "g" (m7g, c6gn, x2gd, im4gn, is4gen, g5g, hpc7g), or the first generation a1.
var gravitonType = regexp.MustCompile(`^(a1|[a-z]+[0-9]+g[a-z]*)(\.|$)`)

var x86Hint = regexp.MustCompile(`(?i)(amd64|x86[_-]64)`)

// Options tune an evaluation.
type Options struct {
	// ImagesChecked says whether the registry was consulted. Without it, images are unverified.
	ImagesChecked bool
}

// Evaluate applies every rule to a workload. images maps an image reference to what the registry
// said; it is empty when images were not checked.
func Evaluate(w inventory.Workload, images map[string]image.Result, opts Options) Assessment {
	a := Assessment{Workload: w, Findings: []Finding{}}
	add := func(rule string, sev Severity, format string, args ...any) {
		a.Findings = append(a.Findings, Finding{Rule: rule, Severity: sev, Message: fmt.Sprintf(format, args...)})
	}

	// Scheduling constraints.
	if !allowsArch(w.Pod, "arm64") {
		add("GR001", Blocker, "pinned to amd64: %s", archConstraint(w.Pod))
	}
	if what := pinnedToX86Types(w.Pod); what != "" {
		add("GR002", Blocker, "only x86 instance types allowed: %s", what)
	}
	if targetsWindows(w.Pod) {
		add("GR009", Blocker, "targets Windows nodes, which Graviton does not have")
	}
	pinned := !allowsArch(w.Pod, "arm64")

	// Images.
	unverified := false
	for _, c := range w.Containers() {
		status := ImageStatus{Container: c.Name, Image: c.Image}
		if what := namesAmd64(c.Image); what != "" {
			add("GR003", Blocker, "container %q: image %s names an amd64 build (%s)", c.Name, c.Image, what)
		}
		switch res, ok := images[c.Image]; {
		case !opts.ImagesChecked:
			status.Status = "not-checked"
			unverified = true
		case !ok || res.Status == image.Unknown:
			status.Status, status.Detail = string(image.Unknown), res.Describe()
			unverified = true
			add("GR005", Info, "container %q: image %s: %s", c.Name, c.Image, res.Describe())
		case res.Status == image.NoArm64:
			status.Status, status.Detail = string(image.NoArm64), res.Describe()
			msg := "container %q: image %s has no linux/arm64 manifest (has %s)"
			if !pinned {
				msg += "; it is not pinned to amd64, so it can land on an arm64 node and fail"
			}
			add("GR004", Blocker, msg, c.Name, c.Image, strings.Join(res.Platforms, ", "))
		default:
			status.Status = string(image.Arm64)
		}
		a.Images = append(a.Images, status)

		if hint := x86Mention(c); hint != "" {
			add("GR006", Caveat, "container %q mentions %q in its command, args or environment", c.Name, hint)
		}
		if requestsGPU(c) {
			add("GR007", Caveat, "container %q requests a GPU", c.Name)
		}
	}
	if w.Kind == "DaemonSet" {
		add("GR008", Info, "runs on every node, so its images must be multi-arch before any Graviton node joins")
	}

	sortFindings(a.Findings)
	a.Score, a.Verdict = verdict(a.Findings, unverified)
	return a
}

func verdict(findings []Finding, unverified bool) (int, Verdict) {
	score, blockers, caveats := 100, 0, 0
	for _, f := range findings {
		switch f.Severity {
		case Blocker:
			blockers++
			score -= blockerCost
		case Caveat:
			caveats++
			score -= caveatCost
		}
	}
	if score < 0 {
		score = 0
	}
	switch {
	case blockers > 0:
		return score, NotReady
	case unverified:
		return score, Unverified
	case caveats > 0:
		return score, ReadyWithCaveats
	}
	return score, Ready
}

func sortFindings(f []Finding) {
	rank := map[Severity]int{Blocker: 0, Caveat: 1, Info: 2}
	sort.SliceStable(f, func(i, j int) bool {
		if rank[f[i].Severity] != rank[f[j].Severity] {
			return rank[f[i].Severity] < rank[f[j].Severity]
		}
		return f[i].Rule < f[j].Rule
	})
}

// ImageRefs lists the distinct image references of the workloads, in first-seen order.
func ImageRefs(ws []inventory.Workload) []string {
	seen := map[string]bool{}
	var out []string
	for _, w := range ws {
		for _, c := range w.Containers() {
			if c.Image != "" && !seen[c.Image] {
				seen[c.Image] = true
				out = append(out, c.Image)
			}
		}
	}
	return out
}

// allowsArch reports whether the pod can be scheduled on a node of the given architecture: the
// node selector must not name another one, and at least one required affinity term must allow it.
func allowsArch(pod corev1.PodSpec, arch string) bool {
	for _, key := range []string{archKey, archKeyBeta} {
		if v, ok := pod.NodeSelector[key]; ok && v != arch {
			return false
		}
	}
	terms := requiredTerms(pod)
	if len(terms) == 0 {
		return true
	}
	for _, term := range terms {
		if termAllows(term, arch) {
			return true
		}
	}
	return false
}

func requiredTerms(pod corev1.PodSpec) []corev1.NodeSelectorTerm {
	if pod.Affinity == nil || pod.Affinity.NodeAffinity == nil {
		return nil
	}
	sel := pod.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if sel == nil {
		return nil
	}
	return sel.NodeSelectorTerms
}

// termAllows is true when every architecture expression of the term lets the arch through (the
// expressions of one term are ANDed).
func termAllows(term corev1.NodeSelectorTerm, arch string) bool {
	for _, e := range term.MatchExpressions {
		if e.Key != archKey && e.Key != archKeyBeta {
			continue
		}
		has := contains(e.Values, arch)
		switch e.Operator {
		case corev1.NodeSelectorOpIn:
			if !has {
				return false
			}
		case corev1.NodeSelectorOpNotIn:
			if has {
				return false
			}
		case corev1.NodeSelectorOpDoesNotExist:
			return false
		}
	}
	return true
}

// archConstraint describes where the pin comes from, for the message.
func archConstraint(pod corev1.PodSpec) string {
	for _, key := range []string{archKey, archKeyBeta} {
		if v, ok := pod.NodeSelector[key]; ok {
			return fmt.Sprintf("nodeSelector %s=%s", key, v)
		}
	}
	return "required nodeAffinity on " + archKey
}

// pinnedToX86Types describes an instance-type pin that excludes every Graviton type, or "".
func pinnedToX86Types(pod corev1.PodSpec) string {
	for _, key := range instanceTypeKeys {
		if v, ok := pod.NodeSelector[key]; ok && !gravitonType.MatchString(v) {
			return fmt.Sprintf("nodeSelector %s=%s", key, v)
		}
	}
	terms := requiredTerms(pod)
	if len(terms) == 0 {
		return ""
	}
	// Terms are ORed: the pod is pinned to x86 only when every term is.
	var seen []string
	for _, term := range terms {
		what := ""
		for _, e := range term.MatchExpressions {
			if e.Operator != corev1.NodeSelectorOpIn || len(e.Values) == 0 {
				continue
			}
			if !contains(instanceTypeKeys, e.Key) && e.Key != instanceFamilyKey {
				continue
			}
			if !anyGraviton(e.Values) {
				what = fmt.Sprintf("%s in [%s]", e.Key, strings.Join(e.Values, ", "))
			}
		}
		if what == "" {
			return ""
		}
		seen = append(seen, what)
	}
	return "required nodeAffinity " + strings.Join(seen, " or ")
}

func anyGraviton(values []string) bool {
	for _, v := range values {
		if gravitonType.MatchString(v) {
			return true
		}
	}
	return false
}

func targetsWindows(pod corev1.PodSpec) bool {
	for _, key := range []string{osKey, osKeyBeta} {
		if pod.NodeSelector[key] == "windows" {
			return true
		}
	}
	return false
}

// namesAmd64 reports how an image reference says it is amd64-only: an arch tag, or an amd64
// repository path such as Docker Hub's amd64/ubuntu. Digests are ignored.
func namesAmd64(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	path, tag := ref, ""
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		path, tag = ref[:i], ref[i+1:]
	}
	for _, seg := range strings.Split(path, "/") {
		if s := strings.ToLower(seg); s == "amd64" || s == "x86_64" {
			return "repository " + seg
		}
	}
	if t := strings.ToLower(tag); x86Hint.MatchString(t) && !strings.Contains(t, "arm64") && !strings.Contains(t, "aarch64") {
		return "tag " + tag
	}
	return ""
}

// x86Mention returns the first architecture-looking token in a container's command, args or env.
func x86Mention(c corev1.Container) string {
	texts := append(append([]string{}, c.Command...), c.Args...)
	for _, e := range c.Env {
		texts = append(texts, e.Value)
	}
	for _, t := range texts {
		if m := x86Hint.FindString(t); m != "" {
			return m
		}
	}
	return ""
}

func requestsGPU(c corev1.Container) bool {
	for _, list := range []corev1.ResourceList{c.Resources.Limits, c.Resources.Requests} {
		for name := range list {
			if strings.HasSuffix(string(name), "/gpu") {
				return true
			}
		}
	}
	return false
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
