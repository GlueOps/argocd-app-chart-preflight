// Package manifests turns the YAML stream `argocd app manifests` prints into
// the pod templates the comparison works on. Same selection as the chart
// README's yq pre-flight: everything with spec.template, or
// spec.jobTemplate.spec.template for a CronJob.
package manifests

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	sigsyaml "sigs.k8s.io/yaml"
)

func bufioReader(b []byte) *bufio.Reader { return bufio.NewReader(bytes.NewReader(b)) }

// PodTemplate is one workload's PodTemplateSpec as decoded JSON
// (map[string]any / []any / string / float64 / bool / nil).
type PodTemplate struct {
	Kind     string
	Name     string
	Template map[string]any
}

// Key is how a template is addressed in the report: "Kind/name".
func (t PodTemplate) Key() string { return t.Kind + "/" + t.Name }

// Parse splits a multi-document YAML stream and decodes each document as JSON.
// Empty documents are skipped; a document that is not a mapping is skipped too
// (Helm can emit scalars/nulls between resources); a YAML error is returned.
func Parse(stream []byte) ([]map[string]any, error) {
	var docs []map[string]any
	r := utilyaml.NewYAMLReader(bufioReader(stream))
	for {
		doc, err := r.Read()
		if errors.Is(err, io.EOF) {
			return docs, nil
		}
		if err != nil {
			return nil, fmt.Errorf("manifest YAML split error: %w", err)
		}
		if len(bytes.TrimSpace(doc)) == 0 {
			continue
		}
		var v any
		if err := sigsyaml.Unmarshal(doc, &v); err != nil {
			return nil, fmt.Errorf("manifest YAML parse error: %w", err)
		}
		if m, ok := v.(map[string]any); ok {
			docs = append(docs, m)
		}
	}
}

// Reduce keeps the documents that carry a pod template.
func Reduce(docs []map[string]any) []PodTemplate {
	var out []PodTemplate
	for _, d := range docs {
		kind, _ := d["kind"].(string)
		meta, _ := d["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		if kind == "" || name == "" {
			continue
		}
		spec, _ := d["spec"].(map[string]any)
		var t any
		if kind == "CronJob" {
			jt, _ := spec["jobTemplate"].(map[string]any)
			js, _ := jt["spec"].(map[string]any)
			t = js["template"]
		} else {
			t = spec["template"]
		}
		if tm, ok := t.(map[string]any); ok {
			out = append(out, PodTemplate{Kind: kind, Name: name, Template: tm})
		}
	}
	return out
}

// ParseAndReduce is Parse followed by Reduce.
func ParseAndReduce(stream []byte) ([]PodTemplate, error) {
	docs, err := Parse(stream)
	if err != nil {
		return nil, err
	}
	return Reduce(docs), nil
}
