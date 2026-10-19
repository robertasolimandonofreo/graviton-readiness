// Package clustertest builds fake clusters for the tests of the packages that read one.
package clustertest

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"

	"github.com/robertasolimandonofreo/graviton-readiness/internal/cluster"
)

var listKinds = map[string]string{
	"deployments": "DeploymentList", "statefulsets": "StatefulSetList", "daemonsets": "DaemonSetList",
	"cronjobs": "CronJobList", "rollouts": "RolloutList", "nodepools": "NodePoolList",
	"ec2nodeclasses": "EC2NodeClassList", "nodes": "NodeList",
}

// New returns a dynamic client that knows every resource that the reader lists, holding objs.
func New(objs ...runtime.Object) *fake.FakeDynamicClient {
	kinds := map[schema.GroupVersionResource]string{}
	for _, k := range cluster.Kinds {
		kinds[k.GVR()] = listKinds[k.GVR().Resource]
	}
	return fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), kinds, objs...)
}

// Object builds an unstructured object from its parts.
func Object(apiVersion, kind, namespace, name string, fields map[string]any) *unstructured.Unstructured {
	meta := map[string]any{"name": name}
	if namespace != "" {
		meta["namespace"] = namespace
	}
	obj := map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": meta}
	for k, v := range fields {
		obj[k] = v
	}
	return &unstructured.Unstructured{Object: obj}
}

// Node builds a node with the given labels.
func Node(name string, labels map[string]any) *unstructured.Unstructured {
	n := Object("v1", "Node", "", name, nil)
	n.Object["metadata"].(map[string]any)["labels"] = labels
	return n
}
