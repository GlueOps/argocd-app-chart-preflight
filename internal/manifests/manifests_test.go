package manifests

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRecordedVenusRenderYieldsTheDeploymentOnly(t *testing.T) {
	docs, err := Parse(fixture(t, "keda-demo-newrelic-nonprod.v0.17.1.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 6 {
		t.Fatalf("docs = %d, want 6", len(docs))
	}
	ts := Reduce(docs)
	if len(ts) != 1 || ts[0].Key() != "Deployment/keda-demo-newrelic" {
		t.Fatalf("templates = %v", ts)
	}
	spec := ts[0].Template["spec"].(map[string]any)
	c := spec["containers"].([]any)[0].(map[string]any)
	if !reflect.DeepEqual(c["command"], []any{"/bin/sh", "-c"}) {
		t.Errorf("command = %v", c["command"])
	}
	if args := c["args"].([]any); len(args) != 1 || !strings.HasPrefix(args[0].(string), "set -e") {
		t.Errorf("args = %v", c["args"])
	}
}

func TestReduceKinds(t *testing.T) {
	stream := strings.Join([]string{
		"---\n{kind: CronJob, metadata: {name: c}, spec: {jobTemplate: {spec: {template: {spec: {containers: [{name: x, args: ['1']}]}}}}}}",
		"---\n{kind: Job, metadata: {name: j}, spec: {template: {spec: {containers: [{name: x}]}}}}",
		"---\n{kind: StatefulSet, metadata: {name: s}, spec: {template: {spec: {containers: [{name: x}]}}}}",
		"---\n{kind: Service, metadata: {name: svc}, spec: {ports: []}}",
		"---\n{kind: Deployment, metadata: {}, spec: {template: {}}}",
		"---\n",
		"---\nnull",
		"---\n42",
	}, "\n")
	ts, err := ParseAndReduce([]byte(stream))
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, x := range ts {
		keys = append(keys, x.Key())
	}
	if want := []string{"CronJob/c", "Job/j", "StatefulSet/s"}; !reflect.DeepEqual(keys, want) {
		t.Errorf("keys = %v, want %v", keys, want)
	}
	c := ts[0].Template["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	if !reflect.DeepEqual(c["args"], []any{"1"}) {
		t.Errorf("cronjob args = %v", c["args"])
	}
}

func TestParseErrors(t *testing.T) {
	if _, err := Parse([]byte("a: b: c")); err == nil || !strings.Contains(err.Error(), "manifest YAML parse error") {
		t.Errorf("err = %v", err)
	}
	docs, err := Parse(nil)
	if err != nil || len(docs) != 0 {
		t.Errorf("empty stream: docs=%v err=%v", docs, err)
	}
}

// YAML 1.1 retyping is exactly what the comparison must see on a 0.17.x render:
// the decoder must hand back numbers/bools/maps, not strings.
func TestScalarsKeepTheirYAMLTypes(t *testing.T) {
	ts, err := ParseAndReduce([]byte("kind: Deployment\nmetadata: {name: d}\nspec:\n  template:\n    spec:\n      containers:\n        - name: x\n          args: [8080, yes, {a: b}, ~, 'quoted', \"  padded  \"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	args := ts[0].Template["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["args"].([]any)
	want := []any{float64(8080), true, map[string]any{"a": "b"}, nil, "quoted", "  padded  "}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("args = %#v, want %#v", args, want)
	}
}
