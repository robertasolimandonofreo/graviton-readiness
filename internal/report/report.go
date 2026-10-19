// Package report turns assessments into a table, JSON or Markdown.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/robertasolimandonofreo/graviton-readiness/internal/inventory"
	"github.com/robertasolimandonofreo/graviton-readiness/internal/rules"
)

// SchemaVersion changes when the JSON layout changes in a way that breaks readers.
const SchemaVersion = 1

// Summary counts the verdicts and weighs them by requested CPU.
type Summary struct {
	Total            int                   `json:"total"`
	Counts           map[rules.Verdict]int `json:"counts"`
	RequestedCPU     int64                 `json:"requestedCPUMillis"`
	PotentialCPU     int64                 `json:"potentialCPUMillis"`
	UnverifiedCPU    int64                 `json:"unverifiedCPUMillis"`
	PotentialPercent int                   `json:"potentialPercent"`
	Unverified       int                   `json:"unverifiedPercent"`
	Skipped          map[string]int        `json:"skipped,omitempty"`
}

// Summarize computes the counts. The CPU figures only cover long-running workloads
// (Deployments, StatefulSets, Pods): a Job or a DaemonSet has no replica count to weigh by, and
// a Job asks for CPU only while it runs. Potential is the CPU of workloads that are ready or
// ready with caveats, with their images verified; unverified is reported apart, never added.
func Summarize(as []rules.Assessment, skipped map[string]int) Summary {
	s := Summary{Total: len(as), Counts: map[rules.Verdict]int{}, Skipped: skipped}
	for _, a := range as {
		s.Counts[a.Verdict]++
		if !a.Workload.LongRunning {
			continue
		}
		cpu := a.Workload.RequestedCPU()
		s.RequestedCPU += cpu
		switch a.Verdict {
		case rules.Ready, rules.ReadyWithCaveats:
			s.PotentialCPU += cpu
		case rules.Unverified:
			s.UnverifiedCPU += cpu
		}
	}
	if s.RequestedCPU > 0 {
		s.PotentialPercent = int(100 * s.PotentialCPU / s.RequestedCPU)
		s.Unverified = int(100 * s.UnverifiedCPU / s.RequestedCPU)
	}
	return s
}

// Options tune the output.
type Options struct {
	Verbose       bool
	ImagesChecked bool
	Version       string
}

func cpu(m int64) string {
	if m == 0 {
		return "-"
	}
	return fmt.Sprintf("%dm", m)
}

func ruleIDs(a rules.Assessment) string {
	seen := map[string]bool{}
	var ids []string
	for _, f := range a.Findings {
		if !seen[f.Rule] {
			seen[f.Rule] = true
			ids = append(ids, f.Rule)
		}
	}
	if len(ids) == 0 {
		return "-"
	}
	return strings.Join(ids, ",")
}

func replicas(w inventory.Workload) string {
	if !w.LongRunning {
		return "-"
	}
	return fmt.Sprint(w.Replicas)
}

// Table writes the default report.
func Table(out io.Writer, as []rules.Assessment, skipped map[string]int, o Options) error {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "VERDICT\tSCORE\tWORKLOAD\tREPLICAS\tCPU\tFINDINGS")
	for _, a := range as {
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\n", a.Verdict, a.Score, a.Workload.ID(),
			replicas(a.Workload), cpu(a.Workload.CPUMillis), ruleIDs(a))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if o.Verbose {
		for _, a := range as {
			if len(a.Findings) == 0 && !hasUnverifiedImage(a) {
				continue
			}
			fmt.Fprintf(out, "\n%s\n", a.Workload.ID())
			for _, f := range a.Findings {
				fmt.Fprintf(out, "  %s [%s] %s\n", f.Rule, f.Severity, f.Message)
			}
			if len(a.Findings) == 0 && !o.ImagesChecked {
				fmt.Fprintln(out, "  images were not checked (use --check-images)")
			}
		}
	}
	return summaryText(out, Summarize(as, skipped), o)
}

func hasUnverifiedImage(a rules.Assessment) bool {
	for _, i := range a.Images {
		if i.Status == "not-checked" {
			return true
		}
	}
	return false
}

func summaryText(out io.Writer, s Summary, o Options) error {
	c := s.Counts
	fmt.Fprintf(out, "\n%d workloads: %d ready, %d with caveats, %d not ready, %d unverified\n",
		s.Total, c[rules.Ready], c[rules.ReadyWithCaveats], c[rules.NotReady], c[rules.Unverified])
	if s.RequestedCPU > 0 {
		fmt.Fprintf(out, "Graviton potential: %d%% of requested CPU (workloads ready or with caveats).\n", s.PotentialPercent)
		if s.Unverified > 0 {
			fmt.Fprintf(out, "Unverified: %d%% of requested CPU could move if the images support arm64.\n", s.Unverified)
		}
	}
	if !o.ImagesChecked {
		fmt.Fprintln(out, "Images were not checked: run with --check-images to read their platforms from the registry.")
	}
	if n := total(s.Skipped); n > 0 {
		fmt.Fprintf(out, "%d objects skipped (%s).\n", n, joinCounts(s.Skipped))
	}
	return nil
}

func total(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

func joinCounts(m map[string]int) string {
	var parts []string
	for _, k := range []string{"ReplicaSet", "ReplicationController"} {
		if m[k] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", m[k], k))
		}
	}
	return strings.Join(parts, ", ")
}

type jsonWorkload struct {
	ID        string              `json:"id"`
	Kind      string              `json:"kind"`
	Namespace string              `json:"namespace"`
	Name      string              `json:"name"`
	File      string              `json:"file,omitempty"`
	Replicas  *int32              `json:"replicas,omitempty"`
	CPUMillis int64               `json:"cpuRequestMillis"`
	Score     int                 `json:"score"`
	Verdict   rules.Verdict       `json:"verdict"`
	Findings  []rules.Finding     `json:"findings"`
	Images    []rules.ImageStatus `json:"images,omitempty"`
}

// JSON writes a machine-readable report that starts with schemaVersion and toolVersion.
func JSON(out io.Writer, as []rules.Assessment, skipped map[string]int, o Options) error {
	doc := struct {
		SchemaVersion int            `json:"schemaVersion"`
		ToolVersion   string         `json:"toolVersion"`
		ImagesChecked bool           `json:"imagesChecked"`
		Summary       Summary        `json:"summary"`
		Workloads     []jsonWorkload `json:"workloads"`
	}{SchemaVersion: SchemaVersion, ToolVersion: o.Version, ImagesChecked: o.ImagesChecked,
		Summary: Summarize(as, skipped), Workloads: []jsonWorkload{}}
	for _, a := range as {
		w := jsonWorkload{
			ID: a.Workload.ID(), Kind: a.Workload.Kind, Namespace: a.Workload.Namespace,
			Name: a.Workload.Name, File: a.Workload.File, CPUMillis: a.Workload.CPUMillis,
			Score: a.Score, Verdict: a.Verdict, Findings: a.Findings, Images: a.Images,
		}
		if a.Workload.LongRunning {
			r := a.Workload.Replicas
			w.Replicas = &r
		}
		doc.Workloads = append(doc.Workloads, w)
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

// Markdown writes a report for a pull request comment: a summary and the workloads that need
// attention, worst first.
func Markdown(out io.Writer, as []rules.Assessment, skipped map[string]int, o Options) error {
	s := Summarize(as, skipped)
	c := s.Counts
	fmt.Fprintln(out, "<!-- graviton-readiness -->")
	fmt.Fprintln(out, "## Graviton readiness")
	fmt.Fprintf(out, "\n%d workloads: **%d** ready, **%d** with caveats, **%d** not ready, **%d** unverified.\n",
		s.Total, c[rules.Ready], c[rules.ReadyWithCaveats], c[rules.NotReady], c[rules.Unverified])
	if s.RequestedCPU > 0 {
		fmt.Fprintf(out, "\nGraviton potential: **%d%%** of requested CPU.", s.PotentialPercent)
		if s.Unverified > 0 {
			fmt.Fprintf(out, " Another %d%% is unverified.", s.Unverified)
		}
		fmt.Fprintln(out)
	}
	if !o.ImagesChecked {
		fmt.Fprintln(out, "\n> Images were not checked. Run with `--check-images` to read their platforms from the registry.")
	}
	rank := map[rules.Verdict]int{rules.NotReady: 0, rules.Unverified: 1, rules.ReadyWithCaveats: 2, rules.Ready: 3}
	var attention []rules.Assessment
	for _, a := range as {
		if a.Verdict != rules.Ready {
			attention = append(attention, a)
		}
	}
	sort.SliceStable(attention, func(i, j int) bool { return rank[attention[i].Verdict] < rank[attention[j].Verdict] })
	const limit = 50
	if len(attention) == 0 {
		return nil
	}
	fmt.Fprintln(out, "\n| Verdict | Score | Workload | Findings |\n| --- | --- | --- | --- |")
	for i, a := range attention {
		if i == limit {
			fmt.Fprintf(out, "\n%d more workloads not shown.\n", len(attention)-limit)
			break
		}
		fmt.Fprintf(out, "| %s | %d | `%s` | %s |\n", a.Verdict, a.Score, a.Workload.ID(), ruleIDs(a))
	}
	return nil
}
