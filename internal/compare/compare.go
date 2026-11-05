// Package compare diffs the pod templates of two renders of the same app
// (current chart vs target). command/args items are compared one by one per
// container and become findings; everything else that differs in the pod
// template is reported as paths only (other_changes) — real, but not this
// upgrade's business, and on captains several versions behind there can be many.
package compare

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"

	"github.com/glueops/argocd-app-chart-preflight/internal/classify"
	"github.com/glueops/argocd-app-chart-preflight/internal/manifests"
	"github.com/glueops/argocd-app-chart-preflight/internal/schema"
)

// PodTemplate is one workload's PodTemplateSpec as decoded JSON (see package manifests).
type PodTemplate = manifests.PodTemplate

// Result of comparing two renders. Impact is the highest finding impact, or ""
// when the command/args are identical.
type Result struct {
	Diff         map[string]schema.ResourceDiff
	OtherChanges map[string][]string
	Impact       string
}

var impactRank = map[string]int{"": -1, schema.ImpactNone: 0, schema.ImpactRoll: 1, schema.ImpactBreak: 2}

// Compare the current render (old) with the target render (new).
func Compare(oldT, newT []PodTemplate) Result {
	res := Result{Diff: map[string]schema.ResourceDiff{}, OtherChanges: map[string][]string{}}
	oldM := index(oldT)
	newM := index(newT)
	for _, k := range unionKeys(oldM, newM) {
		o, haveOld := oldM[k]
		n, haveNew := newM[k]
		if !haveOld || !haveNew {
			if haveOld {
				res.OtherChanges[k] = []string{"(resource absent at target revision)"}
			} else {
				res.OtherChanges[k] = []string{"(resource added at target revision)"}
			}
			continue
		}
		rd := schema.ResourceDiff{}
		for _, typ := range []string{schema.ContainersKey, schema.InitContainersKey} {
			oc := containersOf(o, typ)
			nc := containersOf(n, typ)
			for _, name := range unionKeys(oc, nc) {
				cd := schema.ContainerDiff{}
				for _, field := range []string{schema.FieldCommand, schema.FieldArgs} {
					fd := compareField(oc[name], nc[name], field)
					if len(fd) == 0 {
						continue
					}
					for _, f := range fd {
						if impactRank[f.Impact] > impactRank[res.Impact] {
							res.Impact = f.Impact
						}
					}
					if field == schema.FieldCommand {
						cd.Command = fd
					} else {
						cd.Args = fd
					}
				}
				if cd.Command == nil && cd.Args == nil {
					continue
				}
				if typ == schema.ContainersKey {
					if rd.Containers == nil {
						rd.Containers = map[string]schema.ContainerDiff{}
					}
					rd.Containers[name] = cd
				} else {
					if rd.InitContainers == nil {
						rd.InitContainers = map[string]schema.ContainerDiff{}
					}
					rd.InitContainers[name] = cd
				}
			}
		}
		if rd.Containers != nil || rd.InitContainers != nil {
			res.Diff[k] = rd
		}
		if paths := DiffPaths(withoutCommandArgs(o.Template), withoutCommandArgs(n.Template), "spec.template"); len(paths) > 0 {
			res.OtherChanges[k] = paths
		}
	}
	return res
}

func index(ts []PodTemplate) map[string]PodTemplate {
	m := make(map[string]PodTemplate, len(ts))
	for _, t := range ts {
		m[t.Key()] = t
	}
	return m
}

func unionKeys[V any](a, b map[string]V) []string {
	set := map[string]struct{}{}
	for k := range a {
		set[k] = struct{}{}
	}
	for k := range b {
		set[k] = struct{}{}
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// containersOf returns the named containers of a type; unnamed ones get "#i".
func containersOf(t PodTemplate, typ string) map[string]map[string]any {
	out := map[string]map[string]any{}
	spec, _ := t.Template["spec"].(map[string]any)
	list, _ := spec[typ].([]any)
	for i, c := range list {
		o, ok := c.(map[string]any)
		if !ok {
			continue
		}
		name, _ := o["name"].(string)
		if name == "" {
			name = "#" + strconv.Itoa(i)
		}
		out[name] = o
	}
	return out
}

func compareField(oldC, newC map[string]any, field string) map[string]schema.Finding {
	o, _ := oldC[field].([]any)
	n, _ := newC[field].([]any)
	out := map[string]schema.Finding{}
	for i := 0; i < len(o) || i < len(n); i++ {
		ov, nv := classify.Absent, classify.Absent
		if i < len(o) {
			ov = o[i]
		}
		if i < len(n) {
			nv = n[i]
		}
		if i < len(o) && i < len(n) && reflect.DeepEqual(ov, nv) {
			continue
		}
		c := classify.Classify(ov, nv)
		out[strconv.Itoa(i)] = schema.Finding{Old: nilIfAbsent(ov), New: nilIfAbsent(nv), Category: c.Category, Impact: c.Impact, Hint: c.Hint}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func nilIfAbsent(v any) any {
	if v == classify.Absent {
		return nil
	}
	return v
}

// DiffPaths lists the leaf paths that differ between two JSON values, dotted,
// with "[i]" for list indexes.
func DiffPaths(a, b any, prefix string) []string {
	if reflect.DeepEqual(a, b) {
		return nil
	}
	ao, aIsObj := a.(map[string]any)
	bo, bIsObj := b.(map[string]any)
	if aIsObj && bIsObj {
		var out []string
		for _, k := range unionKeys(ao, bo) {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			out = append(out, DiffPaths(ao[k], bo[k], p)...)
		}
		return out
	}
	aa, aIsArr := a.([]any)
	ba, bIsArr := b.([]any)
	if aIsArr && bIsArr {
		var out []string
		for i := 0; i < len(aa) || i < len(ba); i++ {
			var x, y any
			if i < len(aa) {
				x = aa[i]
			}
			if i < len(ba) {
				y = ba[i]
			}
			out = append(out, DiffPaths(x, y, fmt.Sprintf("%s[%d]", prefix, i))...)
		}
		return out
	}
	if prefix == "" {
		return []string{"(root)"}
	}
	return []string{prefix}
}

// withoutCommandArgs deep-copies a template with command/args removed from
// every container, for the other_changes compare.
func withoutCommandArgs(t map[string]any) any {
	clone := deepCopy(t).(map[string]any)
	if spec, ok := clone["spec"].(map[string]any); ok {
		for _, typ := range []string{schema.ContainersKey, schema.InitContainersKey} {
			if list, ok := spec[typ].([]any); ok {
				for _, c := range list {
					if o, ok := c.(map[string]any); ok {
						delete(o, schema.FieldCommand)
						delete(o, schema.FieldArgs)
					}
				}
			}
		}
	}
	return clone
}

func deepCopy(v any) any {
	switch x := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, val := range x {
			m[k] = deepCopy(val)
		}
		return m
	case []any:
		s := make([]any, len(x))
		for i, val := range x {
			s[i] = deepCopy(val)
		}
		return s
	default:
		return v
	}
}
