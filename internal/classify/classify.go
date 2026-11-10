// Package classify is the one 0.18.0-specific module: it names what a changed
// command/args item means for the tenant. It is matched on the rendered
// (old, new) pair — what the current chart delivers vs what the target would —
// so it observes the chart's behaviour instead of predicting it from values
// files. Rows mirror the "Upgrading from 0.17.x" table in the chart's README.
package classify

import (
	"fmt"
	"strings"

	"github.com/glueops/argocd-app-chart-preflight/internal/schema"
)

// Absent marks an index that exists on one side only (the list length changed).
// It is distinct from nil, which is a rendered YAML null.
type absent struct{}

// Absent is the sentinel for a missing list index.
var Absent any = absent{}

// Result is the classification of one finding.
type Result struct {
	Category string
	Impact   string
	Hint     string
}

// Classify names the (old, new) pair. Inputs are rendered values as decoded from
// JSON: string, float64, bool, map[string]any, []any, nil, or Absent.
func Classify(oldV, newV any) Result {
	if oldV == Absent || newV == Absent {
		return Result{schema.CategoryLengthChanged, schema.ImpactRoll, "the list has a different number of items at the target revision; inspect both sides"}
	}
	if (oldV == nil && newV == "") || (oldV == "" && newV == nil) {
		return Result{schema.CategoryNullItem, schema.ImpactNone, `the API server stored "" for both; no rollout`}
	}
	o, oldIsStr := oldV.(string)
	n, newIsStr := newV.(string)
	if oldIsStr && newIsStr {
		switch {
		case n == `"`+o+`"` || n == `'`+o+`'`:
			return Result{schema.CategoryDoubleQuoted, schema.ImpactBreak, "the item is wrapped in a second pair of quotes; 0.17.x stripped them, the target keeps them — remove the outer quotes before upgrading"}
		case strings.HasPrefix(n, o+" #"):
			return Result{schema.CategoryCommentTruncated, schema.ImpactRoll, "' #' was treated as a YAML comment; the full value is delivered from the target revision"}
		case o == "" && strings.HasPrefix(n, "#"):
			return Result{schema.CategoryLeadingHash, schema.ImpactRoll, "a leading '#' emptied the item; the full value is delivered from the target revision"}
		case strings.TrimSpace(n) == o && n != o:
			return Result{schema.CategoryPadded, schema.ImpactRoll, "leading/trailing spaces are preserved from the target revision"}
		}
		return Result{schema.CategoryOther, schema.ImpactRoll, "the item renders differently at the target revision; inspect both sides"}
	}
	if !oldIsStr && newIsStr {
		return Result{schema.CategoryRetyped, schema.ImpactRoll, fmt.Sprintf("0.17.x rendered this item as a YAML %s; the target delivers the string", describeType(oldV))}
	}
	return Result{schema.CategoryOther, schema.ImpactRoll, "the item renders differently at the target revision; inspect both sides"}
}

func describeType(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case []any:
		return "list"
	case map[string]any:
		return "map"
	case bool:
		return "boolean"
	case float64, int, int64:
		return "number"
	default:
		return fmt.Sprintf("%T", v)
	}
}
