package report_test

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/robertasolimandonofreo/graviton-readiness/internal/image"
	"github.com/robertasolimandonofreo/graviton-readiness/internal/inventory"
	"github.com/robertasolimandonofreo/graviton-readiness/internal/report"
	"github.com/robertasolimandonofreo/graviton-readiness/internal/rules"
)

type sarifDoc struct {
	Version string `json:"version"`
	Runs    []struct {
		Tool struct {
			Driver struct {
				Name  string `json:"name"`
				Rules []struct {
					ID string `json:"id"`
				} `json:"rules"`
			} `json:"driver"`
		} `json:"tool"`
		Results []struct {
			RuleID    string `json:"ruleId"`
			RuleIndex int    `json:"ruleIndex"`
			Level     string `json:"level"`
			Locations []struct {
				PhysicalLocation *struct {
					ArtifactLocation struct {
						URI       string `json:"uri"`
						URIBaseID string `json:"uriBaseId"`
					} `json:"artifactLocation"`
					Region struct {
						StartLine int `json:"startLine"`
					} `json:"region"`
				} `json:"physicalLocation"`
				LogicalLocations []struct {
					FullyQualifiedName string `json:"fullyQualifiedName"`
				} `json:"logicalLocations"`
			} `json:"locations"`
			PartialFingerprints map[string]string `json:"partialFingerprints"`
		} `json:"results"`
	} `json:"runs"`
}

func scan(t *testing.T, path string) []rules.Assessment {
	t.Helper()
	inv, err := inventory.Paths([]string{path}, strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	var out []rules.Assessment
	for _, w := range inv.Workloads {
		out = append(out, rules.Evaluate(w, map[string]image.Result{}, rules.Options{}))
	}
	return out
}

func sarif(t *testing.T, as []rules.Assessment) sarifDoc {
	t.Helper()
	var buf bytes.Buffer
	if err := report.SARIF(&buf, as, report.Options{Version: "test"}); err != nil {
		t.Fatal(err)
	}
	var doc sarifDoc
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	return doc
}

func TestSARIFPointsAtTheLineOfTheField(t *testing.T) {
	const file = "../../testdata/sample.yaml"
	raw, _ := os.ReadFile(file)
	lines := strings.Split(string(raw), "\n")
	doc := sarif(t, scan(t, file))

	if doc.Version != "2.1.0" || len(doc.Runs) != 1 || doc.Runs[0].Tool.Driver.Name != "graviton-readiness" {
		t.Fatalf("header = %+v", doc)
	}
	if got, want := len(doc.Runs[0].Tool.Driver.Rules), len(rules.Catalog); got != want {
		t.Errorf("%d rules, want %d", got, want)
	}
	// The line each rule points at must hold the text of the field behind it.
	holds := map[string]string{"GR001": "kubernetes.io/arch", "GR003": "image:", "GR006": "amd64", "GR008": "kind:"}
	seen := map[string]bool{}
	for _, r := range doc.Runs[0].Results {
		want, ok := holds[r.RuleID]
		if !ok {
			continue
		}
		seen[r.RuleID] = true
		pl := r.Locations[0].PhysicalLocation
		if pl == nil || pl.ArtifactLocation.URI == "" {
			t.Errorf("%s has no physical location", r.RuleID)
			continue
		}
		if line := pl.Region.StartLine; line < 1 || line > len(lines) || !strings.Contains(strings.ToLower(lines[line-1]), want) {
			t.Errorf("%s points at line %d: %q, which lacks %q", r.RuleID, line, lines[pl.Region.StartLine-1], want)
		}
		if r.Locations[0].LogicalLocations[0].FullyQualifiedName == "" {
			t.Errorf("%s has no logical location", r.RuleID)
		}
	}
	for id := range holds {
		if !seen[id] {
			t.Errorf("no result for %s in the sample", id)
		}
	}
	// Levels follow the severities, and the rule index points at the right rule.
	level := map[string]string{"GR001": "error", "GR006": "warning", "GR008": "note"}
	for _, r := range doc.Runs[0].Results {
		if want, ok := level[r.RuleID]; ok && r.Level != want {
			t.Errorf("%s level = %s, want %s", r.RuleID, r.Level, want)
		}
		if doc.Runs[0].Tool.Driver.Rules[r.RuleIndex].ID != r.RuleID {
			t.Errorf("rule index of %s points at %s", r.RuleID, doc.Runs[0].Tool.Driver.Rules[r.RuleIndex].ID)
		}
	}
}

func TestFingerprintSurvivesAnEditThatMovesTheManifest(t *testing.T) {
	const manifest = "apiVersion: v1\nkind: Pod\nmetadata: {name: p}\nspec:\n  nodeSelector: {kubernetes.io/arch: amd64}\n  containers: [{name: c, image: 'c:1'}]\n"
	dir := t.TempDir()
	write := func(name, text string) string {
		if err := os.WriteFile(dir+"/"+name, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir + "/" + name
	}
	// the same file name at two moments, one with lines added above
	a := write("m.yaml", manifest)
	first := sarif(t, scan(t, a)).Runs[0].Results
	b := write("m.yaml", "# a new comment\n# another one\n"+manifest)
	second := sarif(t, scan(t, b)).Runs[0].Results
	if len(first) == 0 || len(first) != len(second) {
		t.Fatalf("%d and %d results", len(first), len(second))
	}
	for i := range first {
		if first[i].PartialFingerprints["gravitonReadiness/v1"] != second[i].PartialFingerprints["gravitonReadiness/v1"] {
			t.Errorf("the fingerprint of %s changed when the manifest moved", first[i].RuleID)
		}
		if l1, l2 := first[i].Locations[0].PhysicalLocation.Region.StartLine, second[i].Locations[0].PhysicalLocation.Region.StartLine; l2 != l1+2 {
			t.Errorf("%s moved from line %d to %d, want %d", first[i].RuleID, l1, l2, l1+2)
		}
	}
	// Two different rules, or two images in one workload, never share a fingerprint.
	seen := map[string]bool{}
	for _, r := range first {
		fp := r.PartialFingerprints["gravitonReadiness/v1"]
		if seen[fp] {
			t.Errorf("duplicate fingerprint %s", fp)
		}
		seen[fp] = true
	}
}

func TestClusterFindingsHaveALogicalLocationOnly(t *testing.T) {
	inv := inventory.New()
	raw := `{"apiVersion":"apps/v1","kind":"DaemonSet","metadata":{"name":"agent","namespace":"kube-system"},"spec":{"template":{"spec":{"containers":[{"name":"a","image":"a:1"}]}}}}`
	if err := inv.AddJSON("cluster", []byte(raw)); err != nil {
		t.Fatal(err)
	}
	doc := sarif(t, []rules.Assessment{rules.Evaluate(inv.Workloads[0], nil, rules.Options{})})
	if len(doc.Runs[0].Results) == 0 {
		t.Fatal("no result")
	}
	loc := doc.Runs[0].Results[0].Locations[0]
	if loc.PhysicalLocation != nil || loc.LogicalLocations[0].FullyQualifiedName != "kube-system/daemonset/agent" {
		t.Errorf("a workload from a cluster has no file: %+v", loc)
	}
}
