package rules

import (
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/robertasolimandonofreo/graviton-readiness/internal/inventory"
)

const nodePoolKey = "karpenter.sh/nodepool"

// PoolArch says which architectures a NodePool can provision.
type PoolArch struct {
	AMD64, ARM64 bool
	// Why explains a restriction, for the message. Empty when the pool is not restricted.
	Why string
}

// poolArch works out the architectures of a NodePool from what the manifest proves: an explicit
// kubernetes.io/arch requirement, or instance types and families that are all x86 (or all
// Graviton). A pool that says neither is not assumed to be amd64: the Karpenter documentation
// does not promise a default, so it counts as allowing both and the rule stays quiet.
func poolArch(p inventory.NodePool) PoolArch {
	a := PoolArch{AMD64: true, ARM64: true}
	var reasons []string
	for _, r := range p.Requirements {
		switch {
		case r.Key == archKey || r.Key == archKeyBeta:
			before := a
			for _, arch := range []string{"amd64", "arm64"} {
				has := contains(r.Values, arch)
				ok := true
				switch r.Operator {
				case "In":
					ok = has
				case "NotIn":
					ok = !has
				case "DoesNotExist":
					ok = false
				}
				if !ok {
					if arch == "amd64" {
						a.AMD64 = false
					} else {
						a.ARM64 = false
					}
				}
			}
			if a != before {
				if r.Reason != "" {
					reasons = append(reasons, r.Reason)
				} else {
					reasons = append(reasons, fmt.Sprintf("%s %s [%s]", r.Key, r.Operator, strings.Join(r.Values, ", ")))
				}
			}
		case r.Operator == "In" && len(r.Values) > 0 && (contains(instanceTypeKeys, r.Key) || r.Key == instanceFamilyKey):
			var graviton, x86 bool
			for _, v := range r.Values {
				if gravitonType.MatchString(v) {
					graviton = true
				} else {
					x86 = true
				}
			}
			if !graviton && a.ARM64 {
				a.ARM64 = false
				reasons = append(reasons, fmt.Sprintf("%s lists only x86 types [%s]", r.Key, strings.Join(r.Values, ", ")))
			}
			if !x86 && a.AMD64 {
				a.AMD64 = false
			}
		}
	}
	a.Why = strings.Join(reasons, "; ")
	return a
}

// poolValue is the value a pool gives a label key: its name for karpenter.sh/nodepool, or the
// label of the template.
func poolValue(p inventory.NodePool, key string) (string, bool) {
	if key == nodePoolKey {
		return p.Name, !p.LabelOnly
	}
	v, ok := p.Labels[key]
	return v, ok
}

// resolvable reports whether some pool can answer for the key. Other keys (zone, capacity type,
// a label that lives on a managed node group) say nothing about the pool.
func resolvable(pools []inventory.NodePool, key string) bool {
	for _, p := range pools {
		if key == nodePoolKey && !p.LabelOnly {
			return true // some pool has a name that the selector can match
		}
		if _, ok := p.Labels[key]; ok {
			return true
		}
	}
	return false
}

func filterPools(pools []inventory.NodePool, keep func(inventory.NodePool) bool) []inventory.NodePool {
	var out []inventory.NodePool
	for _, p := range pools {
		if keep(p) {
			out = append(out, p)
		}
	}
	return out
}

// selectedPools finds the NodePools that the pod's node selector and required node affinity can
// land on. resolved is false when nothing in the pod points at a pool, which is the common case
// and means no statement about a pool can be made.
func selectedPools(pod corev1.PodSpec, pools []inventory.NodePool) (matched []inventory.NodePool, resolved bool) {
	matched = pools
	keys := make([]string, 0, len(pod.NodeSelector))
	for k := range pod.NodeSelector {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !resolvable(pools, k) {
			continue
		}
		resolved = true
		want := pod.NodeSelector[k]
		matched = filterPools(matched, func(p inventory.NodePool) bool {
			v, ok := poolValue(p, k)
			return ok && v == want
		})
	}

	terms := requiredTerms(pod)
	if len(terms) == 0 {
		return matched, resolved
	}
	// Terms are ORed. A term that says nothing about a pool lets every candidate through.
	var union []inventory.NodePool
	seen := map[string]bool{}
	anyTerm := false
	for _, term := range terms {
		sub, termResolved := matched, false
		for _, e := range term.MatchExpressions {
			if !resolvable(pools, e.Key) {
				continue
			}
			termResolved = true
			expr := e
			sub = filterPools(sub, func(p inventory.NodePool) bool {
				v, ok := poolValue(p, expr.Key)
				switch expr.Operator {
				case corev1.NodeSelectorOpIn:
					return ok && contains(expr.Values, v)
				case corev1.NodeSelectorOpNotIn:
					return !ok || !contains(expr.Values, v)
				case corev1.NodeSelectorOpExists:
					return ok
				case corev1.NodeSelectorOpDoesNotExist:
					return !ok
				}
				return true
			})
		}
		anyTerm = anyTerm || termResolved
		for _, p := range sub {
			if !seen[p.Name] {
				seen[p.Name] = true
				union = append(union, p)
			}
		}
	}
	if anyTerm {
		return union, true
	}
	return matched, resolved
}

// UsesNodePoolSelector reports whether the pod picks nodes by the karpenter.sh/nodepool label, in
// its node selector or in a required node affinity. The report uses it to say that no NodePool was
// found when the workloads clearly expect one.
func UsesNodePoolSelector(pod corev1.PodSpec) bool {
	if _, ok := pod.NodeSelector[nodePoolKey]; ok {
		return true
	}
	for _, term := range requiredTerms(pod) {
		for _, e := range term.MatchExpressions {
			if e.Key == nodePoolKey {
				return true
			}
		}
	}
	return false
}

// poolFindings checks the pools a workload selects. It says nothing when the pod does not point
// at a pool, or when the pool is not among the scanned manifests and the pod points at it only by
// a label that no pool carries.
func poolFindings(pod corev1.PodSpec, pools []inventory.NodePool) (rule string, sev Severity, msg string) {
	if len(pools) == 0 {
		return "", "", ""
	}
	matched, resolved := selectedPools(pod, pools)
	if !resolved {
		return "", "", ""
	}
	if len(matched) == 0 {
		return "GR011", Info, "selects a NodePool that is not among the scanned manifests, so its architecture is unknown"
	}
	var names []string
	var why []string
	for _, p := range matched {
		a := poolArch(p)
		if a.ARM64 {
			return "", "", "" // at least one selected pool can provision arm64
		}
		names = append(names, p.Name)
		if a.Why != "" {
			why = append(why, p.Name+": "+a.Why)
		}
	}
	return "GR010", Blocker, fmt.Sprintf("selects NodePool %s, which only provisions amd64 (%s)", strings.Join(names, ", "), strings.Join(why, "; "))
}

// wellKnownPrefixes are label keys that Kubernetes, the cloud or Karpenter set on every node. A
// selector on one of them says nothing about which pool the pod wants.
var wellKnownPrefixes = []string{
	"kubernetes.io/", "beta.kubernetes.io/", "node.kubernetes.io/", "topology.kubernetes.io/",
	"failure-domain.beta.kubernetes.io/", "karpenter.sh/", "karpenter.k8s.aws/",
}

func customKey(key string) bool {
	for _, p := range wellKnownPrefixes {
		if strings.HasPrefix(key, p) {
			return false
		}
	}
	return true
}

// PoolUnknown reports whether the pod chooses its nodes by a label (or by karpenter.sh/nodepool)
// but no known pool answers for it, so the architecture of those nodes was not checked. That is
// the case when the pools are not Karpenter's (EKS managed node groups, GKE or AKS pools), or live
// outside the scanned folder.
func PoolUnknown(pod corev1.PodSpec, pools []inventory.NodePool) bool {
	picks := UsesNodePoolSelector(pod)
	for k := range pod.NodeSelector {
		picks = picks || customKey(k)
	}
	for _, term := range requiredTerms(pod) {
		for _, e := range term.MatchExpressions {
			picks = picks || customKey(e.Key)
		}
	}
	if !picks {
		return false
	}
	if len(pools) == 0 {
		return true
	}
	_, resolved := selectedPools(pod, pools)
	return !resolved
}

// ParsePool turns "key=value:arch" into a pool, for node pools that are not Karpenter NodePools
// (an EKS managed node group, a GKE or AKS pool): nodes labeled key=value have that architecture.
// arch is amd64, arm64 or any.
func ParsePool(spec string) (inventory.NodePool, error) {
	selector, arch, ok := strings.Cut(spec, ":")
	key, value, ok2 := strings.Cut(selector, "=")
	if !ok || !ok2 || key == "" || value == "" {
		return inventory.NodePool{}, fmt.Errorf("invalid --node-pool %q: use key=value:arch, for example role=web:amd64", spec)
	}
	pool := inventory.NodePool{Name: key + "=" + value, Labels: map[string]string{key: value}, LabelOnly: key != nodePoolKey}
	if key == nodePoolKey {
		pool.Name = value
	}
	switch arch {
	case "amd64", "arm64":
		pool.Requirements = []inventory.Requirement{{Key: archKey, Operator: "In", Values: []string{arch}}}
	case "any":
	default:
		return inventory.NodePool{}, fmt.Errorf("invalid --node-pool %q: the architecture must be amd64, arm64 or any", spec)
	}
	return pool, nil
}

// PoolCounts returns how many pools can provision arm64, for the summary.
func PoolCounts(pools []inventory.NodePool) (total, arm64 int) {
	for _, p := range pools {
		total++
		if poolArch(p).ARM64 {
			arm64++
		}
	}
	return total, arm64
}

// poolHint is the text to look for in the manifest to find the line where the pod picks its pool:
// the karpenter.sh/nodepool selector, or else the first label it selects that is its own.
func poolHint(pod corev1.PodSpec) string {
	if UsesNodePoolSelector(pod) {
		return nodePoolKey
	}
	keys := make([]string, 0, len(pod.NodeSelector))
	for k := range pod.NodeSelector {
		if customKey(k) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if len(keys) > 0 {
		return keys[0]
	}
	return "nodeSelector"
}

// ObservedPools derives pools from the nodes of a cluster, for the pods that select nodes by a
// label that no known pool answers for: an EKS managed node group, a GKE or AKS node pool. The
// architecture is what the nodes with that label report today, so it is a statement about the
// nodes that exist, and the message says so. A label that both architectures carry allows both.
// Nothing is derived for a label that no node has: a pool scaled to zero says nothing.
func ObservedPools(pods []corev1.PodSpec, known []inventory.NodePool, nodes []inventory.Node) []inventory.NodePool {
	if len(nodes) == 0 {
		return nil
	}
	type pair struct{ key, value string }
	wanted := map[pair]bool{}
	want := func(key, value string) {
		if key == nodePoolKey || customKey(key) {
			wanted[pair{key, value}] = true
		}
	}
	for _, pod := range pods {
		for k, v := range pod.NodeSelector {
			want(k, v)
		}
		for _, term := range requiredTerms(pod) {
			for _, e := range term.MatchExpressions {
				if e.Operator == corev1.NodeSelectorOpIn {
					for _, v := range e.Values {
						want(e.Key, v)
					}
				}
			}
		}
	}
	answered := func(p pair) bool {
		for _, kp := range known {
			if v, ok := poolValue(kp, p.key); ok && v == p.value {
				return true
			}
		}
		return false
	}
	var keys []pair
	for p := range wanted {
		if !answered(p) {
			keys = append(keys, p)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].key+"="+keys[i].value < keys[j].key+"="+keys[j].value })

	var out []inventory.NodePool
	for _, p := range keys {
		arches := map[string]int{}
		total := 0
		for _, n := range nodes {
			if n.Labels[p.key] == p.value {
				total++
				arches[n.Arch()]++
			}
		}
		if total == 0 {
			continue
		}
		pool := inventory.NodePool{Name: p.key + "=" + p.value, Labels: map[string]string{p.key: p.value}, LabelOnly: p.key != nodePoolKey}
		if p.key == nodePoolKey {
			pool.Name = p.value
		}
		if len(arches) == 1 {
			for arch := range arches {
				if arch == "amd64" || arch == "arm64" {
					pool.Requirements = []inventory.Requirement{{Key: archKey, Operator: "In", Values: []string{arch},
						Reason: fmt.Sprintf("observed on the cluster: all %d node(s) labeled %s=%s are %s", total, p.key, p.value, arch)}}
				}
			}
		}
		out = append(out, pool)
	}
	return out
}

// OnArm64 reports whether the workload can only land on arm64 nodes: it selects the architecture
// itself (a node selector or a required node affinity that allows arm64 and not amd64), or every
// NodePool it can land on provisions only arm64. Such a workload is already on Graviton, so moving
// it is not part of the migration. A pool that states no architecture does not count: nothing says
// that its nodes are arm64.
func OnArm64(pod corev1.PodSpec, pools []inventory.NodePool) bool {
	if allowsArch(pod, "arm64") && !allowsArch(pod, "amd64") {
		return true
	}
	matched, resolved := selectedPools(pod, pools)
	if !resolved || len(matched) == 0 {
		return false
	}
	for _, p := range matched {
		if a := poolArch(p); a.AMD64 || !a.ARM64 {
			return false
		}
	}
	return true
}
