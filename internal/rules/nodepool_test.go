package rules

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/robertasolimandonofreo/graviton-readiness/internal/inventory"
)

func pool(name string, labels map[string]string, reqs ...inventory.Requirement) inventory.NodePool {
	return inventory.NodePool{Name: name, Labels: labels, Requirements: reqs}
}

func arch(op string, values ...string) inventory.Requirement {
	return inventory.Requirement{Key: archKey, Operator: op, Values: values}
}

var testPools = []inventory.NodePool{
	pool("web", map[string]string{"role": "web"}, arch("In", "amd64")),
	pool("batch", map[string]string{"role": "batch"}, arch("In", "arm64")),
	pool("mixed", map[string]string{"role": "mixed"}, arch("In", "amd64", "arm64")),
	pool("untyped", map[string]string{"role": "untyped"}),
	pool("t3-only", map[string]string{"role": "t3"}, inventory.Requirement{Key: "node.kubernetes.io/instance-type", Operator: "In", Values: []string{"t3.large", "t3.medium"}}),
	pool("graviton-types", map[string]string{"role": "g"}, inventory.Requirement{Key: instanceFamilyKey, Operator: "In", Values: []string{"m7g", "c7g"}}),
}

func podWith(selector string) string {
	return "containers:\n  - name: c\n    image: registry.example.com/web:1.0\n" + selector
}

func TestNodePoolRule(t *testing.T) {
	tests := []struct {
		name string
		pod  string
		want string
	}{
		{"selects an amd64 pool by name", "nodeSelector:\n  karpenter.sh/nodepool: web\n", "GR010"},
		{"selects an amd64 pool by label", "nodeSelector:\n  role: web\n", "GR010"},
		{"selects an arm64 pool", "nodeSelector:\n  role: batch\n", ""},
		{"selects a pool that allows both", "nodeSelector:\n  role: mixed\n", ""},
		{"a pool with no arch is not assumed to be amd64", "nodeSelector:\n  role: untyped\n", ""},
		{"x86 instance types prove amd64", "nodeSelector:\n  role: t3\n", "GR010"},
		{"graviton families prove arm64", "nodeSelector:\n  role: g\n", ""},
		{"no selector says nothing about a pool", "", ""},
		{"a label no pool defines says nothing", "nodeSelector:\n  disk: ssd\n", ""},
		{"a name that is not among the pools", "nodeSelector:\n  karpenter.sh/nodepool: elsewhere\n", "GR011"},
		{"a label value that no pool has", "nodeSelector:\n  role: nobody\n", "GR011"},
		{"affinity picks an amd64 pool", affinity("karpenter.sh/nodepool", "In", "web"), "GR010"},
		{"affinity picks amd64 or arm64 pools", affinity("karpenter.sh/nodepool", "In", "web, batch"), ""},
		{"affinity NotIn leaves an arm64 pool", affinity("role", "NotIn", "web"), ""},
		{"selector and affinity are both applied", "nodeSelector:\n  role: web\n" + affinity("karpenter.sh/nodepool", "In", "web, batch"), "GR010"},
		{"already pinned to amd64: the pod rule speaks, not the pool", "nodeSelector:\n  kubernetes.io/arch: amd64\n  role: web\n", "GR001"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := deployment(t, "Deployment", podWith(tt.pod))
			got := Evaluate(w, nil, Options{NodePools: testPools})
			var rules []string
			for _, f := range got.Findings {
				if f.Rule == "GR010" || f.Rule == "GR011" || f.Rule == "GR001" {
					rules = append(rules, f.Rule)
				}
			}
			if strings.Join(rules, ",") != tt.want {
				t.Errorf("rules = %v, want %q (%+v)", rules, tt.want, got.Findings)
			}
		})
	}
}

func TestNoNodePoolsMeansNoPoolFinding(t *testing.T) {
	w := deployment(t, "Deployment", podWith("nodeSelector:\n  karpenter.sh/nodepool: web\n"))
	got := Evaluate(w, nil, Options{})
	for _, f := range got.Findings {
		if f.Rule == "GR010" || f.Rule == "GR011" {
			t.Errorf("no pools were scanned, but got %s", f.Rule)
		}
	}
}

func TestPoolMessageSaysWhy(t *testing.T) {
	w := deployment(t, "Deployment", podWith("nodeSelector:\n  role: t3\n"))
	got := Evaluate(w, nil, Options{NodePools: testPools})
	if len(got.Findings) == 0 || !strings.Contains(got.Findings[0].Message, "only x86 types") {
		t.Errorf("message = %+v", got.Findings)
	}
}

func TestPoolUnknown(t *testing.T) {
	cases := []struct {
		name  string
		pod   string
		pools []inventory.NodePool
		want  bool
	}{
		{"label with no NodePool at all", "nodeSelector:\n  role: web\n", nil, true},
		{"karpenter selector with no NodePool", "nodeSelector:\n  karpenter.sh/nodepool: web\n", nil, true},
		{"label that a pool defines", "nodeSelector:\n  role: web\n", testPools, false},
		{"label that no pool defines", "nodeSelector:\n  disk: ssd\n", testPools, true},
		{"built-in labels only", "nodeSelector:\n  kubernetes.io/os: linux\n  topology.kubernetes.io/zone: us-east-1a\n", nil, false},
		{"no selector", "", nil, false},
	}
	for _, c := range cases {
		w := deployment(t, "Deployment", podWith(c.pod))
		if got := PoolUnknown(w.Pod, c.pools); got != c.want {
			t.Errorf("%s: PoolUnknown = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestParsePool(t *testing.T) {
	p, err := ParsePool("role=web:amd64")
	if err != nil || p.Name != "role=web" || p.Labels["role"] != "web" || poolArch(p).ARM64 {
		t.Errorf("amd64 pool = %+v, %v", p, err)
	}
	if p, err := ParsePool("eks.amazonaws.com/nodegroup=graviton:arm64"); err != nil || poolArch(p).AMD64 {
		t.Errorf("arm64 pool = %+v, %v", p, err)
	}
	if p, err := ParsePool("role=any:any"); err != nil || !poolArch(p).ARM64 || !poolArch(p).AMD64 {
		t.Errorf("any pool = %+v, %v", p, err)
	}
	if p, err := ParsePool("karpenter.sh/nodepool=prod:amd64"); err != nil || p.Name != "prod" {
		t.Errorf("a karpenter.sh/nodepool pool is named by its value: %+v, %v", p, err)
	}
	for _, bad := range []string{"", "role=web", "role:amd64", "=web:amd64", "role=web:sparc", "role=:arm64"} {
		if _, err := ParsePool(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestPoolArchCombinesRequirements(t *testing.T) {
	// NotIn and an explicit list are combined: In [amd64, arm64] minus NotIn [arm64] is amd64 only.
	p := pool("p", nil, arch("In", "amd64", "arm64"), arch("NotIn", "arm64"))
	if a := poolArch(p); a.ARM64 || !a.AMD64 {
		t.Errorf("arch = %+v", a)
	}
	if a := poolArch(pool("p", nil, arch("DoesNotExist"))); a.ARM64 || a.AMD64 {
		t.Errorf("DoesNotExist should allow neither: %+v", a)
	}
	n, g := PoolCounts(testPools)
	if n != 6 || g != 4 {
		t.Errorf("counts = %d, %d; want 6, 4", n, g)
	}
}

func TestLabelOnlyPoolIsNotMatchedByName(t *testing.T) {
	// The workload selects both a label and the NodePool name, as many real manifests do. Only the
	// label is declared, so the name selector must not turn the declared pool into "not found".
	declared, err := ParsePool("role=web:amd64")
	if err != nil {
		t.Fatal(err)
	}
	w := deployment(t, "Deployment", podWith("nodeSelector:\n  role: web\n  karpenter.sh/nodepool: web\n"))
	got := Evaluate(w, nil, Options{NodePools: []inventory.NodePool{declared}})
	var rules []string
	for _, f := range got.Findings {
		if f.Rule == "GR010" || f.Rule == "GR011" {
			rules = append(rules, f.Rule)
		}
	}
	if strings.Join(rules, ",") != "GR010" {
		t.Errorf("rules = %v, want GR010 (%+v)", rules, got.Findings)
	}
	if PoolUnknown(w.Pod, []inventory.NodePool{declared}) {
		t.Error("the pool is declared, so it is not unknown")
	}
}

func TestWindowsNodeClassMakesAPoolAmd64Only(t *testing.T) {
	inv := inventory.New()
	for _, doc := range []string{
		`{"apiVersion":"karpenter.sh/v1","kind":"NodePool","metadata":{"name":"win"},"spec":{"template":{"metadata":{"labels":{"role":"win"}},"spec":{"nodeClassRef":{"name":"windows"},"requirements":[{"key":"kubernetes.io/arch","operator":"In","values":["amd64","arm64"]}]}}}}`,
		`{"apiVersion":"karpenter.sh/v1","kind":"NodePool","metadata":{"name":"lin"},"spec":{"template":{"metadata":{"labels":{"role":"lin"}},"spec":{"nodeClassRef":{"name":"default"},"requirements":[{"key":"kubernetes.io/arch","operator":"In","values":["amd64","arm64"]}]}}}}`,
		`{"apiVersion":"karpenter.k8s.aws/v1","kind":"EC2NodeClass","metadata":{"name":"windows"},"spec":{"amiFamily":"Windows2022"}}`,
		`{"apiVersion":"karpenter.k8s.aws/v1","kind":"EC2NodeClass","metadata":{"name":"default"},"spec":{"amiFamily":"AL2023"}}`,
	} {
		if err := inv.AddJSON("x", []byte(doc)); err != nil {
			t.Fatal(err)
		}
	}
	inv.LinkNodeClasses()
	win := Evaluate(deployment(t, "Deployment", podWith("nodeSelector:\n  role: win\n")), nil, Options{NodePools: inv.NodePools})
	var msg string
	for _, f := range win.Findings {
		if f.Rule == "GR010" {
			msg = f.Message
		}
	}
	if !strings.Contains(msg, "EC2NodeClass windows uses a Windows AMI") {
		t.Errorf("GR010 must explain the Windows class: %q (%+v)", msg, win.Findings)
	}
	lin := Evaluate(deployment(t, "Deployment", podWith("nodeSelector:\n  role: lin\n")), nil, Options{NodePools: inv.NodePools})
	for _, f := range lin.Findings {
		if f.Rule == "GR010" {
			t.Errorf("a Linux class must not block: %s", f.Message)
		}
	}
	if n, arm := PoolCounts(inv.NodePools); n != 2 || arm != 1 {
		t.Errorf("counts = %d, %d; want 2 pools of which 1 can provision arm64", n, arm)
	}
}

func podSpec(t *testing.T, selector string) corev1.PodSpec {
	t.Helper()
	return deployment(t, "Deployment", podWith(selector)).Pod
}

func nodes(arch map[string]string, label, value string) []inventory.Node {
	var out []inventory.Node
	for name, a := range arch {
		out = append(out, inventory.Node{Name: name, Labels: map[string]string{label: value, "kubernetes.io/arch": a}})
	}
	return out
}

func TestObservedPools(t *testing.T) {
	pod := podSpec(t, "nodeSelector:\n  eks.amazonaws.com/nodegroup: general\n")
	eval := func(ns []inventory.Node, known ...inventory.NodePool) (string, []inventory.NodePool) {
		pools := ObservedPools([]corev1.PodSpec{pod}, known, ns)
		w := deployment(t, "Deployment", podWith("nodeSelector:\n  eks.amazonaws.com/nodegroup: general\n"))
		got := Evaluate(w, nil, Options{NodePools: append(append([]inventory.NodePool{}, known...), pools...)})
		var rules []string
		for _, f := range got.Findings {
			if f.Rule == "GR010" || f.Rule == "GR011" {
				rules = append(rules, f.Rule)
			}
		}
		return strings.Join(rules, ","), pools
	}
	const label = "eks.amazonaws.com/nodegroup"

	if rules, pools := eval(nodes(map[string]string{"a": "amd64", "b": "amd64"}, label, "general")); rules != "GR010" || len(pools) != 1 {
		t.Errorf("a node group of amd64 nodes blocks: %q %+v", rules, pools)
	} else if r := pools[0].Requirements[0].Reason; !strings.Contains(r, "observed on the cluster: all 2 node(s) labeled eks.amazonaws.com/nodegroup=general are amd64") {
		t.Errorf("the message says it is an observation: %q", r)
	}
	if rules, _ := eval(nodes(map[string]string{"a": "arm64"}, label, "general")); rules != "" {
		t.Errorf("arm64 nodes do not block: %q", rules)
	}
	if rules, _ := eval(nodes(map[string]string{"a": "amd64", "b": "arm64"}, label, "general")); rules != "" {
		t.Errorf("a group with both architectures does not block: %q", rules)
	}
	// No node has the label: a group scaled to zero says nothing, so no pool is made.
	if rules, pools := eval(nodes(map[string]string{"a": "amd64"}, label, "another")); len(pools) != 0 || rules != "GR011" && rules != "" {
		t.Errorf("no node has the label: pools %v rules %q", pools, rules)
	}
	if got := ObservedPools([]corev1.PodSpec{pod}, nil, nil); got != nil {
		t.Errorf("no nodes read, no pools: %+v", got)
	}
	// A pool that is already known answers for the label: the nodes are not asked.
	known := inventory.NodePool{Name: "p", Labels: map[string]string{label: "general"}, LabelOnly: true}
	if pools := ObservedPools([]corev1.PodSpec{pod}, []inventory.NodePool{known}, nodes(map[string]string{"a": "amd64"}, label, "general")); len(pools) != 0 {
		t.Errorf("a known pool wins over what the nodes say: %+v", pools)
	}
	// The karpenter.sh/nodepool label of a pool whose manifest was not read.
	npod := podSpec(t, "nodeSelector:\n  karpenter.sh/nodepool: web\n")
	ns := nodes(map[string]string{"a": "amd64"}, "karpenter.sh/nodepool", "web")
	if pools := ObservedPools([]corev1.PodSpec{npod}, nil, ns); len(pools) != 1 || pools[0].Name != "web" || pools[0].LabelOnly {
		t.Errorf("a pool selected by name is named by it: %+v", pools)
	}
}

func TestOnArm64(t *testing.T) {
	pools := []inventory.NodePool{
		pool("arm", map[string]string{"role": "arm"}, arch("In", "arm64")),
		pool("x86", map[string]string{"role": "x86"}, arch("In", "amd64")),
		pool("both", map[string]string{"role": "both"}, arch("In", "amd64", "arm64")),
		pool("silent", map[string]string{"role": "silent"}),
		pool("graviton", map[string]string{"role": "g"}, inventory.Requirement{Key: instanceFamilyKey, Operator: "In", Values: []string{"m7g", "c7g"}}),
	}
	tests := []struct {
		name string
		pod  string
		want bool
	}{
		{"an arm64 node selector", "nodeSelector:\n  kubernetes.io/arch: arm64\n", true},
		{"an amd64 node selector", "nodeSelector:\n  kubernetes.io/arch: amd64\n", false},
		{"affinity that allows only arm64", affinity("kubernetes.io/arch", "In", "arm64"), true},
		{"affinity that allows both", affinity("kubernetes.io/arch", "In", "amd64, arm64"), false},
		{"a pool with only arm64 nodes", "nodeSelector:\n  role: arm\n", true},
		{"selected by the pool name", "nodeSelector:\n  karpenter.sh/nodepool: arm\n", true},
		{"a pool with only graviton types", "nodeSelector:\n  role: g\n", true},
		{"an amd64 pool", "nodeSelector:\n  role: x86\n", false},
		{"a pool with both", "nodeSelector:\n  role: both\n", false},
		{"a pool that says nothing is not assumed to be arm64", "nodeSelector:\n  role: silent\n", false},
		{"nothing selected", "", false},
		{"a pool that is not known", "nodeSelector:\n  role: nobody\n", false},
		{"affinity over two pools, one amd64", affinity("karpenter.sh/nodepool", "In", "arm, x86"), false},
		{"affinity over two arm64 pools", affinity("karpenter.sh/nodepool", "In", "arm, graviton"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := deployment(t, "Deployment", podWith(tt.pod))
			if got := OnArm64(w.Pod, pools); got != tt.want {
				t.Errorf("OnArm64 = %v, want %v", got, tt.want)
			}
			if got := Evaluate(w, nil, Options{NodePools: pools}).OnGraviton; got != tt.want {
				t.Errorf("Assessment.OnGraviton = %v, want %v", got, tt.want)
			}
		})
	}
}
