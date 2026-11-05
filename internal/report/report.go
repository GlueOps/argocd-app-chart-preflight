// Package report rolls per-app verdicts up to ApplicationSets and the run,
// and serializes the report: YAML for the ConfigMap and the receiver (humans
// and the receiver's parser), JSON for log lines.
package report

import (
	"encoding/json"
	"sort"

	"github.com/glueops/argocd-app-chart-preflight/internal/schema"
)

// Ladder: error beats diff beats unknown beats ok; skipped only when nothing
// else exists. ok-noop-diff counts as ok.
var appRank = map[string]int{schema.AppSkipped: -1, schema.AppOK: 0, schema.AppOKNoopDiff: 0, schema.AppUnknown: 1, schema.AppDiff: 2, schema.AppError: 3}
var groupRank = map[string]int{schema.GroupSkipped: -1, schema.GroupOK: 0, schema.GroupUnknown: 1, schema.GroupDiff: 2, schema.GroupError: 3}
var groupByRank = []string{schema.GroupOK, schema.GroupUnknown, schema.GroupDiff, schema.GroupError}

// GroupStatus of a set from its apps.
func GroupStatus(apps []schema.App) string {
	max := -1
	for _, a := range apps {
		if r := appRank[a.Status]; r > max {
			max = r
		}
	}
	if max < 0 {
		return schema.GroupSkipped
	}
	return groupByRank[max]
}

// RunStatus from the sets.
func RunStatus(sets []schema.AppSet) string {
	max := -1
	for _, s := range sets {
		if r := groupRank[s.Status]; r > max {
			max = r
		}
	}
	if max < 0 {
		return schema.GroupSkipped
	}
	return groupByRank[max]
}

// SummarizeApps counts apps by status.
func SummarizeApps(apps []schema.App) schema.AppSummary {
	s := schema.AppSummary{Apps: len(apps)}
	for _, a := range apps {
		switch a.Status {
		case schema.AppOK:
			s.OK++
		case schema.AppOKNoopDiff:
			s.OKNoopDiff++
		case schema.AppDiff:
			s.Diff++
		case schema.AppError:
			s.Error++
		case schema.AppUnknown:
			s.Unknown++
		case schema.AppSkipped:
			s.Skipped++
		}
	}
	return s
}

// SummarizeSets counts sets by status.
func SummarizeSets(sets []schema.AppSet, appsIgnored int) schema.RunSummary {
	s := schema.RunSummary{AppSets: len(sets), AppsIgnored: appsIgnored}
	for _, g := range sets {
		switch g.Status {
		case schema.GroupOK:
			s.OK++
		case schema.GroupDiff:
			s.Diff++
		case schema.GroupError:
			s.Error++
		case schema.GroupUnknown:
			s.Unknown++
		case schema.GroupSkipped:
			s.Skipped++
		}
	}
	return s
}

// ToJSON is the wire/log encoding.
func ToJSON(r *schema.Report) ([]byte, error) { return json.Marshal(r) }

// FindingLine is one finding flattened for a log line.
type FindingLine struct {
	Resource      string `json:"resource"`
	Container     string `json:"container"`
	ContainerType string `json:"container_type"`
	Field         string `json:"field"`
	Index         string `json:"index"`
	Old           any    `json:"old"`
	New           any    `json:"new"`
	Category      string `json:"category"`
	Impact        string `json:"impact"`
}

// Findings flattens an app's diff map in a stable order (for log lines and tests).
func Findings(app *schema.App) []FindingLine {
	var out []FindingLine
	resources := make([]string, 0, len(app.Diff))
	for k := range app.Diff {
		resources = append(resources, k)
	}
	sort.Strings(resources)
	for _, res := range resources {
		rd := app.Diff[res]
		for _, typ := range []struct {
			name string
			m    map[string]schema.ContainerDiff
		}{{schema.ContainersKey, rd.Containers}, {schema.InitContainersKey, rd.InitContainers}} {
			names := make([]string, 0, len(typ.m))
			for n := range typ.m {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				cd := typ.m[n]
				for _, field := range []struct {
					name string
					m    map[string]schema.Finding
				}{{schema.FieldCommand, cd.Command}, {schema.FieldArgs, cd.Args}} {
					idx := make([]string, 0, len(field.m))
					for i := range field.m {
						idx = append(idx, i)
					}
					sort.Strings(idx)
					for _, i := range idx {
						f := field.m[i]
						out = append(out, FindingLine{Resource: res, Container: n, ContainerType: typ.name, Field: field.name, Index: i, Old: f.Old, New: f.New, Category: f.Category, Impact: f.Impact})
					}
				}
			}
		}
	}
	return out
}

// FitYAML returns the report's YAML for a store with a size limit: the full
// text when it fits, else only the apps that need attention, else the roll-ups
// alone. Summaries always describe the whole run. truncated says apps were cut.
func FitYAML(r *schema.Report, full []byte, maxBytes int) (text []byte, truncated bool, err error) {
	if len(full) <= maxBytes {
		return full, false, nil
	}
	for _, keep := range []func(schema.App) bool{needsAttention, func(schema.App) bool { return false }} {
		if text, err = ToYAML(withApps(r, keep)); err != nil {
			return nil, false, err
		}
		if len(text) <= maxBytes {
			break
		}
	}
	return text, true, nil
}

func needsAttention(a schema.App) bool {
	return a.Status == schema.AppDiff || a.Status == schema.AppError || a.Status == schema.AppUnknown
}

func withApps(r *schema.Report, keep func(schema.App) bool) *schema.Report {
	out := *r
	out.AppSets = make([]schema.AppSet, len(r.AppSets))
	for i, s := range r.AppSets {
		apps := []schema.App{}
		for _, a := range s.Apps {
			if keep(a) {
				apps = append(apps, a)
			}
		}
		s.Apps = apps
		out.AppSets[i] = s
	}
	return &out
}
