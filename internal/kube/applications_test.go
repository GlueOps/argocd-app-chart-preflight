package kube

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

func application(name string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
		"metadata": map[string]any{
			"name": name, "namespace": "glueops-core",
			"labels":          map[string]any{"argocd.argoproj.io/application-set-name": "s"},
			"ownerReferences": []any{map[string]any{"kind": "ApplicationSet", "name": "s"}},
			"annotations":     map[string]any{"big": strings.Repeat("x", 100)},
			"managedFields":   []any{map[string]any{"manager": "m"}},
		},
		"spec":   map[string]any{"project": "p", "sources": []any{map[string]any{"repoURL": "r"}}},
		"status": map[string]any{"sync": map[string]any{"status": "Synced", "revision": "abc"}, "resources": []any{"x"}, "history": []any{"y"}},
	}}
}

func pagedClient(t *testing.T, pages ...[]string) (*dynamicfake.FakeDynamicClient, *[]metav1.ListOptions) {
	t.Helper()
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{applicationsGVR: "ApplicationList"})
	var seen []metav1.ListOptions
	client.PrependReactor("list", "applications", func(a k8stesting.Action) (bool, runtime.Object, error) {
		opts := a.(k8stesting.ListActionImpl).ListOptions
		seen = append(seen, opts)
		i := 0
		if opts.Continue != "" {
			i, _ = strconv.Atoi(opts.Continue)
		}
		list := &unstructured.UnstructuredList{Object: map[string]any{"apiVersion": "argoproj.io/v1alpha1", "kind": "ApplicationList"}}
		for _, name := range pages[i] {
			list.Items = append(list.Items, application(name))
		}
		if i+1 < len(pages) {
			list.SetContinue(strconv.Itoa(i + 1))
		}
		return true, list, nil
	})
	return client, &seen
}

func TestListApplicationsPagesAndKeepsOnlyWhatDiscoveryUses(t *testing.T) {
	client, seen := pagedClient(t, []string{"a", "b"}, []string{"c"})
	apps, err := ListApplications(context.Background(), client, "glueops-core")
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 3 || len(*seen) != 2 || (*seen)[0].Limit != ApplicationPageSize || (*seen)[1].Continue != "1" {
		t.Fatalf("apps=%d requests=%+v", len(apps), *seen)
	}
	want := map[string]any{
		"metadata": map[string]any{"name": "a", "namespace": "glueops-core", "labels": map[string]any{"argocd.argoproj.io/application-set-name": "s"}, "ownerReferences": []any{map[string]any{"kind": "ApplicationSet", "name": "s"}}},
		"spec":     map[string]any{"project": "p", "sources": []any{map[string]any{"repoURL": "r"}}},
		"status":   map[string]any{"sync": map[string]any{"status": "Synced"}},
	}
	if !reflect.DeepEqual(apps[0], want) {
		t.Errorf("app = %+v", apps[0])
	}
}

func TestListApplicationsFailure(t *testing.T) {
	client, _ := pagedClient(t, []string{"a"})
	client.PrependReactor("list", "applications", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("applications.argoproj.io is forbidden")
	})
	if _, err := ListApplications(context.Background(), client, "glueops-core"); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("err = %v", err)
	}
}
