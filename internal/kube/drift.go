package kube

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
)

// ChangedPaths returns sorted JSON pointers to changed or missing declared fields.
// Live-only fields are ignored except in ConfigMap/Secret payload maps, which
// have no API defaults. Kubernetes resource quantities are compared by value.
func ChangedPaths(expected, actual map[string]any) []string {
	// Normalize numeric representations from YAML and dynamic clients alike.
	normalize := func(v map[string]any) map[string]any {
		b, _ := json.Marshal(v)
		var out map[string]any
		_ = json.Unmarshal(b, &out)
		delete(out, "status")
		if out["apiVersion"] == "v1" && (out["kind"] == "Secret" || out["kind"] == "ConfigMap") {
			keys := []string{"data"}
			if out["kind"] == "ConfigMap" {
				keys = append(keys, "binaryData")
			}
			for _, key := range keys {
				if out[key] == nil {
					out[key] = map[string]any{}
				}
			}
		}
		// The API stores Secret stringData as base64 data, with stringData winning.
		if out["apiVersion"] == "v1" && out["kind"] == "Secret" {
			if stringData, ok := out["stringData"].(map[string]any); ok {
				data, _ := out["data"].(map[string]any)
				if data == nil {
					data = map[string]any{}
				}
				for key, value := range stringData {
					if text, ok := value.(string); ok {
						data[key] = base64.StdEncoding.EncodeToString([]byte(text))
					}
				}
				out["data"] = data
				delete(out, "stringData")
			}
		}
		if metadata, ok := out["metadata"].(map[string]any); ok {
			for _, key := range []string{"managedFields", "resourceVersion", "uid", "generation", "creationTimestamp", "deletionTimestamp", "deletionGracePeriodSeconds", "selfLink"} {
				delete(metadata, key)
			}
		}
		return out
	}
	var paths []string
	var compare func(any, any, string)
	compare = func(want, got any, path string) {
		switch w := want.(type) {
		case map[string]any:
			g, ok := got.(map[string]any)
			if !ok {
				paths = append(paths, path)
				return
			}
			// Payload maps have no API defaults: added keys are drift too.
			if expected["apiVersion"] == "v1" && (expected["kind"] == "ConfigMap" || expected["kind"] == "Secret") && (path == "/data" || path == "/binaryData") {
				for key := range g {
					if _, exists := w[key]; !exists {
						paths = append(paths, pointerChild(path, key))
					}
				}
			}
			for key, value := range w {
				p := pointerChild(path, key)
				live, exists := g[key]
				if !exists {
					paths = append(paths, p)
					continue
				}
				compare(value, live, p)
			}
		case []any:
			g, ok := got.([]any)
			if !ok || len(w) != len(g) {
				paths = append(paths, path)
				return
			}
			for i := range w {
				compare(w[i], g[i], path+"/"+strconv.Itoa(i))
			}
		default:
			if quantityField(expected, path) {
				wq, we := resource.ParseQuantity(fmt.Sprint(want))
				gq, ge := resource.ParseQuantity(fmt.Sprint(got))
				if we == nil && ge == nil && wq.Cmp(gq) == 0 {
					return
				}
			}
			if !reflect.DeepEqual(want, got) {
				paths = append(paths, path)
			}
		}
	}
	compare(normalize(expected), normalize(actual), "")
	sort.Strings(paths)
	return paths
}

var containerQuantity = regexp.MustCompile(`^/(containers|initContainers|ephemeralContainers)/[0-9]+/resources/(requests|limits)/[^/]+$`)
var podQuantity = regexp.MustCompile(`^/(resources/(requests|limits)|overhead)/[^/]+$`)
var limitQuantity = regexp.MustCompile(`^/spec/limits/[0-9]+/(max|min|default|defaultRequest|maxLimitRequestRatio)/[^/]+$`)

// Only recognized Kubernetes quantity fields get semantic comparison; application
// strings that happen to resemble quantities retain their exact meaning.
func quantityField(obj map[string]any, path string) bool {
	var podSpec string
	switch fmt.Sprint(obj["apiVersion"], "/", obj["kind"]) {
	case "v1/Pod":
		podSpec = "/spec"
	case "apps/v1/Deployment", "apps/v1/StatefulSet", "apps/v1/DaemonSet", "apps/v1/ReplicaSet", "v1/ReplicationController", "batch/v1/Job":
		podSpec = "/spec/template/spec"
	case "batch/v1/CronJob":
		podSpec = "/spec/jobTemplate/spec/template/spec"
	case "v1/PersistentVolumeClaim":
		return strings.HasPrefix(path, "/spec/resources/requests/") || strings.HasPrefix(path, "/spec/resources/limits/")
	case "v1/PersistentVolume":
		return strings.HasPrefix(path, "/spec/capacity/")
	case "v1/ResourceQuota":
		return strings.HasPrefix(path, "/spec/hard/")
	case "v1/LimitRange":
		return limitQuantity.MatchString(path)
	default:
		return false
	}
	suffix, ok := strings.CutPrefix(path, podSpec)
	return ok && (containerQuantity.MatchString(suffix) || podQuantity.MatchString(suffix))
}

func pointerChild(path, key string) string {
	return path + "/" + strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
}
