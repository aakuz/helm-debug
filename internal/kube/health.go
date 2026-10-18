package kube

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	podsGVR   = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	eventsGVR = schema.GroupVersionResource{Version: "v1", Resource: "events"}
)

// ListPods lists the pods in a namespace that match a label selector.
func (c *Cluster) ListPods(ctx context.Context, namespace, selector string) ([]unstructured.Unstructured, error) {
	l, err := c.Dynamic.Resource(podsGVR).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	return l.Items, nil
}

// ListWarningEvents lists the Warning events in a namespace.
func (c *Cluster) ListWarningEvents(ctx context.Context, namespace string) ([]unstructured.Unstructured, error) {
	l, err := c.Dynamic.Resource(eventsGVR).Namespace(namespace).List(ctx, metav1.ListOptions{FieldSelector: "type=Warning"})
	if err != nil {
		return nil, err
	}
	return l.Items, nil
}
