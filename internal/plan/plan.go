// Package plan turns the verdicts into an order of work: what can move to Graviton now, what
// after a look, what after a change, what has to be verified first and what stays on amd64.
package plan

import (
	"sort"
	"strings"

	"github.com/robertasolimandonofreo/graviton-readiness/internal/inventory"
	"github.com/robertasolimandonofreo/graviton-readiness/internal/rules"
)

// Group is where a workload falls in the plan.
type Group string

const (
	// ReadyNow has no blocker and no caveat, and its images are known to run on arm64: the first
	// phase, and a good pilot.
	ReadyNow Group = "ready-now"
	// ReadyAfterCheck is ready, with a caveat that a person has to look at (an architecture
	// written in a command, a GPU).
	ReadyAfterCheck Group = "ready-after-check"
	// ReadyAfterChanges has blockers that a change in the manifest or in the image build removes.
	ReadyAfterChanges Group = "ready-after-changes"
	// VerifyFirst has nothing that blocks it in the configuration, but its images were not
	// checked, or the registry did not answer.
	VerifyFirst Group = "verify-first"
	// KeepOnAMD64 has a blocker that no change removes: it needs an architecture that Graviton
	// does not have (Windows).
	KeepOnAMD64 Group = "keep-on-amd64"
	// AlreadyOnGraviton can only land on arm64 nodes (it selects arm64 or an arm64-only pool) and
	// has no blocker: it is done, and moving it is not part of the plan.
	AlreadyOnGraviton Group = "already-on-graviton"
)

// Groups lists the groups in the order of the plan.
var Groups = []Group{ReadyNow, ReadyAfterCheck, ReadyAfterChanges, VerifyFirst, KeepOnAMD64, AlreadyOnGraviton}

// Change is one thing to do for a workload.
type Change struct {
	Rule string `json:"rule"`
	// Action is the first sentence of the fix of the rule.
	Action string `json:"action"`
	// Detail is what the rule found in this workload.
	Detail string `json:"detail"`
}

// Item is one workload in the plan.
type Item struct {
	Workload inventory.Workload `json:"-"`
	Score    int                `json:"score"`
	Group    Group              `json:"group"`
	Changes  []Change           `json:"changes"`
	// CPUMillis is the CPU the workload requests in total. It is zero for the objects that do not
	// have a replica count to weigh by (DaemonSets, Jobs, CronJobs).
	CPUMillis int64 `json:"cpuRequestMillis"`
	// Priority orders ReadyAfterChanges: the CPU that would move, divided by the number of
	// changes it takes. A big workload that needs one change goes before a small one that needs
	// four. It is a way to order the work, not a forecast of savings.
	Priority float64 `json:"-"`
}

// Totals counts the groups and weighs them by requested CPU.
type Totals struct {
	Total        int             `json:"total"`
	Counts       map[Group]int   `json:"counts"`
	CPU          map[Group]int64 `json:"cpuMillis"`
	RequestedCPU int64           `json:"requestedCPUMillis"`
	// NowPercent is the CPU that can move now, as a share of the requested CPU. PotentialPercent
	// adds what moves after a look and after changes. Unverified is never added to either.
	NowPercent       int `json:"movableNowPercent"`
	PotentialPercent int `json:"potentialAfterChangesPercent"`
	VerifyPercent    int `json:"verifyFirstPercent"`
	// OnGravitonPercent is the CPU that already runs on Graviton, as a share of the requested CPU.
	// It is not part of the other two: those are about what is left to move.
	OnGravitonPercent int `json:"alreadyOnGravitonPercent"`
}

// Plan is the result.
type Plan struct {
	Items  []Item `json:"-"`
	Totals Totals `json:"totals"`
}

// Build groups the assessments and orders each group.
func Build(as []rules.Assessment) Plan {
	p := Plan{Totals: Totals{Counts: map[Group]int{}, CPU: map[Group]int64{}, Total: len(as)}}
	for _, a := range as {
		it := Item{Workload: a.Workload, Score: a.Score, Changes: []Change{}}
		if a.Workload.LongRunning {
			it.CPUMillis = a.Workload.RequestedCPU()
		}
		switch a.Verdict {
		case rules.Ready:
			it.Group = ReadyNow
			if a.OnGraviton {
				it.Group = AlreadyOnGraviton
			}
		case rules.ReadyWithCaveats:
			it.Group = ReadyAfterCheck
			it.Changes = changes(a, rules.Caveat)
			if a.OnGraviton {
				it.Group = AlreadyOnGraviton
			}
		case rules.NotReady:
			it.Group = ReadyAfterChanges
			if hasRule(a, "GR009") {
				it.Group = KeepOnAMD64
			}
			it.Changes = changes(a, rules.Blocker)
		default:
			it.Group = VerifyFirst
			it.Changes = verifyChanges(a)
		}
		if it.Group == ReadyAfterChanges {
			n := len(it.Changes)
			if n == 0 {
				n = 1
			}
			it.Priority = float64(it.CPUMillis) / float64(n)
		}
		p.Items = append(p.Items, it)
		p.Totals.Counts[it.Group]++
		p.Totals.CPU[it.Group] += it.CPUMillis
		p.Totals.RequestedCPU += it.CPUMillis
	}
	sort.SliceStable(p.Items, func(i, j int) bool {
		a, b := p.Items[i], p.Items[j]
		if ga, gb := order(a.Group), order(b.Group); ga != gb {
			return ga < gb
		}
		if a.Priority != b.Priority {
			return a.Priority > b.Priority
		}
		if a.CPUMillis != b.CPUMillis {
			return a.CPUMillis > b.CPUMillis
		}
		return a.Workload.ID() < b.Workload.ID()
	})
	if t := &p.Totals; t.RequestedCPU > 0 {
		now, check, chg := t.CPU[ReadyNow], t.CPU[ReadyAfterCheck], t.CPU[ReadyAfterChanges]
		t.NowPercent = int(100 * now / t.RequestedCPU)
		t.PotentialPercent = int(100 * (now + check + chg) / t.RequestedCPU)
		t.VerifyPercent = int(100 * t.CPU[VerifyFirst] / t.RequestedCPU)
		t.OnGravitonPercent = int(100 * t.CPU[AlreadyOnGraviton] / t.RequestedCPU)
	}
	return p
}

func order(g Group) int {
	for i, x := range Groups {
		if x == g {
			return i
		}
	}
	return len(Groups)
}

func hasRule(a rules.Assessment, id string) bool {
	for _, f := range a.Findings {
		if f.Rule == id {
			return true
		}
	}
	return false
}

// changes lists one change per rule of the given severity, in the order of the rules.
func changes(a rules.Assessment, sev rules.Severity) []Change {
	out := []Change{}
	seen := map[string]bool{}
	for _, f := range a.Findings {
		if f.Severity != sev || seen[f.Rule] {
			continue
		}
		seen[f.Rule] = true
		out = append(out, Change{Rule: f.Rule, Action: action(f.Rule), Detail: f.Message})
	}
	return out
}

// verifyChanges says what it takes to verify a workload: read its images, or fix the access to
// the registry that did not answer.
func verifyChanges(a rules.Assessment) []Change {
	out := []Change{}
	for _, f := range a.Findings {
		if f.Rule == "GR005" {
			out = append(out, Change{Rule: "GR005", Action: action("GR005"), Detail: f.Message})
		}
	}
	if len(out) > 0 {
		return out
	}
	for _, i := range a.Images {
		if i.Status == "not-checked" {
			return []Change{{Rule: "", Action: "Run with --check-images, to read the platforms of its images from the registry.",
				Detail: "image " + i.Image + " was not checked"}}
		}
	}
	return out
}

// action is the first sentence of the fix of a rule.
func action(id string) string {
	r, ok := rules.Lookup(id)
	if !ok {
		return ""
	}
	if i := strings.Index(r.Fix, ". "); i >= 0 {
		return r.Fix[:i+1]
	}
	return r.Fix
}
