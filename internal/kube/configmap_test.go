package kube

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/glueops/argocd-app-chart-preflight/internal/schema"
)

func report() *schema.Report {
	return &schema.Report{SchemaVersion: 1, Run: schema.Run{ID: "20261104100000_0123abcd", CaptainDomain: "c", TargetRevision: schema.Str("0.18.0"), FinishedAt: "2026-11-04T10:00:01.000Z", Status: schema.GroupOK}, AppSets: []schema.AppSet{}}
}

func TestStatusConfigMapKeys(t *testing.T) {
	cm := StatusConfigMap("ns", "app-chart-preflight-status", report(), []byte("schema_version: 1\n"), true)
	want := map[string]string{"status": "ok", "target_revision": "0.18.0", "finished_at": "2026-11-04T10:00:01.000Z", "schema_version": "1", "report_truncated": "true", "report.yaml": "schema_version: 1\n"}
	for k, v := range want {
		if cm.Data[k] != v {
			t.Errorf("data[%s] = %q, want %q", k, cm.Data[k], v)
		}
	}
	if len(cm.Data) != len(want) || cm.Labels["app.kubernetes.io/managed-by"] != ManagedByLabel {
		t.Errorf("configmap = %+v", cm)
	}
}

func TestUpsertCreatesThenUpdates(t *testing.T) {
	client := fake.NewClientset()
	cm := StatusConfigMap("ns", "cm", report(), []byte("a"), false)
	if got, err := UpsertConfigMap(context.Background(), client, cm); err != nil || got != "created" {
		t.Fatalf("first upsert: %q %v", got, err)
	}
	cm2 := StatusConfigMap("ns", "cm", report(), []byte("b"), false)
	if got, err := UpsertConfigMap(context.Background(), client, cm2); err != nil || got != "updated" {
		t.Fatalf("second upsert: %q %v", got, err)
	}
	stored, err := client.CoreV1().ConfigMaps("ns").Get(context.Background(), "cm", metav1.GetOptions{})
	if err != nil || stored.Data["report.yaml"] != "b" {
		t.Errorf("stored = %+v err=%v", stored.Data, err)
	}
}

func TestPodNamespace(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "namespace"), []byte("glueops-core-argocd-app-chart-preflight\n"), 0o600)
	if ns, err := PodNamespace(dir); err != nil || ns != "glueops-core-argocd-app-chart-preflight" {
		t.Errorf("ns=%q err=%v", ns, err)
	}
	if _, err := PodNamespace(t.TempDir()); err == nil {
		t.Error("missing file must error")
	}
}
