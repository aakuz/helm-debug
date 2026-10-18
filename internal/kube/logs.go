package kube

import (
	"context"
	"io"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

// maxLogBytes bounds how much log text is read for one container.
const maxLogBytes = 1 << 20

// LogReader returns the last tail lines of a container's log (of its previous run when previous is set).
type LogReader func(ctx context.Context, namespace, pod, container string, tail int64, previous bool) (string, error)

// PodLogReader reads logs through the pods/log subresource (a read-only GET).
func PodLogReader(kc kubernetes.Interface) LogReader {
	return func(ctx context.Context, namespace, pod, container string, tail int64, previous bool) (string, error) {
		limit := int64(maxLogBytes)
		rc, err := kc.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{
			Container:  container,
			TailLines:  &tail,
			Previous:   previous,
			LimitBytes: &limit,
		}).Stream(ctx)
		if err != nil {
			return "", err
		}
		defer func() { _ = rc.Close() }()
		b, err := io.ReadAll(rc)
		return string(b), err
	}
}
