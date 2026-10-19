package inventory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func load(t *testing.T, doc string) *Inventory {
	t.Helper()
	inv := &Inventory{Skipped: map[string]int{}}
	if err := inv.Add("f.yaml", []byte(doc)); err != nil {
		t.Fatal(err)
	}
	return inv
}

func TestKindsAndDefaults(t *testing.T) {
	inv := load(t, `
apiVersion: apps/v1
kind: Deployment
metadata: {name: web, namespace: shop}
spec:
  replicas: 3
  template:
    spec:
      containers:
        - {name: a, image: a:1, resources: {requests: {cpu: 250m}}}
        - {name: b, image: b:1, resources: {requests: {cpu: "1"}}}
---
apiVersion: apps/v1
kind: StatefulSet
metadata: {name: db}
spec:
  template: {spec: {containers: [{name: c, image: c:1}]}}
---
apiVersion: batch/v1
kind: CronJob
metadata: {name: nightly}
spec:
  schedule: "0 1 * * *"
  jobTemplate: {spec: {template: {spec: {containers: [{name: d, image: d:1}]}}}}
---
apiVersion: apps/v1
kind: ReplicaSet
metadata: {name: owned}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: ignored}
`)
	if len(inv.Workloads) != 3 {
		t.Fatalf("workloads = %d", len(inv.Workloads))
	}
	byName := map[string]Workload{}
	for _, w := range inv.Workloads {
		byName[w.Name] = w
	}
	web := byName["web"]
	if web.ID() != "shop/deployment/web" || web.Replicas != 3 || web.CPUMillis != 1250 || web.RequestedCPU() != 3750 || !web.LongRunning {
		t.Errorf("web = %+v", web)
	}
	if db := byName["db"]; db.Namespace != "default" || db.Replicas != 1 {
		t.Errorf("db defaults = %+v", db)
	}
	if job := byName["nightly"]; job.LongRunning || job.Pod.Containers[0].Image != "d:1" {
		t.Errorf("cronjob = %+v", job)
	}
	if inv.Skipped["ReplicaSet"] != 1 {
		t.Errorf("skipped = %v", inv.Skipped)
	}
}

func TestListJSONAndInitContainers(t *testing.T) {
	inv := load(t, `{"apiVersion":"v1","kind":"List","items":[
	  {"apiVersion":"v1","kind":"Pod","metadata":{"name":"p"},"spec":{
	    "initContainers":[{"name":"i","image":"i:1"}],
	    "containers":[{"name":"c","image":"c:1","resources":{"requests":{"cpu":"2"}}}]}}]}`)
	if len(inv.Workloads) != 1 {
		t.Fatalf("workloads = %d", len(inv.Workloads))
	}
	w := inv.Workloads[0]
	cs := w.Containers()
	if len(cs) != 2 || cs[0].Name != "i" || w.CPUMillis != 2000 {
		t.Errorf("containers = %v, cpu = %d", cs, w.CPUMillis)
	}
}

func TestInvalidDocumentIsAnError(t *testing.T) {
	inv := &Inventory{Skipped: map[string]int{}}
	if err := inv.Add("", []byte("kind: Deployment\nspec: [unterminated")); err == nil {
		t.Error("expected an error for a broken document")
	}
	if err := inv.Add("", []byte("# only a comment\n---\n")); err != nil {
		t.Errorf("empty documents should be fine: %v", err)
	}
}

func TestPathsWalksDirectoriesAndStdin(t *testing.T) {
	inv, err := Paths([]string{"../../testdata", "-"}, strings.NewReader("apiVersion: v1\nkind: Pod\nmetadata: {name: from-stdin}\nspec: {containers: [{name: c, image: c:1}]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Workloads) != 6 {
		t.Errorf("workloads = %d, want 6 (5 in sample.yaml and 1 from stdin)", len(inv.Workloads))
	}
	for i := 1; i < len(inv.Workloads); i++ {
		if inv.Workloads[i-1].ID() > inv.Workloads[i].ID() {
			t.Error("workloads are not sorted by ID")
		}
	}
}

func TestDirectoryWalkSkipsFilesThatDoNotParse(t *testing.T) {
	dir := t.TempDir()
	write := func(name, doc string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("good.yaml", "apiVersion: v1\nkind: Pod\nmetadata: {name: good}\nspec: {containers: [{name: c, image: c:1}]}\n")
	// A Helm template: valid YAML for the first document, then template syntax.
	write("template.yaml", "apiVersion: v1\nkind: Pod\nmetadata: {name: half}\nspec: {containers: [{name: c, image: c:1}]}\n---\nmetadata:\n  name: {{ .Release.Name }}\n")

	inv, err := Paths([]string{dir}, strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Workloads) != 1 || inv.Workloads[0].Name != "good" {
		t.Errorf("workloads = %+v, want only the good pod (nothing from the skipped file)", inv.Workloads)
	}
	if len(inv.Unreadable) != 1 || !strings.HasSuffix(inv.Unreadable[0].Path, "template.yaml") {
		t.Errorf("unreadable = %+v", inv.Unreadable)
	}

	// The same file named on the command line is an error.
	if _, err := Paths([]string{filepath.Join(dir, "template.yaml")}, strings.NewReader("")); err == nil {
		t.Error("an explicit file that does not parse must fail")
	}
}

func TestGenerateNameStandsInForTheName(t *testing.T) {
	inv := load(t, `
apiVersion: batch/v1
kind: Job
metadata: {generateName: migrate-, namespace: nps}
spec:
  template: {spec: {containers: [{name: m, image: m:1}]}}
`)
	if len(inv.Workloads) != 1 || inv.Workloads[0].ID() != "nps/job/migrate-*" {
		t.Errorf("workloads = %+v", inv.Workloads)
	}
}

func TestNodePools(t *testing.T) {
	inv := load(t, `
apiVersion: karpenter.sh/v1
kind: NodePool
metadata: {name: web}
spec:
  template:
    metadata:
      labels: {role: web}
    spec:
      requirements:
        - {key: kubernetes.io/arch, operator: In, values: [amd64]}
        - {key: karpenter.sh/capacity-type, operator: In, values: [spot, on-demand]}
---
apiVersion: karpenter.k8s.aws/v1
kind: EC2NodeClass
metadata: {name: default}
---
apiVersion: container.example.com/v1
kind: NodePool
metadata: {name: not-karpenter}
spec: {}
`)
	if len(inv.NodePools) != 1 {
		t.Fatalf("pools = %+v, want only the Karpenter one", inv.NodePools)
	}
	p := inv.NodePools[0]
	if p.Name != "web" || p.Labels["role"] != "web" || len(p.Requirements) != 2 || p.Requirements[0].Values[0] != "amd64" {
		t.Errorf("pool = %+v", p)
	}
}

func TestSkippedFileDoesNotLeaveNodePoolsBehind(t *testing.T) {
	dir := t.TempDir()
	doc := "apiVersion: karpenter.sh/v1\nkind: NodePool\nmetadata: {name: half}\nspec: {template: {spec: {requirements: []}}}\n---\nmetadata:\n  name: {{ .Release.Name }}\n"
	if err := os.WriteFile(filepath.Join(dir, "t.yaml"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	inv, err := Paths([]string{dir}, strings.NewReader(""))
	if err != nil || len(inv.NodePools) != 0 || len(inv.Unreadable) != 1 {
		t.Errorf("pools = %+v, unreadable = %+v, err = %v", inv.NodePools, inv.Unreadable, err)
	}
}

func TestDocumentsKnowTheirLine(t *testing.T) {
	text := `# a comment
apiVersion: v1
kind: Pod
metadata: {name: first}
spec:
  containers:
    - name: c
      image: first:1
---
apiVersion: v1
kind: Pod
metadata: {name: second}
spec:
  nodeSelector:
    kubernetes.io/arch: amd64
  containers:
    - name: c
      image: second:2
`
	inv := load(t, text)
	if len(inv.Workloads) != 2 {
		t.Fatalf("workloads = %d", len(inv.Workloads))
	}
	lines := strings.Split(text, "\n")
	at := func(line int) string { return strings.TrimSpace(lines[line-1]) }
	first, second := inv.Workloads[0], inv.Workloads[1]
	if first.Line != 1 || second.Line != 9 {
		t.Errorf("documents start at lines %d and %d, want 1 and 9", first.Line, second.Line)
	}
	// Every line that LineOf returns holds the text that was asked for.
	for _, c := range []struct {
		w      Workload
		needle string
	}{{first, "image:"}, {second, "image:"}, {second, "kubernetes.io/arch"}, {second, "NODESELECTOR"}} {
		line := c.w.LineOf(c.needle)
		if line == 0 || !strings.Contains(strings.ToLower(at(line)), strings.ToLower(c.needle)) {
			t.Errorf("%s: LineOf(%q) = %d, which holds %q", c.w.Name, c.needle, line, at(line))
		}
	}
	// With nothing to look for, or nothing found, the line of the kind: is returned.
	if l := second.LineOf(""); at(l) != "kind: Pod" || l != 11 {
		t.Errorf("LineOf(\"\") = %d (%q)", l, at(l))
	}
	if l := second.LineOf("not in the manifest"); l != 11 {
		t.Errorf("a missing needle falls back to the kind: line, got %d", l)
	}
	// A second image lookup finds the line of that document, not of the first one.
	if second.LineOf("image:") <= first.LineOf("image:") {
		t.Error("the second document's image is below the first's")
	}
	if (Workload{}).LineOf("x") != 0 {
		t.Error("a workload without a file has no line")
	}
}

func TestJSONFileAndListKeepALine(t *testing.T) {
	inv := load(t, `{"apiVersion":"v1","kind":"List","items":[
  {"apiVersion":"v1","kind":"Pod","metadata":{"name":"a"},"spec":{"containers":[{"name":"c","image":"a:1"}]}}]}`)
	if len(inv.Workloads) != 1 || inv.Workloads[0].Line != 1 {
		t.Errorf("workloads = %+v", inv.Workloads)
	}
}

func TestEC2NodeClassesAndLinking(t *testing.T) {
	inv := load(t, `
apiVersion: karpenter.sh/v1
kind: NodePool
metadata: {name: win}
spec:
  template:
    spec:
      nodeClassRef: {group: karpenter.k8s.aws, kind: EC2NodeClass, name: windows}
      requirements:
        - {key: kubernetes.io/arch, operator: In, values: [amd64, arm64]}
---
apiVersion: karpenter.sh/v1
kind: NodePool
metadata: {name: linux}
spec:
  template:
    spec:
      nodeClassRef: {name: default}
      requirements:
        - {key: kubernetes.io/arch, operator: In, values: [amd64, arm64]}
---
apiVersion: karpenter.sh/v1
kind: NodePool
metadata: {name: alias}
spec:
  template:
    spec:
      nodeClassRef: {name: aliased}
---
apiVersion: karpenter.k8s.aws/v1
kind: EC2NodeClass
metadata: {name: windows}
spec: {amiFamily: Windows2022}
---
apiVersion: karpenter.k8s.aws/v1
kind: EC2NodeClass
metadata: {name: default}
spec:
  amiSelectorTerms: [{alias: al2023@latest}]
---
apiVersion: karpenter.k8s.aws/v1
kind: EC2NodeClass
metadata: {name: aliased}
spec:
  amiSelectorTerms: [{alias: windows2019@latest}]
---
apiVersion: other.example.com/v1
kind: EC2NodeClass
metadata: {name: not-karpenter}
spec: {amiFamily: Windows2022}
`)
	if len(inv.NodeClasses) != 3 || len(inv.NodePools) != 3 {
		t.Fatalf("classes %d, pools %d", len(inv.NodeClasses), len(inv.NodePools))
	}
	if inv.NodePools[0].NodeClass != "windows" {
		t.Errorf("nodeClassRef = %q", inv.NodePools[0].NodeClass)
	}
	if !inv.NodeClasses[0].Windows() || inv.NodeClasses[1].Windows() || !inv.NodeClasses[2].Windows() {
		t.Error("Windows2022 and a windows2019@latest alias are Windows; al2023@latest is not")
	}
	inv.LinkNodeClasses()
	extra := func(i int) int { return len(inv.NodePools[i].Requirements) }
	if extra(0) != 2 || inv.NodePools[0].Requirements[1].Reason == "" || !strings.Contains(inv.NodePools[0].Requirements[1].Reason, "Windows") {
		t.Errorf("the Windows pool must get a requirement with a reason: %+v", inv.NodePools[0].Requirements)
	}
	if extra(1) != 1 {
		t.Errorf("a Linux class adds nothing: %+v", inv.NodePools[1].Requirements)
	}
	if extra(2) != 1 || !strings.Contains(inv.NodePools[2].Requirements[0].Reason, "windows2019@latest") {
		t.Errorf("an alias is a Windows AMI too: %+v", inv.NodePools[2].Requirements)
	}
}

func TestNodesAndRollouts(t *testing.T) {
	inv := load(t, `
apiVersion: v1
kind: Node
metadata:
  name: n1
  labels: {kubernetes.io/arch: arm64, role: batch}
---
apiVersion: v1
kind: Node
metadata:
  name: n2
  labels: {beta.kubernetes.io/arch: amd64}
---
apiVersion: v1
kind: Node
metadata: {name: n3}
---
apiVersion: argoproj.io/v1alpha1
kind: Rollout
metadata: {name: canary, namespace: shop}
spec:
  replicas: 3
  template:
    spec:
      containers: [{name: app, image: a:1, resources: {requests: {cpu: 200m}}}]
---
apiVersion: argoproj.io/v1alpha1
kind: Rollout
metadata: {name: by-ref, namespace: shop}
spec:
  workloadRef: {apiVersion: apps/v1, kind: Deployment, name: api}
---
apiVersion: other.example.com/v1
kind: Rollout
metadata: {name: not-argo}
spec:
  template: {spec: {containers: [{name: c, image: c:1}]}}
`)
	if len(inv.Nodes) != 3 || inv.Nodes[0].Arch() != "arm64" || inv.Nodes[1].Arch() != "amd64" || inv.Nodes[2].Arch() != "" {
		t.Errorf("nodes = %+v (the beta label counts, and a node may not say)", inv.Nodes)
	}
	if inv.Nodes[0].Labels["role"] != "batch" {
		t.Error("labels are kept")
	}
	if len(inv.Workloads) != 1 || inv.Workloads[0].ID() != "shop/rollout/canary" || inv.Workloads[0].Replicas != 3 || !inv.Workloads[0].LongRunning {
		t.Errorf("workloads = %+v (only the Argo Rollout with a template counts)", inv.Workloads)
	}
	if inv.Skipped["Rollout (workloadRef)"] != 1 {
		t.Errorf("skipped = %v", inv.Skipped)
	}
}
