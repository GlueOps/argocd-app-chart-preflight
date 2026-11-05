package kube

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/glueops/argocd-app-chart-preflight/internal/schema"
)

// ManagedByLabel marks the status ConfigMap.
const ManagedByLabel = "argocd-app-chart-preflight"

// PodNamespace reads the pod's own namespace from the ServiceAccount mount.
func PodNamespace(serviceAccountDir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(serviceAccountDir, "namespace"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// StatusConfigMap builds the ConfigMap: flat keys first so
// `kubectl get cm -o jsonpath='{.data.status}'` answers without parsing the
// report, then the YAML report under report.yaml.
func StatusConfigMap(namespace, name string, r *schema.Report, yamlText []byte) *corev1.ConfigMap {
	target := ""
	if r.Run.TargetRevision != nil {
		target = *r.Run.TargetRevision
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{"app.kubernetes.io/managed-by": ManagedByLabel}},
		Data: map[string]string{
			"status":          r.Run.Status,
			"target_revision": target,
			"finished_at":     r.Run.FinishedAt,
			"schema_version":  strconv.Itoa(r.SchemaVersion),
			"report.yaml":     string(yamlText),
		},
	}
}

// UpsertConfigMap creates or replaces the status ConfigMap. Returns "created"
// or "updated".
func UpsertConfigMap(ctx context.Context, client kubernetes.Interface, cm *corev1.ConfigMap) (string, error) {
	api := client.CoreV1().ConfigMaps(cm.Namespace)
	existing, err := api.Get(ctx, cm.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if _, err := api.Create(ctx, cm, metav1.CreateOptions{}); err != nil {
			return "", fmt.Errorf("configmap create failed: %w", err)
		}
		return "created", nil
	}
	if err != nil {
		return "", fmt.Errorf("configmap get failed: %w", err)
	}
	cm.ResourceVersion = existing.ResourceVersion
	if _, err := api.Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		return "", fmt.Errorf("configmap update failed: %w", err)
	}
	return "updated", nil
}
