package kube

import (
	"context"
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestHealthListsReturnAPIError(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name     string
		resource string
		list     func(c *Cluster) error
	}{
		{"pods", "pods", func(c *Cluster) error {
			_, err := c.ListPods(context.Background(), "ns", "app=web")
			return err
		}},
		{"events", "events", func(c *Cluster) error {
			_, err := c.ListWarningEvents(context.Background(), "ns")
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dc := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
				podsGVR: "PodList", eventsGVR: "EventList",
			})
			dc.PrependReactor("list", tt.resource, func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, nil, boom
			})
			if err := tt.list(&Cluster{Dynamic: dc}); !errors.Is(err, boom) {
				t.Fatalf("err = %v, want %v", err, boom)
			}
		})
	}
}
