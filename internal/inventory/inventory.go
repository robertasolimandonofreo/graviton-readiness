// Package inventory reads Kubernetes manifests (files, directories or stdin) into a list of
// workloads, each one reduced to what the architecture rules need.
package inventory

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// Workload is a pod-creating object, with its pod template.
type Workload struct {
	Kind      string
	Namespace string
	Name      string
	Replicas  int32
	// CPUMillis is the CPU requested by one replica, summed over its containers.
	CPUMillis int64
	// LongRunning is true for objects that keep running (Deployment, StatefulSet, Pod). Only
	// those count in the CPU figures: Jobs and DaemonSets do not have a replica count to weigh by.
	LongRunning bool
	Pod         corev1.PodSpec
	File        string
	// Line is the line of the file where the document of this workload starts, and Doc is its
	// text: together they let a finding point at the line of the field that caused it. Both are
	// empty for a workload read from a cluster.
	Line int
	Doc  string
}

// LineOf returns the line of the file of the first line of the document that contains substr
// (case-insensitive), or the line of its kind: when there is none. 0 means there is no file.
func (w Workload) LineOf(substr string) int {
	if w.Doc == "" || w.Line == 0 {
		return 0
	}
	lines := strings.Split(w.Doc, "\n")
	if needle := strings.ToLower(substr); needle != "" {
		for i, l := range lines {
			if strings.Contains(strings.ToLower(l), needle) {
				return w.Line + i
			}
		}
	}
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "kind:") {
			return w.Line + i
		}
	}
	return w.Line
}

// ID is the display name, namespace/kind/name in lower case, like kubectl output.
func (w Workload) ID() string {
	return fmt.Sprintf("%s/%s/%s", w.Namespace, strings.ToLower(w.Kind), w.Name)
}

// RequestedCPU is the CPU the workload asks for in total, in millicores.
func (w Workload) RequestedCPU() int64 { return w.CPUMillis * int64(w.Replicas) }

// Containers returns every container of the pod: init containers first, then the app ones.
func (w Workload) Containers() []corev1.Container {
	out := append([]corev1.Container{}, w.Pod.InitContainers...)
	return append(out, w.Pod.Containers...)
}

// Inventory is what a scan found.
type Inventory struct {
	Workloads []Workload
	// Skipped counts objects that were read but are not analyzed, by kind (for example
	// ReplicaSets, which belong to a Deployment).
	Skipped map[string]int
	// Unreadable lists the files found while walking a directory that could not be parsed as
	// Kubernetes manifests (a Helm template, for example). They are skipped, not fatal. A file
	// given on the command line that cannot be parsed is an error instead.
	Unreadable []Unreadable
	// NodePools are the Karpenter NodePools found next to the workloads. A workload that selects
	// a pool by label has the architecture of that pool, which the pod itself does not say.
	NodePools []NodePool
}

// NodePool is the part of a Karpenter NodePool that decides which nodes it can provision.
type NodePool struct {
	Name         string
	Labels       map[string]string
	Requirements []Requirement
	File         string
	// LabelOnly marks a pool declared by label (--node-pool role=web:amd64): it has no name that
	// karpenter.sh/nodepool could select.
	LabelOnly bool
}

// Requirement is one entry of spec.template.spec.requirements.
type Requirement struct {
	Key      string
	Operator string
	Values   []string
}

// Unreadable is a file that was skipped, and why.
type Unreadable struct {
	Path   string
	Reason string
}

// Paths loads every .yaml, .yml and .json file under the given paths. "-" reads stdin.
func Paths(paths []string, stdin io.Reader) (*Inventory, error) {
	inv := &Inventory{Skipped: map[string]int{}}
	for _, p := range paths {
		if p == "-" {
			data, err := io.ReadAll(stdin)
			if err != nil {
				return nil, err
			}
			if err := inv.Add("", data); err != nil {
				return nil, fmt.Errorf("stdin: %w", err)
			}
			continue
		}
		err := filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			switch strings.ToLower(filepath.Ext(path)) {
			case ".yaml", ".yml", ".json":
			default:
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			// A file named on the command line must parse. One found in a directory may be
			// a template or a values file, so it is skipped, and nothing it held is kept.
			before, pools, skipped := len(inv.Workloads), len(inv.NodePools), maps.Clone(inv.Skipped)
			if err := inv.Add(path, data); err != nil {
				if path == p {
					return fmt.Errorf("%s: %w", path, err)
				}
				inv.Workloads, inv.NodePools, inv.Skipped = inv.Workloads[:before], inv.NodePools[:pools], skipped
				inv.Unreadable = append(inv.Unreadable, Unreadable{Path: path, Reason: firstLine(err)})
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.SliceStable(inv.Workloads, func(i, j int) bool { return inv.Workloads[i].ID() < inv.Workloads[j].ID() })
	return inv, nil
}

// New returns an empty inventory.
func New() *Inventory { return &Inventory{Skipped: map[string]int{}} }

// AddJSON adds one object that is already JSON (an object listed from a cluster). file is only
// used to say where it came from.
func (inv *Inventory) AddJSON(file string, raw []byte) error {
	return inv.addObject(source{file: file}, raw)
}

// Sort orders the workloads by ID, so the output does not depend on where each one came from.
func (inv *Inventory) Sort() {
	sort.SliceStable(inv.Workloads, func(i, j int) bool { return inv.Workloads[i].ID() < inv.Workloads[j].ID() })
}

// docSeparator is a YAML document separator on a line of its own.
var docSeparator = regexp.MustCompile(`(?m)^---[ \t]*$`)

// source says where an object came from.
type source struct {
	file string
	line int    // the line of the file where its document starts, 0 when there is no file
	doc  string // the text of its document
}

// Add parses a multi-document YAML (or JSON) stream. file is only used to say where a workload
// came from, and each document remembers the line where it starts.
func (inv *Inventory) Add(file string, data []byte) error {
	text := string(data)
	seps := docSeparator.FindAllStringIndex(text, -1)
	start := 0
	for i := 0; i <= len(seps); i++ {
		end := len(text)
		if i < len(seps) {
			end = seps[i][0]
		}
		doc := text[start:end]
		// The first line of a document that follows a separator is the separator's own line.
		line := 1 + strings.Count(text[:start], "\n")
		if i < len(seps) {
			start = seps[i][1]
		}
		if strings.TrimSpace(doc) == "" {
			continue
		}
		raw, err := yaml.YAMLToJSON([]byte(doc))
		if err != nil {
			return err
		}
		if len(bytes.TrimSpace(raw)) == 0 || string(raw) == "null" {
			continue
		}
		if err := inv.addObject(source{file: file, line: line, doc: doc}, raw); err != nil {
			return err
		}
	}
	return nil
}

func (inv *Inventory) addObject(src source, raw json.RawMessage) error {
	var meta struct {
		metav1.TypeMeta `json:",inline"`
		Items           []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return err
	}
	decode := func(v any) error { return json.Unmarshal(raw, v) }
	add := func(kind string, om metav1.ObjectMeta, replicas *int32, long bool, pod corev1.PodSpec) {
		w := Workload{
			Kind:        kind,
			Namespace:   om.Namespace,
			Name:        om.Name,
			Replicas:    1,
			LongRunning: long,
			Pod:         pod,
			File:        src.file,
			Line:        src.line,
			Doc:         src.doc,
		}
		if w.Namespace == "" {
			w.Namespace = "default"
		}
		if w.Name == "" && om.GenerateName != "" {
			// Created with generateName, so the real name does not exist until it runs.
			w.Name = om.GenerateName + "*"
		}
		if replicas != nil {
			w.Replicas = *replicas
		}
		w.CPUMillis = cpuRequest(pod)
		inv.Workloads = append(inv.Workloads, w)
	}

	switch meta.Kind {
	case "":
		// Not a Kubernetes object (for example a values file): ignore it.
	case "List":
		for _, item := range meta.Items {
			if err := inv.addObject(src, item); err != nil {
				return err
			}
		}
	case "Deployment":
		var o appsv1.Deployment
		if err := decode(&o); err != nil {
			return err
		}
		add("Deployment", o.ObjectMeta, o.Spec.Replicas, true, o.Spec.Template.Spec)
	case "StatefulSet":
		var o appsv1.StatefulSet
		if err := decode(&o); err != nil {
			return err
		}
		add("StatefulSet", o.ObjectMeta, o.Spec.Replicas, true, o.Spec.Template.Spec)
	case "DaemonSet":
		var o appsv1.DaemonSet
		if err := decode(&o); err != nil {
			return err
		}
		add("DaemonSet", o.ObjectMeta, nil, false, o.Spec.Template.Spec)
	case "Job":
		var o batchv1.Job
		if err := decode(&o); err != nil {
			return err
		}
		add("Job", o.ObjectMeta, nil, false, o.Spec.Template.Spec)
	case "CronJob":
		var o batchv1.CronJob
		if err := decode(&o); err != nil {
			return err
		}
		add("CronJob", o.ObjectMeta, nil, false, o.Spec.JobTemplate.Spec.Template.Spec)
	case "Pod":
		var o corev1.Pod
		if err := decode(&o); err != nil {
			return err
		}
		add("Pod", o.ObjectMeta, nil, true, o.Spec)
	case "NodePool":
		// Only Karpenter's: other projects have a kind with the same name.
		if !strings.HasPrefix(meta.APIVersion, "karpenter.sh/") {
			return nil
		}
		var o struct {
			Metadata metav1.ObjectMeta `json:"metadata"`
			Spec     struct {
				Template struct {
					Metadata struct {
						Labels map[string]string `json:"labels"`
					} `json:"metadata"`
					Spec struct {
						Requirements []struct {
							Key      string   `json:"key"`
							Operator string   `json:"operator"`
							Values   []string `json:"values"`
						} `json:"requirements"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
		}
		if err := decode(&o); err != nil {
			return err
		}
		pool := NodePool{Name: o.Metadata.Name, Labels: o.Spec.Template.Metadata.Labels, File: src.file}
		for _, r := range o.Spec.Template.Spec.Requirements {
			pool.Requirements = append(pool.Requirements, Requirement{Key: r.Key, Operator: r.Operator, Values: r.Values})
		}
		inv.NodePools = append(inv.NodePools, pool)
	case "ReplicaSet", "ReplicationController":
		// Owned by a Deployment in practice: counted so the report can say what was left out.
		inv.Skipped[meta.Kind]++
	}
	return nil
}

func firstLine(err error) string {
	msg, _, _ := strings.Cut(err.Error(), "\n")
	return msg
}

// cpuRequest sums the CPU requests of the app containers, in millicores. Init containers run
// before the app and do not add to the steady-state request.
func cpuRequest(pod corev1.PodSpec) int64 {
	var total int64
	for _, c := range pod.Containers {
		if q, ok := c.Resources.Requests[corev1.ResourceCPU]; ok {
			total += q.MilliValue()
		}
	}
	return total
}
