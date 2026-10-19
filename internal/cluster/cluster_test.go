package cluster_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"github.com/robertasolimandonofreo/graviton-readiness/internal/cluster"
	"github.com/robertasolimandonofreo/graviton-readiness/internal/cluster/clustertest"
	"github.com/robertasolimandonofreo/graviton-readiness/internal/inventory"
)

func deployment(ns, name, image string) *unstructured.Unstructured {
	return clustertest.Object("apps/v1", "Deployment", ns, name, map[string]any{"spec": map[string]any{
		"replicas": int64(2),
		"template": map[string]any{"spec": map[string]any{"containers": []any{
			map[string]any{"name": "app", "image": image},
		}}},
	}})
}

func objects() []runtime.Object {
	return []runtime.Object{
		deployment("shop", "api", "a:1"),
		deployment("other", "web", "w:1"),
		clustertest.Object("karpenter.sh/v1", "NodePool", "", "web", map[string]any{"spec": map[string]any{"template": map[string]any{
			"spec": map[string]any{"nodeClassRef": map[string]any{"name": "default"}},
		}}}),
		clustertest.Object("karpenter.k8s.aws/v1", "EC2NodeClass", "", "default", map[string]any{"spec": map[string]any{"amiFamily": "AL2023"}}),
		clustertest.Node("n1", map[string]any{"kubernetes.io/arch": "arm64", "role": "batch"}),
		clustertest.Node("n2", map[string]any{"kubernetes.io/arch": "amd64", "role": "web"}),
	}
}

func TestReadListsEverythingAndNothingElse(t *testing.T) {
	client := clustertest.New(objects()...)
	inv := inventory.New()
	res, err := cluster.Read(context.Background(), inv, cluster.Options{Client: client})
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Workloads) != 2 || len(inv.NodePools) != 1 || len(inv.NodeClasses) != 1 || len(inv.Nodes) != 2 {
		t.Fatalf("workloads %d, pools %d, classes %d, nodes %d", len(inv.Workloads), len(inv.NodePools), len(inv.NodeClasses), len(inv.Nodes))
	}
	if res.Counts["deployments"] != 2 || res.Counts["nodes"] != 2 {
		t.Errorf("counts = %v", res.Counts)
	}
	// Read-only: the only verb that was ever sent is list.
	for _, a := range client.Actions() {
		if a.GetVerb() != "list" {
			t.Errorf("verb %q on %s: the reader must only list", a.GetVerb(), a.GetResource().Resource)
		}
	}
	if len(client.Actions()) != len(cluster.Kinds) {
		t.Errorf("%d list calls for %d kinds", len(client.Actions()), len(cluster.Kinds))
	}
	if inv.Nodes[0].Arch() != "arm64" || inv.Nodes[1].Arch() != "amd64" {
		t.Errorf("node architectures: %q %q", inv.Nodes[0].Arch(), inv.Nodes[1].Arch())
	}
}

func TestNamespaceLimitsTheWorkloadsOnly(t *testing.T) {
	inv := inventory.New()
	if _, err := cluster.Read(context.Background(), inv, cluster.Options{Client: clustertest.New(objects()...), Namespace: "shop"}); err != nil {
		t.Fatal(err)
	}
	if len(inv.Workloads) != 1 || inv.Workloads[0].Namespace != "shop" {
		t.Errorf("workloads = %+v", inv.Workloads)
	}
	if len(inv.NodePools) != 1 || len(inv.NodeClasses) != 1 || len(inv.Nodes) != 2 {
		t.Errorf("pools, classes and nodes belong to the cluster and are read whole: %d %d %d", len(inv.NodePools), len(inv.NodeClasses), len(inv.Nodes))
	}
}

func fail(client interface {
	PrependReactor(verb, resource string, reaction k8stesting.ReactionFunc)
}, resource string, err error) {
	client.PrependReactor("list", resource, func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, err })
}

func TestMissingResourcesAreNotes(t *testing.T) {
	client := clustertest.New(objects()...)
	fail(client, "rollouts", apierrors.NewNotFound(schema.GroupResource{Group: "argoproj.io", Resource: "rollouts"}, ""))
	fail(client, "nodes", apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "", nil))
	var notes bytes.Buffer
	inv := inventory.New()
	if _, err := cluster.Read(context.Background(), inv, cluster.Options{Client: client, Notes: &notes}); err != nil {
		t.Fatalf("optional kinds must not fail the read: %v", err)
	}
	for _, want := range []string{"rollouts is not installed", "could not list nodes"} {
		if !strings.Contains(notes.String(), want) {
			t.Errorf("notes lack %q:\n%s", want, notes.String())
		}
	}
	if len(inv.Workloads) != 2 {
		t.Errorf("the rest must still be read: %d workloads", len(inv.Workloads))
	}
}

func TestRequiredKindFailsTheRead(t *testing.T) {
	client := clustertest.New(objects()...)
	fail(client, "deployments", apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, "", nil))
	if _, err := cluster.Read(context.Background(), inventory.New(), cluster.Options{Client: client}); err == nil || !strings.Contains(err.Error(), "listing deployments") {
		t.Errorf("a Deployments list that is forbidden must fail the read: %v", err)
	}
}

func TestPaging(t *testing.T) {
	client := clustertest.New()
	calls := 0
	client.PrependReactor("list", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		calls++
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "DeploymentList"})
		if calls == 1 {
			list.Items = []unstructured.Unstructured{*deployment("shop", "first", "a:1")}
			list.SetContinue("next")
		} else {
			list.Items = []unstructured.Unstructured{*deployment("shop", "second", "a:1")}
		}
		return true, list, nil
	})
	inv := inventory.New()
	if _, err := cluster.Read(context.Background(), inv, cluster.Options{Client: client}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(inv.Workloads) != 2 {
		t.Errorf("calls %d, workloads %d: the second page must be read", calls, len(inv.Workloads))
	}
}
