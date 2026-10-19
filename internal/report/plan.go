package report

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/robertasolimandonofreo/graviton-readiness/internal/plan"
)

var groupTitle = map[plan.Group]string{
	plan.ReadyNow:          "Ready now",
	plan.ReadyAfterCheck:   "Ready after a check",
	plan.ReadyAfterChanges: "Ready after changes",
	plan.VerifyFirst:       "Verify first",
	plan.KeepOnAMD64:       "Keep on amd64",
	plan.AlreadyOnGraviton: "Already on Graviton",
}

var groupHelp = map[plan.Group]string{
	plan.ReadyNow:          "no blocker, no caveat, and every image runs on arm64. The safest first move, and a good pilot.",
	plan.ReadyAfterCheck:   "ready, with a caveat that a person has to look at first.",
	plan.ReadyAfterChanges: "blockers that a change in the manifest or in the image build removes. The best return for the effort first.",
	plan.VerifyFirst:       "nothing blocks it in the configuration, but its images were not checked, or the registry did not answer.",
	plan.KeepOnAMD64:       "needs an architecture that Graviton does not have (Windows). No change fixes it.",
	plan.AlreadyOnGraviton: "can only land on arm64 nodes (it selects arm64, or a pool that only has arm64 nodes). Done: not part of what is left to move.",
}

func vcpu(m int64) string { return fmt.Sprintf("%.1f vCPU", float64(m)/1000) }

func cpuLabel(m int64) string {
	if m == 0 {
		return ""
	}
	return " (" + vcpu(m) + ")"
}

// PlanTable writes the plan for a terminal.
func PlanTable(out io.Writer, p plan.Plan, o Options) error {
	t := p.Totals
	fmt.Fprintln(out, "GRAVITON MIGRATION PLAN")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Total workloads:          %d\n", t.Total)
	for _, g := range plan.Groups {
		fmt.Fprintf(out, "%-26s%d\n", groupTitle[g]+":", t.Counts[g])
	}
	if t.RequestedCPU > 0 {
		fmt.Fprintf(out, "\nRequested CPU:            %s\n", vcpu(t.RequestedCPU))
		if t.CPU[plan.AlreadyOnGraviton] > 0 {
			fmt.Fprintf(out, "Already on Graviton:      %s (%d%%), not counted below\n", vcpu(t.CPU[plan.AlreadyOnGraviton]), t.OnGravitonPercent)
		}
		fmt.Fprintf(out, "Movable now:              %s (%d%%)\n", vcpu(t.CPU[plan.ReadyNow]), t.NowPercent)
		fmt.Fprintf(out, "Potential after changes:  %s (%d%%)\n", vcpu(t.CPU[plan.ReadyNow]+t.CPU[plan.ReadyAfterCheck]+t.CPU[plan.ReadyAfterChanges]), t.PotentialPercent)
		if t.CPU[plan.VerifyFirst] > 0 {
			fmt.Fprintf(out, "To verify first:          %s (%d%%), not counted in the potential\n", vcpu(t.CPU[plan.VerifyFirst]), t.VerifyPercent)
		}
	}
	n := 0
	for _, g := range plan.Groups {
		var items []plan.Item
		for _, it := range p.Items {
			if it.Group == g {
				items = append(items, it)
			}
		}
		if len(items) == 0 {
			continue
		}
		n++
		fmt.Fprintf(out, "\nPhase %d: %s\n  %s\n", n, groupTitle[g], groupHelp[g])
		for i, it := range items {
			prefix := "  "
			if g == plan.ReadyAfterChanges {
				prefix = fmt.Sprintf("  %d. ", i+1)
			}
			fmt.Fprintf(out, "%s%s%s\n", prefix, it.Workload.ID(), cpuLabel(it.CPUMillis))
			for _, c := range it.Changes {
				rule := c.Rule
				if rule == "" {
					rule = "   "
				}
				fmt.Fprintf(out, "       %s  %s\n", rule, c.Action)
				if o.Verbose && c.Detail != "" {
					fmt.Fprintf(out, "             %s\n", c.Detail)
				}
			}
		}
	}
	if !o.ImagesChecked {
		fmt.Fprintln(out, "\nImages were not checked: run with --check-images to read their platforms from the registry.")
	}
	return nil
}

type planJSONItem struct {
	ID        string        `json:"id"`
	Kind      string        `json:"kind"`
	Namespace string        `json:"namespace"`
	Name      string        `json:"name"`
	File      string        `json:"file,omitempty"`
	Score     int           `json:"score"`
	CPUMillis int64         `json:"cpuRequestMillis"`
	Changes   []plan.Change `json:"changes"`
}

// PlanJSON writes the plan for a machine: the totals, then the workloads of each group in order.
func PlanJSON(out io.Writer, p plan.Plan, o Options) error {
	groups := map[plan.Group][]planJSONItem{}
	for _, g := range plan.Groups {
		groups[g] = []planJSONItem{}
	}
	for _, it := range p.Items {
		w := it.Workload
		groups[it.Group] = append(groups[it.Group], planJSONItem{ID: w.ID(), Kind: w.Kind, Namespace: w.Namespace, Name: w.Name,
			File: w.File, Score: it.Score, CPUMillis: it.CPUMillis, Changes: it.Changes})
	}
	doc := struct {
		SchemaVersion int                           `json:"schemaVersion"`
		ToolVersion   string                        `json:"toolVersion"`
		ImagesChecked bool                          `json:"imagesChecked"`
		Totals        plan.Totals                   `json:"totals"`
		Groups        map[plan.Group][]planJSONItem `json:"groups"`
	}{SchemaVersion: SchemaVersion, ToolVersion: o.Version, ImagesChecked: o.ImagesChecked, Totals: p.Totals, Groups: groups}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

// PlanMarkdown writes the plan for a pull request comment. Its marker differs from the one of the
// scan report, so both can live in the same pull request.
func PlanMarkdown(out io.Writer, p plan.Plan, o Options) error {
	t := p.Totals
	fmt.Fprintln(out, "<!-- graviton-readiness-plan -->")
	fmt.Fprintln(out, "## Graviton migration plan")
	fmt.Fprintf(out, "\n%d workloads: **%d** already on Graviton, **%d** ready now, **%d** after a check, **%d** after changes, **%d** to verify first, **%d** to keep on amd64.\n",
		t.Total, t.Counts[plan.AlreadyOnGraviton], t.Counts[plan.ReadyNow], t.Counts[plan.ReadyAfterCheck], t.Counts[plan.ReadyAfterChanges], t.Counts[plan.VerifyFirst], t.Counts[plan.KeepOnAMD64])
	if t.RequestedCPU > 0 {
		fmt.Fprintf(out, "\nMovable now: **%d%%** of the requested CPU. Potential after changes: **%d%%**.", t.NowPercent, t.PotentialPercent)
		if t.VerifyPercent > 0 {
			fmt.Fprintf(out, " Another %d%% needs its images verified first.", t.VerifyPercent)
		}
		fmt.Fprintln(out)
	}
	if !o.ImagesChecked {
		fmt.Fprintln(out, "\n> Images were not checked. Run with `--check-images` to read their platforms from the registry.")
	}
	const limit = 25
	for _, g := range plan.Groups {
		if g == plan.ReadyNow {
			continue
		}
		var items []plan.Item
		for _, it := range p.Items {
			if it.Group == g {
				items = append(items, it)
			}
		}
		if len(items) == 0 {
			continue
		}
		fmt.Fprintf(out, "\n### %s (%d)\n\n| Workload | CPU | What to do |\n| --- | --- | --- |\n", groupTitle[g], len(items))
		for i, it := range items {
			if i == limit {
				fmt.Fprintf(out, "\n%d more workloads not shown.\n", len(items)-limit)
				break
			}
			todo := ""
			for j, c := range it.Changes {
				if j > 0 {
					todo += "<br>"
				}
				todo += "`" + c.Rule + "` " + c.Action
			}
			cpu := "-"
			if it.CPUMillis > 0 {
				cpu = vcpu(it.CPUMillis)
			}
			fmt.Fprintf(out, "| `%s` | %s | %s |\n", it.Workload.ID(), cpu, todo)
		}
	}
	return nil
}
