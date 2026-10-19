// Package cluster reads the workloads, the Karpenter NodePools and EC2NodeClasses and the nodes
// from a live cluster. It only lists: no other verb is ever sent, and the requests are paged and
// rate limited, because reading a production cluster is not free.
package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/robertasolimandonofreo/graviton-readiness/internal/inventory"
)

// pageSize is how many objects one list call returns.
const pageSize = 500

// kind is a resource to list. required kinds fail the read when they cannot be listed; the others
// (custom resources that may not be installed, or that RBAC does not allow) only produce a note.
type kind struct {
	gvr      schema.GroupVersionResource
	required bool
	// clusterScoped resources are never limited to a namespace.
	clusterScoped bool
}

// GVR is the resource that is listed.
func (k kind) GVR() schema.GroupVersionResource { return k.gvr }

// Kinds are the resources that are read, in this order, one at a time. Pods and Jobs are left out:
// almost every one belongs to a controller that is read, and there are many of them.
var Kinds = []kind{
	{gvr: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, required: true},
	{gvr: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}},
	{gvr: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "daemonsets"}},
	{gvr: schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "cronjobs"}},
	{gvr: schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "rollouts"}},
	{gvr: schema.GroupVersionResource{Group: "karpenter.sh", Version: "v1", Resource: "nodepools"}, clusterScoped: true},
	{gvr: schema.GroupVersionResource{Group: "karpenter.k8s.aws", Version: "v1", Resource: "ec2nodeclasses"}, clusterScoped: true},
	{gvr: schema.GroupVersionResource{Group: "", Version: "v1", Resource: "nodes"}, clusterScoped: true},
}

// Options says which cluster to read.
type Options struct {
	Kubeconfig string
	Context    string
	// Namespace limits the workloads to one namespace. Empty reads every namespace. NodePools,
	// EC2NodeClasses and nodes belong to the cluster and are always read whole.
	Namespace string
	// Client replaces the one built from the kubeconfig. Tests use it.
	Client dynamic.Interface
	// Notes receives what was skipped or could not be read.
	Notes io.Writer
}

// Result is what was read.
type Result struct {
	// Context is the kubeconfig context that was used, to be printed.
	Context string
	Counts  map[string]int
}

// Read lists the cluster into inv.
func Read(ctx context.Context, inv *inventory.Inventory, o Options) (*Result, error) {
	res := &Result{Counts: map[string]int{}}
	client := o.Client
	if client == nil {
		c, name, err := newClient(o)
		if err != nil {
			return nil, err
		}
		client, res.Context = c, name
	}
	note := func(format string, args ...any) {
		if o.Notes != nil {
			fmt.Fprintf(o.Notes, format+"\n", args...)
		}
	}
	for _, k := range Kinds {
		namespace := o.Namespace
		if k.clusterScoped {
			namespace = ""
		}
		n, err := list(ctx, client, k.gvr, namespace, func(item *unstructured.Unstructured) error {
			raw, err := json.Marshal(item.Object)
			if err != nil {
				return err
			}
			return inv.AddJSON("cluster", raw)
		})
		switch {
		case err == nil:
			res.Counts[k.gvr.Resource] = n
		case isAbsent(err):
			note("note: %s is not installed in this cluster, skipped", k.gvr.Resource)
		case k.required:
			return nil, fmt.Errorf("listing %s: %w", k.gvr.Resource, err)
		default:
			note("note: could not list %s (%s), skipped", k.gvr.Resource, short(err))
		}
	}
	return res, nil
}

func newClient(o Options) (dynamic.Interface, string, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if o.Kubeconfig != "" {
		rules.ExplicitPath = o.Kubeconfig
	}
	cfg := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: o.Context})
	raw, err := cfg.RawConfig()
	if err != nil {
		return nil, "", fmt.Errorf("reading the kubeconfig: %w", err)
	}
	name := raw.CurrentContext
	if o.Context != "" {
		name = o.Context
	}
	rest, err := cfg.ClientConfig()
	if err != nil {
		return nil, "", fmt.Errorf("the kubeconfig has no usable cluster: %w", err)
	}
	// Gentle on the API server: a few requests a second, each one bounded.
	rest.QPS, rest.Burst, rest.Timeout = 5, 10, 60*time.Second
	client, err := dynamic.NewForConfig(rest)
	if err != nil {
		return nil, "", err
	}
	return client, name, nil
}

// list pages through one resource and calls fn for every object. It returns how many there were.
func list(ctx context.Context, client dynamic.Interface, gvr schema.GroupVersionResource, namespace string, fn func(*unstructured.Unstructured) error) (int, error) {
	var res dynamic.ResourceInterface = client.Resource(gvr)
	if namespace != "" {
		res = client.Resource(gvr).Namespace(namespace)
	}
	total, cont := 0, ""
	for {
		page, err := res.List(ctx, metav1.ListOptions{Limit: pageSize, Continue: cont})
		if err != nil {
			return total, err
		}
		for i := range page.Items {
			if err := fn(&page.Items[i]); err != nil {
				return total, err
			}
			total++
		}
		if cont = page.GetContinue(); cont == "" {
			return total, nil
		}
	}
}

// isAbsent is true when the resource does not exist in the cluster (a custom resource that is not
// installed), which is not an error.
func isAbsent(err error) bool {
	return apierrors.IsNotFound(err) || meta.IsNoMatchError(err) ||
		strings.Contains(err.Error(), "the server could not find the requested resource")
}

func short(err error) string {
	msg, _, _ := strings.Cut(err.Error(), "\n")
	if len(msg) > 140 {
		msg = msg[:140] + "..."
	}
	return msg
}
