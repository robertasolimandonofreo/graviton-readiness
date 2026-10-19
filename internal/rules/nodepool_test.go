package rules

import (
	"strings"
	"testing"

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
