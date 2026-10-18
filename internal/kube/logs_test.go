package kube

import (
	"context"
	"testing"

	kubefake "k8s.io/client-go/kubernetes/fake"
)

func TestPodLogReader(t *testing.T) {
	read := PodLogReader(kubefake.NewClientset())
	got, err := read(context.Background(), "prod", "web-1", "main", 5, true)
	if err != nil {
		t.Fatal(err)
	}
	if got != "fake logs" { // the client-go fake returns a fixed body
		t.Fatalf("log = %q", got)
	}
}
