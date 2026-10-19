package rules

import (
	"strings"
	"testing"

	"github.com/robertasolimandonofreo/graviton-readiness/internal/image"
	"github.com/robertasolimandonofreo/graviton-readiness/internal/inventory"
)

// deployment builds a one-workload inventory from the pod-spec lines, indented under spec.
func deployment(t *testing.T, kind, podSpec string) inventory.Workload {
	t.Helper()
	doc := "apiVersion: apps/v1\nkind: " + kind + "\nmetadata:\n  name: app\nspec:\n  template:\n    spec:\n" +
		indent(podSpec, "      ")
	inv := &inventory.Inventory{Skipped: map[string]int{}}
	if err := inv.Add("", []byte(doc)); err != nil {
		t.Fatal(err)
	}
	if len(inv.Workloads) != 1 {
		t.Fatalf("got %d workloads", len(inv.Workloads))
	}
	return inv.Workloads[0]
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.Trim(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n") + "\n"
}

func ids(a Assessment) string {
	var out []string
	for _, f := range a.Findings {
		out = append(out, f.Rule)
	}
	return strings.Join(out, ",")
}

const plainPod = `
containers:
  - name: web
    image: registry.example.com/web:1.0
`

func TestEvaluate(t *testing.T) {
	arm := map[string]image.Result{"registry.example.com/web:1.0": {Status: image.Arm64}}
	x86 := map[string]image.Result{"registry.example.com/web:1.0": {Status: image.NoArm64, Platforms: []string{"linux/amd64"}}}
	unknown := map[string]image.Result{"registry.example.com/web:1.0": {Status: image.Unknown, Reason: "unauthorized"}}

	tests := []struct {
		name    string
		kind    string
		pod     string
		images  map[string]image.Result
		checked bool
		want    string // finding IDs
		verdict Verdict
	}{
		{"clean and verified", "Deployment", plainPod, arm, true, "", Ready},
		{"clean but images not checked", "Deployment", plainPod, nil, false, "", Unverified},
		{"registry did not answer", "Deployment", plainPod, unknown, true, "GR005", Unverified},
		{"no arm64 manifest, unpinned", "Deployment", plainPod, x86, true, "GR004", NotReady},
		{"node selector pins amd64", "Deployment", plainPod + "nodeSelector:\n  kubernetes.io/arch: amd64\n", arm, true, "GR001", NotReady},
		{"node selector arm64 is fine", "Deployment", plainPod + "nodeSelector:\n  kubernetes.io/arch: arm64\n", arm, true, "", Ready},
		{"beta arch label pins amd64", "Deployment", plainPod + "nodeSelector:\n  beta.kubernetes.io/arch: amd64\n", arm, true, "GR001", NotReady},
		{"affinity In amd64 only", "Deployment", plainPod + affinity("kubernetes.io/arch", "In", "amd64"), arm, true, "GR001", NotReady},
		{"affinity In both", "Deployment", plainPod + affinity("kubernetes.io/arch", "In", "amd64, arm64"), arm, true, "", Ready},
		{"affinity NotIn arm64", "Deployment", plainPod + affinity("kubernetes.io/arch", "NotIn", "arm64"), arm, true, "GR001", NotReady},
		{"x86 instance type selector", "Deployment", plainPod + "nodeSelector:\n  node.kubernetes.io/instance-type: m5.large\n", arm, true, "GR002", NotReady},
		{"graviton instance type selector", "Deployment", plainPod + "nodeSelector:\n  node.kubernetes.io/instance-type: m7g.large\n", arm, true, "", Ready},
		{"x86 instance family affinity", "Deployment", plainPod + affinity("karpenter.k8s.aws/instance-family", "In", "m5, c5"), arm, true, "GR002", NotReady},
		{"mixed instance family affinity", "Deployment", plainPod + affinity("karpenter.k8s.aws/instance-family", "In", "m5, m7g"), arm, true, "", Ready},
		{"windows", "Deployment", plainPod + "nodeSelector:\n  kubernetes.io/os: windows\n", arm, true, "GR009", NotReady},
		{"amd64 tag", "Deployment", "containers:\n  - name: web\n    image: registry.example.com/web:1.0-amd64\n", nil, false, "GR003", NotReady},
		{"amd64 repository", "Deployment", "containers:\n  - name: web\n    image: amd64/ubuntu:22.04\n", nil, false, "GR003", NotReady},
		{"arm64 tag is fine", "Deployment", "containers:\n  - name: web\n    image: registry.example.com/web:1.0-arm64\n", nil, false, "", Unverified},
		{"digest is ignored", "Deployment", "containers:\n  - name: web\n    image: registry.example.com/web@sha256:" + strings.Repeat("a", 64) + "\n", nil, false, "", Unverified},
		{"x86 hint in args", "Deployment", "containers:\n  - name: web\n    image: registry.example.com/web:1.0\n    args: [\"curl -L https://x/tool_linux_amd64\"]\n", arm, true, "GR006", ReadyWithCaveats},
		{"x86 hint in env", "Deployment", "containers:\n  - name: web\n    image: registry.example.com/web:1.0\n    env:\n      - {name: ARCH, value: x86_64}\n", arm, true, "GR006", ReadyWithCaveats},
		{"gpu", "Deployment", "containers:\n  - name: web\n    image: registry.example.com/web:1.0\n    resources:\n      limits:\n        nvidia.com/gpu: 1\n", arm, true, "GR007", ReadyWithCaveats},
		{"daemonset", "DaemonSet", plainPod, arm, true, "GR008", Ready},
		{"init container checked too", "Deployment", plainPod + "initContainers:\n  - name: init\n    image: registry.example.com/init:1\n", arm, true, "GR005", Unverified},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := deployment(t, tt.kind, tt.pod)
			got := Evaluate(w, tt.images, Options{ImagesChecked: tt.checked})
			if ids(got) != tt.want {
				t.Errorf("findings = %q, want %q (%+v)", ids(got), tt.want, got.Findings)
			}
			if got.Verdict != tt.verdict {
				t.Errorf("verdict = %s, want %s", got.Verdict, tt.verdict)
			}
		})
	}
}

func affinity(key, op, values string) string {
	return `affinity:
  nodeAffinity:
    requiredDuringSchedulingIgnoredDuringExecution:
      nodeSelectorTerms:
        - matchExpressions:
            - key: ` + key + `
              operator: ` + op + `
              values: [` + values + `]
`
}

func TestAffinityTermsAreOred(t *testing.T) {
	// The first term pins amd64, the second allows arm64: the pod can run on arm64.
	pod := plainPod + `affinity:
  nodeAffinity:
    requiredDuringSchedulingIgnoredDuringExecution:
      nodeSelectorTerms:
        - matchExpressions:
            - {key: kubernetes.io/arch, operator: In, values: [amd64]}
        - matchExpressions:
            - {key: kubernetes.io/arch, operator: In, values: [arm64]}
`
	got := Evaluate(deployment(t, "Deployment", pod), nil, Options{})
	if strings.Contains(ids(got), "GR001") {
		t.Errorf("OR of terms should allow arm64, got %s", ids(got))
	}
}

func TestNoArm64WhenPinnedDoesNotSuggestLandingOnArm(t *testing.T) {
	pod := plainPod + "nodeSelector:\n  kubernetes.io/arch: amd64\n"
	images := map[string]image.Result{"registry.example.com/web:1.0": {Status: image.NoArm64, Platforms: []string{"linux/amd64"}}}
	got := Evaluate(deployment(t, "Deployment", pod), images, Options{ImagesChecked: true})
	for _, f := range got.Findings {
		if f.Rule == "GR004" && strings.Contains(f.Message, "can land on an arm64 node") {
			t.Errorf("a pinned workload cannot land on arm64: %s", f.Message)
		}
	}
}

func TestScoreAndOrder(t *testing.T) {
	pod := "containers:\n  - name: web\n    image: registry.example.com/web:1.0-amd64\n    args: [amd64]\n" +
		"nodeSelector:\n  kubernetes.io/arch: amd64\n"
	got := Evaluate(deployment(t, "Deployment", pod), nil, Options{})
	if got.Score != 100-2*blockerCost-caveatCost {
		t.Errorf("score = %d", got.Score)
	}
	if got.Findings[0].Severity != Blocker || got.Findings[len(got.Findings)-1].Severity != Caveat {
		t.Errorf("findings are not ordered by severity: %+v", got.Findings)
	}
}

func TestGravitonTypes(t *testing.T) {
	for _, v := range []string{"m7g.large", "c6gn.xlarge", "x2gd.metal", "im4gn.large", "is4gen.xlarge", "g5g.xlarge", "hpc7g.16xlarge", "t4g.small", "a1.medium", "m6g", "r8g"} {
		if !gravitonType.MatchString(v) {
			t.Errorf("%s should be Graviton", v)
		}
	}
	for _, v := range []string{"m5.large", "c7i.large", "m7a.large", "g4dn.xlarge", "g5.xlarge", "r6id.large", "p4d.24xlarge", "t3.micro", "m6a"} {
		if gravitonType.MatchString(v) {
			t.Errorf("%s should not be Graviton", v)
		}
	}
}

func TestCatalogCoversEveryRule(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range Catalog {
		if seen[r.ID] || r.Title == "" || r.Why == "" || r.Fix == "" {
			t.Errorf("rule %s is duplicated or incomplete", r.ID)
		}
		seen[r.ID] = true
	}
	// Every rule that Evaluate can emit must be documented.
	for _, id := range []string{"GR001", "GR002", "GR003", "GR004", "GR005", "GR006", "GR007", "GR008", "GR009", "GR010", "GR011"} {
		if _, ok := Lookup(id); !ok {
			t.Errorf("%s is not in the catalog", id)
		}
	}
}
