package plan_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/robertasolimandonofreo/graviton-readiness/internal/image"
	"github.com/robertasolimandonofreo/graviton-readiness/internal/inventory"
	"github.com/robertasolimandonofreo/graviton-readiness/internal/plan"
	"github.com/robertasolimandonofreo/graviton-readiness/internal/rules"
)

// workload builds a Deployment. extra goes into the pod spec, res is the content of resources.
func workload(t *testing.T, name string, replicas int, extra, res, img string) inventory.Workload {
	t.Helper()
	doc := fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata: {name: %s, namespace: shop}
spec:
  replicas: %d
  template:
    spec:
%s      containers:
        - {name: c, image: %q, resources: {%s}}
`, name, replicas, extra, img, res)
	inv := inventory.New()
	if err := inv.Add("", []byte(doc)); err != nil {
		t.Fatal(err)
	}
	return inv.Workloads[0]
}

const pinned = "      nodeSelector: {kubernetes.io/arch: amd64}\n"

func assessments(t *testing.T) []rules.Assessment {
	t.Helper()
	images := map[string]image.Result{
		"arm:1": {Status: image.Arm64},
		"x86:1": {Status: image.NoArm64, Platforms: []string{"linux/amd64"}},
	}
	checked := rules.Options{ImagesChecked: true}
	var out []rules.Assessment
	for _, w := range []inventory.Workload{
		workload(t, "ready-small", 1, "", "requests: {cpu: 100m}", "arm:1"),
		workload(t, "ready-big", 4, "", "requests: {cpu: 500m}", "arm:1"),
		workload(t, "gpu", 1, "", "requests: {cpu: 1}, limits: {nvidia.com/gpu: 1}", "arm:1"),
		// pinned to amd64 and an image without arm64: two changes, 2 vCPU, priority 1000
		workload(t, "two-changes", 2, pinned, "requests: {cpu: 1}", "x86:1"),
		// one change, 1 vCPU, priority 1000: ties with two-changes, which has more CPU and goes first
		workload(t, "one-change", 1, pinned, "requests: {cpu: 1}", "arm:1"),
		// one change and 8 vCPU: the best return, first
		workload(t, "big-one-change", 8, pinned, "requests: {cpu: 1}", "arm:1"),
		workload(t, "windows", 1, "      nodeSelector: {kubernetes.io/os: windows}\n", "requests: {cpu: 2}", "arm:1"),
	} {
		out = append(out, rules.Evaluate(w, images, checked))
	}
	// images that were never checked
	out = append(out, rules.Evaluate(workload(t, "unchecked", 2, "", "requests: {cpu: 250m}", "arm:1"), nil, rules.Options{}))
	return out
}

func ids(items []plan.Item, g plan.Group) []string {
	var out []string
	for _, it := range items {
		if it.Group == g {
			out = append(out, it.Workload.Name)
		}
	}
	return out
}

func TestGroups(t *testing.T) {
	p := plan.Build(assessments(t))
	want := map[plan.Group]string{
		plan.ReadyNow:          "ready-big,ready-small",
		plan.ReadyAfterCheck:   "gpu",
		plan.ReadyAfterChanges: "big-one-change,two-changes,one-change",
		plan.VerifyFirst:       "unchecked",
		plan.KeepOnAMD64:       "windows",
	}
	for g, w := range want {
		if got := strings.Join(ids(p.Items, g), ","); got != w {
			t.Errorf("%s = %q, want %q", g, got, w)
		}
	}
	// The items come out in the order of the groups.
	last := -1
	order := map[plan.Group]int{}
	for i, g := range plan.Groups {
		order[g] = i
	}
	for _, it := range p.Items {
		if order[it.Group] < last {
			t.Errorf("%s comes after a later group", it.Workload.Name)
		}
		last = order[it.Group]
	}
}

func TestChangesAndPriority(t *testing.T) {
	p := plan.Build(assessments(t))
	by := map[string]plan.Item{}
	for _, it := range p.Items {
		by[it.Workload.Name] = it
	}
	two := by["two-changes"]
	if len(two.Changes) != 2 || two.Changes[0].Rule != "GR001" || two.Changes[1].Rule != "GR004" {
		t.Errorf("two-changes = %+v", two.Changes)
	}
	if !strings.HasPrefix(two.Changes[0].Action, "Remove the kubernetes.io/arch constraint") || strings.Contains(two.Changes[0].Action, ". ") {
		t.Errorf("the action is the first sentence of the fix: %q", two.Changes[0].Action)
	}
	if two.Changes[0].Detail == "" {
		t.Error("the detail of what was found is kept")
	}
	// 8 vCPU / 1 change beats 2 vCPU / 2 changes (1000) and 1 vCPU / 1 change (1000).
	if by["big-one-change"].Priority != 8000 || two.Priority != 1000 || by["one-change"].Priority != 1000 {
		t.Errorf("priorities: %v %v %v", by["big-one-change"].Priority, two.Priority, by["one-change"].Priority)
	}
	if got := strings.Join(ids(p.Items, plan.ReadyAfterChanges), ","); got != "big-one-change,two-changes,one-change" {
		t.Errorf("a tie goes to the workload with more CPU: %s", got)
	}
	if c := by["gpu"].Changes; len(c) != 1 || c[0].Rule != "GR007" {
		t.Errorf("a caveat is a thing to check: %+v", c)
	}
	if c := by["windows"].Changes; len(c) != 1 || c[0].Rule != "GR009" {
		t.Errorf("windows = %+v", c)
	}
	if c := by["unchecked"].Changes; len(c) != 1 || !strings.Contains(c[0].Action, "--check-images") {
		t.Errorf("unchecked = %+v", c)
	}
	if len(by["ready-big"].Changes) != 0 {
		t.Error("a ready workload has nothing to change")
	}
}

func TestTotals(t *testing.T) {
	p := plan.Build(assessments(t))
	tot := p.Totals
	// requested CPU: ready 100m+2000m, gpu 1000m, changes 2000m+1000m+8000m, windows 2000m, unchecked 500m
	if tot.Total != 8 || tot.RequestedCPU != 16600 {
		t.Fatalf("total %d, requested %d", tot.Total, tot.RequestedCPU)
	}
	for g, want := range map[plan.Group]int64{plan.ReadyNow: 2100, plan.ReadyAfterCheck: 1000, plan.ReadyAfterChanges: 11000, plan.KeepOnAMD64: 2000, plan.VerifyFirst: 500} {
		if tot.CPU[g] != want {
			t.Errorf("%s CPU = %d, want %d", g, tot.CPU[g], want)
		}
	}
	// 2100/16600 = 12%; (2100+1000+11000)/16600 = 84%; unverified 500/16600 = 3% and never added.
	if tot.NowPercent != 12 || tot.PotentialPercent != 84 || tot.VerifyPercent != 3 {
		t.Errorf("percents now %d potential %d verify %d", tot.NowPercent, tot.PotentialPercent, tot.VerifyPercent)
	}
	if tot.Counts[plan.ReadyAfterChanges] != 3 || tot.Counts[plan.KeepOnAMD64] != 1 {
		t.Errorf("counts = %v", tot.Counts)
	}
}

func TestNothingToPlan(t *testing.T) {
	p := plan.Build(nil)
	if p.Totals.Total != 0 || p.Totals.NowPercent != 0 || len(p.Items) != 0 {
		t.Errorf("an empty plan: %+v", p.Totals)
	}
}
