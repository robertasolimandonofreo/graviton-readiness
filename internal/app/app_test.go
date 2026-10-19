package app

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/robertasolimandonofreo/graviton-readiness/internal/image"
)

type fakeChecker map[string]image.Result

func (f fakeChecker) Check(_ context.Context, ref string) image.Result {
	if r, ok := f[ref]; ok {
		return r
	}
	return image.Result{Status: image.Unknown, Reason: "not in the fake registry"}
}

func run(t *testing.T, stdin string, c image.Checker, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(args, Env{Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errb, Version: "test", Checker: c})
	return code, out.String(), errb.String()
}

const sample = "../../testdata/sample.yaml"

func TestScanTable(t *testing.T) {
	code, out, _ := run(t, "", nil, "scan", sample)
	if code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{"VERDICT", "shop/deployment/api", "not-ready", "5 workloads", "Images were not checked"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestFlagsMayFollowThePaths(t *testing.T) {
	code, out, _ := run(t, "", nil, "scan", sample, "-f", "json")
	if code != exitOK || !strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Errorf("exit=%d out=%.40q", code, out)
	}
}

func TestFailOn(t *testing.T) {
	tests := []struct {
		failOn string
		want   int
	}{{"", exitOK}, {"not-ready", exitFail}, {"unverified", exitFail}, {"caveats", exitFail}}
	for _, tt := range tests {
		args := []string{"scan", sample}
		if tt.failOn != "" {
			args = append(args, "--fail-on", tt.failOn)
		}
		if code, _, _ := run(t, "", nil, args...); code != tt.want {
			t.Errorf("--fail-on %q: exit = %d, want %d", tt.failOn, code, tt.want)
		}
	}
	// A clean, verified workload never fails.
	clean := "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: ok}\nspec:\n  template:\n    spec:\n      containers:\n        - {name: c, image: ok:1}\n"
	c := fakeChecker{"ok:1": {Status: image.Arm64}}
	if code, _, _ := run(t, clean, c, "scan", "-", "--check-images", "--fail-on", "caveats"); code != exitOK {
		t.Errorf("clean workload failed the build: exit %d", code)
	}
}

func TestCheckImagesWithFakeRegistry(t *testing.T) {
	c := fakeChecker{
		"public.ecr.aws/nginx/nginx:1.27":             {Status: image.Arm64},
		"registry.example.com/shop/api:2.3":           {Status: image.Arm64},
		"registry.example.com/shop/worker:1.9-amd64":  {Status: image.NoArm64, Platforms: []string{"linux/amd64"}},
		"registry.example.com/platform/log-agent:4.1": {Status: image.NoArm64, Platforms: []string{"linux/amd64"}},
		"registry.example.com/shop/report:7":          {Status: image.Arm64},
	}
	code, out, _ := run(t, "", c, "scan", sample, "--check-images", "-f", "json")
	if code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	var doc struct {
		SchemaVersion int  `json:"schemaVersion"`
		ImagesChecked bool `json:"imagesChecked"`
		Summary       struct {
			PotentialPercent int `json:"potentialPercent"`
		} `json:"summary"`
		Workloads []struct {
			ID      string `json:"id"`
			Verdict string `json:"verdict"`
		} `json:"workloads"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.SchemaVersion != 1 || !doc.ImagesChecked {
		t.Errorf("header = %+v", doc)
	}
	got := map[string]string{}
	for _, w := range doc.Workloads {
		got[w.ID] = w.Verdict
	}
	want := map[string]string{
		"shop/deployment/web":             "ready",
		"shop/deployment/api":             "not-ready", // pinned to amd64
		"shop/deployment/worker":          "not-ready",
		"kube-system/daemonset/log-agent": "not-ready", // image without arm64
		"shop/cronjob/report":             "ready",
	}
	for id, v := range want {
		if got[id] != v {
			t.Errorf("%s = %q, want %q", id, got[id], v)
		}
	}
	// web: 3 x 500m = 1500m of 1500+2000+500 = 4000m requested by long-running workloads.
	if doc.Summary.PotentialPercent != 37 {
		t.Errorf("potential = %d%%, want 37%%", doc.Summary.PotentialPercent)
	}
}

func TestStdinAndMarkdown(t *testing.T) {
	code, out, _ := run(t, "apiVersion: v1\nkind: Pod\nmetadata: {name: p}\nspec:\n  nodeSelector: {kubernetes.io/arch: amd64}\n  containers:\n    - {name: c, image: x:1}\n", nil, "scan", "-", "-f", "markdown")
	if code != exitOK || !strings.HasPrefix(out, "<!-- graviton-readiness -->") || !strings.Contains(out, "`default/pod/p`") {
		t.Errorf("exit=%d out=%s", code, out)
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"nope"},
		{"scan"},
		{"scan", sample, "-f", "xml"},
		{"scan", sample, "--fail-on", "bad"},
		{"scan", sample, "--timeout", "soon", "--check-images"},
		{"scan", "does-not-exist"},
		{"explain"},
		{"explain", "GR999"},
	} {
		if code, _, _ := run(t, "", nil, args...); code != exitUsage {
			t.Errorf("%v: exit = %d, want %d", args, code, exitUsage)
		}
	}
}

func TestExplainAndVersion(t *testing.T) {
	code, out, _ := run(t, "", nil, "explain", "gr004")
	if code != exitOK || !strings.Contains(out, "GR004 [blocker]") || !strings.Contains(out, "buildx") {
		t.Errorf("exit=%d out=%s", code, out)
	}
	if _, out, _ := run(t, "", nil, "version"); !strings.Contains(out, "test") {
		t.Errorf("version output = %q", out)
	}
}

func TestEmptyScanWarns(t *testing.T) {
	dir := t.TempDir()
	code, _, errOut := run(t, "", nil, "scan", dir)
	if code != exitOK || !strings.Contains(errOut, "no workloads found") {
		t.Errorf("exit=%d stderr=%q", code, errOut)
	}
}

const poolManifests = `
apiVersion: karpenter.sh/v1
kind: NodePool
metadata: {name: web}
spec:
  template:
    metadata: {labels: {role: web}}
    spec:
      requirements:
        - {key: kubernetes.io/arch, operator: In, values: [amd64]}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: api}
spec:
  template:
    spec:
      nodeSelector: {role: web}
      containers: [{name: c, image: c:1}]
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: managed}
spec:
  template:
    spec:
      nodeSelector: {eks.amazonaws.com/nodegroup: general}
      containers: [{name: c, image: c:1}]
`

func TestNodePoolRuleAndNote(t *testing.T) {
	code, out, _ := run(t, poolManifests, nil, "scan", "-", "-v")
	if code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{"GR010", "only provisions amd64", "NodePools read: 1 (0 can provision arm64)", "1 workloads choose nodes by a label that no scanned NodePool defines", "--node-pool"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// poolRules scans the manifests and returns, per workload, the NodePool rules it got.
func poolRules(t *testing.T, args ...string) map[string][]string {
	t.Helper()
	code, out, _ := run(t, poolManifests, nil, append([]string{"scan", "-", "-f", "json"}, args...)...)
	if code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	var doc struct {
		Workloads []struct {
			Name     string `json:"name"`
			Findings []struct {
				Rule string `json:"rule"`
			} `json:"findings"`
		} `json:"workloads"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, w := range doc.Workloads {
		for _, f := range w.Findings {
			if f.Rule == "GR010" || f.Rule == "GR011" {
				got[w.Name] = append(got[w.Name], f.Rule)
			}
		}
	}
	return got
}

func TestNodePoolFlagDeclaresAPoolThatIsNotKarpenter(t *testing.T) {
	// Only the Karpenter NodePool is known: api is pinned to it, managed is not resolved.
	if got := poolRules(t); len(got) != 1 || got["api"][0] != "GR010" {
		t.Errorf("without the flag: %v", got)
	}
	// Declaring the EKS node group as amd64 pins managed too.
	if got := poolRules(t, "--node-pool", "eks.amazonaws.com/nodegroup=general:amd64"); got["api"][0] != "GR010" || got["managed"][0] != "GR010" {
		t.Errorf("amd64 node group: %v", got)
	}
	// Declared arm64, managed is fine, and api keeps its finding.
	if got := poolRules(t, "--node-pool", "eks.amazonaws.com/nodegroup=general:arm64"); len(got) != 1 || got["api"][0] != "GR010" {
		t.Errorf("arm64 node group: %v", got)
	}
	_, out, _ := run(t, poolManifests, nil, "scan", "-", "--node-pool", "eks.amazonaws.com/nodegroup=general:amd64")
	if strings.Contains(out, "choose nodes by a label") {
		t.Errorf("every label is now known, the note should be gone:\n%s", out)
	}
}

func TestNodePoolFlagErrorsAndJSON(t *testing.T) {
	for _, bad := range []string{"role", "role=web", "role=web:sparc"} {
		if code, _, _ := run(t, poolManifests, nil, "scan", "-", "--node-pool", bad); code != exitUsage {
			t.Errorf("--node-pool %q: exit = %d, want %d", bad, code, exitUsage)
		}
	}
	_, out, _ := run(t, poolManifests, nil, "scan", "-", "-f", "json")
	var doc struct {
		NodePools struct {
			Read       int `json:"read"`
			ARM64      int `json:"arm64"`
			Unresolved int `json:"workloadsWithUnknownPool"`
		} `json:"nodePools"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || doc.NodePools.Read != 1 || doc.NodePools.ARM64 != 0 || doc.NodePools.Unresolved != 1 {
		t.Errorf("nodePools = %+v, err = %v", doc.NodePools, err)
	}
}
