package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"helm.sh/helm/v4/pkg/release/common"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type listJSON struct {
	Name, Namespace, Status, Chart string
	Revision                       int
}

func listRows(t *testing.T, c *cluster, args ...string) []listJSON {
	t.Helper()
	out, err := run(t, c, "prod", append([]string{"list", "-o", "json"}, args...)...)
	wantProblems(t, err, false)
	var res struct{ Releases []listJSON }
	if jerr := json.Unmarshal([]byte(out), &res); jerr != nil {
		t.Fatalf("bad json: %v\n%s", jerr, out)
	}
	return res.Releases
}

func names(rows []listJSON) string {
	var n []string
	for _, r := range rows {
		n = append(n, r.Namespace+"/"+r.Name)
	}
	return strings.Join(n, ",")
}

func listCluster(t *testing.T) *cluster {
	c := newCluster(t)
	c.release("web", "prod", 1, common.StatusSuperseded, simpleManifest)
	c.release("web", "prod", 2, common.StatusDeployed, simpleManifest)
	c.release("api", "prod", 1, common.StatusFailed, simpleManifest)
	c.release("old", "prod", 1, common.StatusUninstalled, simpleManifest)
	c.release("stuck", "prod", 1, common.StatusPendingUpgrade, simpleManifest)
	c.release("other", "dev", 1, common.StatusDeployed, simpleManifest)
	return c
}

func TestListShowsLatestRevisionOfDeployedAndFailedSortedByName(t *testing.T) {
	rows := listRows(t, listCluster(t))
	if got := names(rows); got != "prod/api,prod/web" {
		t.Fatalf("got %s", got)
	}
	if rows[1].Revision != 2 || rows[1].Status != "deployed" || rows[1].Chart != "web-1.2.0" || rows[0].Status != "failed" {
		t.Errorf("rows = %+v", rows)
	}
}

func TestListAllShowsEveryStatus(t *testing.T) {
	if got := names(listRows(t, listCluster(t), "--all")); got != "prod/api,prod/old,prod/stuck,prod/web" {
		t.Fatalf("got %s", got)
	}
}

func TestListAllNamespacesSortsByNameThenNamespace(t *testing.T) {
	c := listCluster(t)
	c.release("api", "dev", 1, common.StatusDeployed, simpleManifest)
	if got := names(listRows(t, c, "-A")); got != "dev/api,prod/api,dev/other,prod/web" {
		t.Fatalf("got %s", got)
	}
}

func TestListTable(t *testing.T) {
	out, err := run(t, listCluster(t), "prod", "list")
	wantProblems(t, err, false)
	golden(t, out)
}

func TestListEmptyIsEmptyArray(t *testing.T) {
	out, err := run(t, newCluster(t), "prod", "list", "-o", "json")
	wantProblems(t, err, false)
	if !strings.Contains(out, `"releases": []`) {
		t.Fatalf("releases must serialize as [] not null:\n%s", out)
	}
}

func TestListWarnsAboutUndecodableRecords(t *testing.T) {
	c := listCluster(t)
	_, err := c.kube.CoreV1().Secrets("prod").Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "sh.helm.release.v1.bad.v1", Namespace: "prod",
			Labels: map[string]string{"owner": "helm", "name": "bad", "version": "1"},
		},
		Data: map[string][]byte{"release": []byte("garbage")},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := run(t, c, "prod", "list")
	wantProblems(t, err, false)
	if !strings.Contains(out, "warning: 1 release(s) could not be decoded") || !strings.Contains(out, "web") {
		t.Fatalf("want the warning and the readable releases:\n%s", out)
	}
}
