package report

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"

	"github.com/robertasolimandonofreo/graviton-readiness/internal/rules"
)

// The types below are the part of SARIF 2.1.0 that GitHub Code Scanning reads.

type sarifLog struct {
	Schema  string     `json:"$schema"`
	Version string     `json:"version"`
	Runs    []sarifRun `json:"runs"`
}

type sarifRun struct {
	Tool    sarifTool     `json:"tool"`
	Results []sarifResult `json:"results"`
}

type sarifTool struct {
	Driver sarifDriver `json:"driver"`
}

type sarifDriver struct {
	Name           string      `json:"name"`
	Version        string      `json:"version,omitempty"`
	InformationURI string      `json:"informationUri"`
	Rules          []sarifRule `json:"rules"`
}

type sarifText struct {
	Text string `json:"text"`
}

type sarifRule struct {
	ID                   string          `json:"id"`
	HelpURI              string          `json:"helpUri,omitempty"`
	ShortDescription     sarifText       `json:"shortDescription"`
	FullDescription      sarifText       `json:"fullDescription"`
	Help                 sarifText       `json:"help"`
	DefaultConfiguration sarifRuleConfig `json:"defaultConfiguration"`
}

type sarifRuleConfig struct {
	Level string `json:"level"`
}

type sarifResult struct {
	RuleID              string            `json:"ruleId"`
	RuleIndex           int               `json:"ruleIndex"`
	Level               string            `json:"level"`
	Message             sarifText         `json:"message"`
	Locations           []sarifLocation   `json:"locations"`
	PartialFingerprints map[string]string `json:"partialFingerprints"`
}

type sarifLocation struct {
	PhysicalLocation *sarifPhysical `json:"physicalLocation,omitempty"`
	LogicalLocations []sarifLogical `json:"logicalLocations,omitempty"`
}

type sarifPhysical struct {
	ArtifactLocation sarifArtifact `json:"artifactLocation"`
	Region           sarifRegion   `json:"region"`
}

type sarifArtifact struct {
	URI       string `json:"uri"`
	URIBaseID string `json:"uriBaseId,omitempty"`
}

type sarifRegion struct {
	StartLine int `json:"startLine"`
}

type sarifLogical struct {
	FullyQualifiedName string `json:"fullyQualifiedName"`
	Kind               string `json:"kind"`
}

const repoURL = "https://github.com/robertasolimandonofreo/graviton-readiness"

// artifact is the location of a file: a path inside the directory of the scan is relative to the
// repository root, and a path outside it (the scan was run on another directory) is a file URI.
func artifact(file string) sarifArtifact {
	if strings.HasPrefix(file, "/") {
		return sarifArtifact{URI: "file://" + file}
	}
	if len(file) > 1 && file[1] == ':' { // a Windows drive, such as C:/repo/k8s
		return sarifArtifact{URI: "file:///" + file}
	}
	return sarifArtifact{URI: strings.TrimPrefix(file, "./"), URIBaseID: "%SRCROOT%"}
}

// sarifLevel maps the severities to SARIF levels: blockers are errors, caveats warnings and infos
// notes.
func sarifLevel(s rules.Severity) string {
	switch s {
	case rules.Blocker:
		return "error"
	case rules.Caveat:
		return "warning"
	}
	return "note"
}

// SARIF writes the findings as a SARIF 2.1.0 log, for GitHub Code Scanning and other viewers. A
// finding from a file points at the line of the field that caused it (the image, the selector);
// one from a cluster has no file and carries the workload as a logical location.
func SARIF(out io.Writer, as []rules.Assessment, o Options) error {
	driver := sarifDriver{Name: "graviton-readiness", Version: o.Version, InformationURI: repoURL, Rules: []sarifRule{}}
	index := map[string]int{}
	for _, r := range rules.Catalog {
		index[r.ID] = len(driver.Rules)
		driver.Rules = append(driver.Rules, sarifRule{
			ID: r.ID, HelpURI: repoURL + "#rules",
			ShortDescription:     sarifText{r.Title},
			FullDescription:      sarifText{r.Why},
			Help:                 sarifText{r.Fix},
			DefaultConfiguration: sarifRuleConfig{Level: sarifLevel(r.Severity)},
		})
	}

	results := []sarifResult{}
	for _, a := range as {
		w := a.Workload
		for _, f := range a.Findings {
			loc := sarifLocation{LogicalLocations: []sarifLogical{{FullyQualifiedName: w.ID(), Kind: strings.ToLower(w.Kind)}}}
			if line := w.LineOf(f.Hint); w.File != "" && w.File != "cluster" && line > 0 {
				loc.PhysicalLocation = &sarifPhysical{ArtifactLocation: artifact(w.File), Region: sarifRegion{StartLine: line}}
			}
			// The fingerprint ignores the line, so an alert survives edits that move the manifest.
			sum := sha256.Sum256([]byte(w.File + "|" + w.ID() + "|" + f.Rule + "|" + f.Hint))
			results = append(results, sarifResult{
				RuleID: f.Rule, RuleIndex: index[f.Rule], Level: sarifLevel(f.Severity),
				Message:             sarifText{f.Message},
				Locations:           []sarifLocation{loc},
				PartialFingerprints: map[string]string{"gravitonReadiness/v1": hex.EncodeToString(sum[:16])},
			})
		}
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(sarifLog{Schema: "https://json.schemastore.org/sarif-2.1.0.json", Version: "2.1.0",
		Runs: []sarifRun{{Tool: sarifTool{Driver: driver}, Results: results}}})
}
