package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"

	"github.com/robertasolimandonofreo/graviton-readiness/internal/cluster/clustertest"
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

func TestSARIFFormat(t *testing.T) {
	code, out, _ := run(t, "", nil, "scan", sample, "-f", "sarif")
	if code != exitOK {
		t.Fatalf("exit %d", code)
	}
	var doc struct {
		Version string `json:"version"`
		Runs    []struct {
			Results []struct {
				RuleID string `json:"ruleId"`
			} `json:"results"`
		} `json:"runs"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || doc.Version != "2.1.0" || len(doc.Runs[0].Results) == 0 {
		t.Errorf("%v: %.200s", err, out)
	}
}

func TestPlanCommand(t *testing.T) {
	code, out, _ := run(t, "", nil, "plan", sample)
	if code != exitOK {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"GRAVITON MIGRATION PLAN", "Ready after changes:", "Verify first:", "shop/deployment/api", "GR001", "Images were not checked"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	// --check-images with a registry that knows the images moves the workloads between groups.
	c := fakeChecker{
		"public.ecr.aws/nginx/nginx:1.27":             {Status: image.Arm64},
		"registry.example.com/shop/report:7":          {Status: image.Arm64},
		"registry.example.com/platform/log-agent:4.1": {Status: image.Arm64},
	}
	_, out, _ = run(t, "", c, "plan", sample, "--check-images")
	if !regexp.MustCompile(`Ready now:\s+3\n`).MatchString(out) || !regexp.MustCompile(`Verify first:\s+0\n`).MatchString(out) {
		t.Errorf("with the images verified, 3 workloads are ready now and none is left to verify:\n%s", out)
	}
	if strings.Contains(out, "Images were not checked") {
		t.Errorf("they were checked:\n%s", out)
	}
}

func TestPlanFormatsAndFlags(t *testing.T) {
	_, out, _ := run(t, "", nil, "plan", sample, "-f", "json")
	var doc struct {
		SchemaVersion int `json:"schemaVersion"`
		Totals        struct {
			Total int `json:"total"`
		} `json:"totals"`
		Groups map[string][]struct {
			ID      string `json:"id"`
			Changes []struct {
				Rule string `json:"rule"`
			} `json:"changes"`
		} `json:"groups"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || doc.SchemaVersion != 1 || doc.Totals.Total != 5 {
		t.Fatalf("%v: %.200s", err, out)
	}
	if len(doc.Groups["ready-after-changes"]) != 2 || doc.Groups["ready-after-changes"][0].ID != "shop/deployment/api" || len(doc.Groups["ready-now"]) != 0 {
		t.Errorf("groups = %+v", doc.Groups)
	}
	if _, md, _ := run(t, "", nil, "plan", sample, "-f", "markdown"); !strings.HasPrefix(md, "<!-- graviton-readiness-plan -->") || !strings.Contains(md, "### Ready after changes (2)") {
		t.Errorf("markdown:\n%s", md)
	}
	// sarif belongs to scan, and --fail-on belongs to scan.
	if code, _, _ := run(t, "", nil, "plan", sample, "-f", "sarif"); code != exitUsage {
		t.Errorf("plan has no sarif format, exit %d", code)
	}
	if code, _, errOut := run(t, "", nil, "plan", sample, "--fail-on", "not-ready"); code != exitUsage || !strings.Contains(errOut, "belongs to scan") {
		t.Errorf("exit %d: %q", code, errOut)
	}
}

func clusterObjects() []runtime.Object {
	dep := func(name, selector string) *unstructured.Unstructured {
		return clustertest.Object("apps/v1", "Deployment", "shop", name, map[string]any{"spec": map[string]any{
			"replicas": int64(2),
			"template": map[string]any{"spec": map[string]any{
				"nodeSelector": map[string]any{"eks.amazonaws.com/nodegroup": selector},
				"containers":   []any{map[string]any{"name": "app", "image": "a:1", "resources": map[string]any{"requests": map[string]any{"cpu": "500m"}}}},
			}},
		}})
	}
	return []runtime.Object{
		dep("on-x86", "general"),
		dep("on-arm", "graviton"),
		clustertest.Node("g1", map[string]any{"kubernetes.io/arch": "amd64", "eks.amazonaws.com/nodegroup": "general"}),
		clustertest.Node("g2", map[string]any{"kubernetes.io/arch": "amd64", "eks.amazonaws.com/nodegroup": "general"}),
		clustertest.Node("a1", map[string]any{"kubernetes.io/arch": "arm64", "eks.amazonaws.com/nodegroup": "graviton"}),
	}
}

func runCluster(t *testing.T, client dynamic.Interface, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(args, Env{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb, Version: "test", Cluster: client})
	return code, out.String(), errb.String()
}

func TestScanClusterDerivesNonKarpenterPoolsFromNodes(t *testing.T) {
	client := clustertest.New(clusterObjects()...)
	code, out, errOut := runCluster(t, client, "scan", "--cluster", "-f", "json")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	var doc struct {
		NodePools struct {
			Read       int `json:"read"`
			ARM64      int `json:"arm64"`
			Unresolved int `json:"workloadsWithUnknownPool"`
		} `json:"nodePools"`
		Workloads []struct {
			Name     string `json:"name"`
			Findings []struct {
				Rule    string `json:"rule"`
				Message string `json:"message"`
			} `json:"findings"`
		} `json:"workloads"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, w := range doc.Workloads {
		for _, f := range w.Findings {
			if f.Rule == "GR010" {
				got[w.Name] = f.Message
			}
		}
	}
	if len(got) != 1 || !strings.Contains(got["on-x86"], "observed on the cluster: all 2 node(s)") {
		t.Errorf("only the workload on the amd64 node group is blocked, with the observation as the reason: %v", got)
	}
	if doc.NodePools.Read != 2 || doc.NodePools.ARM64 != 1 || doc.NodePools.Unresolved != 0 {
		t.Errorf("pools = %+v: both labels are resolved from the nodes", doc.NodePools)
	}
	for _, a := range client.Actions() {
		if a.GetVerb() != "list" {
			t.Errorf("verb %q: --cluster must only list", a.GetVerb())
		}
	}
	if !strings.Contains(errOut, "read-only") || !strings.Contains(errOut, "2 Deployments") || !strings.Contains(errOut, "3 nodes") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestScanClusterNeedsNoPathsAndWarnsInTheRightOrder(t *testing.T) {
	_, _, errOut := runCluster(t, clustertest.New(clusterObjects()...), "scan", "--cluster")
	if strings.Contains(errOut, "no workloads found") {
		t.Errorf("the cluster has workloads, but got: %q", errOut)
	}
	_, _, errOut = runCluster(t, clustertest.New(), "scan", "--cluster")
	if !strings.Contains(errOut, "no workloads found in the cluster") {
		t.Errorf("an empty cluster should warn: %q", errOut)
	}
	if code, _, errOut := run(t, "", nil, "scan"); code != exitUsage || !strings.Contains(errOut, "--cluster") {
		t.Errorf("exit %d, %q", code, errOut)
	}
}

func TestScanClusterNamespaceFlag(t *testing.T) {
	_, out, _ := runCluster(t, clustertest.New(clusterObjects()...), "scan", "--cluster", "-n", "elsewhere")
	if strings.Contains(out, "shop/deployment/on-x86") {
		t.Errorf("-n elsewhere must not read shop:\n%s", out)
	}
	_, out, _ = runCluster(t, clustertest.New(clusterObjects()...), "scan", "--cluster", "--namespace", "shop")
	if !strings.Contains(out, "shop/deployment/on-x86") {
		t.Errorf("--namespace shop must read shop:\n%s", out)
	}
}

func TestPlanAndSARIFWorkOnACluster(t *testing.T) {
	code, out, _ := runCluster(t, clustertest.New(clusterObjects()...), "plan", "--cluster")
	if code != exitOK || !strings.Contains(out, "shop/deployment/on-x86") || !strings.Contains(out, "GR010") {
		t.Errorf("plan on a cluster (exit %d):\n%s", code, out)
	}
	_, out, _ = runCluster(t, clustertest.New(clusterObjects()...), "scan", "--cluster", "-f", "sarif")
	if !strings.Contains(out, `"logicalLocations"`) || strings.Contains(out, `"physicalLocation"`) {
		t.Errorf("a cluster has no file: only logical locations are expected:\n%.400s", out)
	}
}

func TestScanWritesTheOtherReportsFromOneRun(t *testing.T) {
	dir := t.TempDir()
	jsonF, sarifF, planF := filepath.Join(dir, "r.json"), filepath.Join(dir, "r.sarif"), filepath.Join(dir, "plan.md")
	var asked atomic.Int32
	c := countingChecker{inner: fakeChecker{"public.ecr.aws/nginx/nginx:1.27": {Status: image.Arm64}}, calls: &asked}
	code, md, _ := run(t, "", c, "scan", sample, "--check-images", "-f", "markdown", "--json-file", jsonF, "--sarif-file", sarifF, "--plan-file", planF)
	if code != exitOK || !strings.HasPrefix(md, "<!-- graviton-readiness -->") {
		t.Fatalf("exit %d, stdout %.60q", code, md)
	}
	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if !strings.Contains(read(jsonF), `"schemaVersion": 1`) || !strings.Contains(read(sarifF), `"version": "2.1.0"`) || !strings.HasPrefix(read(planF), "<!-- graviton-readiness-plan -->") {
		t.Error("the three files must hold their formats")
	}
	// Five workloads, five distinct images: one lookup each, not three runs' worth.
	if n := asked.Load(); n != 5 {
		t.Errorf("the registry was asked %d times, want 5 (one per image)", n)
	}
	if code, _, _ := run(t, "", nil, "scan", sample, "--json-file", filepath.Join(dir, "no", "such", "r.json")); code != exitUsage {
		t.Errorf("an unwritable file must be an error, exit %d", code)
	}
	if code, _, errOut := run(t, "", nil, "plan", sample, "--sarif-file", sarifF); code != exitUsage || !strings.Contains(errOut, "belong to scan") {
		t.Errorf("plan: exit %d %q", code, errOut)
	}
}

type countingChecker struct {
	inner fakeChecker
	calls *atomic.Int32
}

func (c countingChecker) Check(ctx context.Context, ref string) image.Result {
	c.calls.Add(1) // CheckAll asks the registries in parallel
	return c.inner.Check(ctx, ref)
}
