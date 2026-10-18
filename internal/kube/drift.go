package kube

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// ChangedPaths returns sorted JSON pointers to changed or missing declared fields.
// Live-only fields are ignored because they may be defaults or controller additions.
func ChangedPaths(expected, actual map[string]any) []string {
	// Normalize numeric representations from YAML and dynamic clients alike.
	normalize := func(v map[string]any) map[string]any {
		b, _ := json.Marshal(v)
		var out map[string]any
		_ = json.Unmarshal(b, &out)
		delete(out, "status")
		// The API stores Secret stringData as base64 data, with stringData winning.
		if out["apiVersion"] == "v1" && out["kind"] == "Secret" {
			if strings, ok := out["stringData"].(map[string]any); ok {
				data, _ := out["data"].(map[string]any)
				if data == nil {
					data = map[string]any{}
				}
				for key, value := range strings {
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
			for key, value := range w {
				p := path + "/" + strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
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
			if !reflect.DeepEqual(want, got) {
				paths = append(paths, path)
			}
		}
	}
	compare(normalize(expected), normalize(actual), "")
	sort.Strings(paths)
	return paths
}
