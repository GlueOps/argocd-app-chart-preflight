package kube

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

// ApplicationPageSize bounds how many Applications are decoded at once.
const ApplicationPageSize = 100

var applicationsGVR = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}

// NewDynamicClient builds a client from a kubeconfig path.
func NewDynamicClient(kubeconfig, userAgent string, timeout time.Duration) (dynamic.Interface, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("kubeconfig unusable: %w", err)
	}
	cfg.UserAgent = userAgent
	cfg.Timeout = timeout
	return dynamic.NewForConfig(cfg)
}

// ListApplications reads every Application in namespace, a page at a time,
// keeping only what discovery uses. `argocd app list -o json` carries each
// object whole: at 750 Applications that is 25 MB of JSON, 210 MB in the CLI and
// as much again here, which is enough to OOM a 512Mi pod during discovery.
func ListApplications(ctx context.Context, client dynamic.Interface, namespace string) ([]map[string]any, error) {
	var out []map[string]any
	opts := metav1.ListOptions{Limit: ApplicationPageSize}
	for {
		page, err := client.Resource(applicationsGVR).Namespace(namespace).List(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("list applications: %w", err)
		}
		for i := range page.Items {
			out = append(out, slimApplication(page.Items[i].Object))
		}
		if opts.Continue = page.GetContinue(); opts.Continue == "" {
			return out, nil
		}
	}
}

func slimApplication(o map[string]any) map[string]any {
	meta, _ := o["metadata"].(map[string]any)
	status, _ := o["status"].(map[string]any)
	sync, _ := status["sync"].(map[string]any)
	return map[string]any{
		"metadata": map[string]any{"name": meta["name"], "namespace": meta["namespace"], "labels": meta["labels"], "ownerReferences": meta["ownerReferences"]},
		"spec":     o["spec"],
		"status":   map[string]any{"sync": map[string]any{"status": sync["status"]}},
	}
}
