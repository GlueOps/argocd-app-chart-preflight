package classify

import (
	"strings"
	"testing"

	"github.com/glueops/argocd-app-chart-preflight/internal/schema"
)

// One row per line of the README's "Upgrading from 0.17.x" table, as the
// rendered (old, new) pair.
func TestClassifyTable(t *testing.T) {
	cases := []struct {
		name     string
		old, new any
		category string
		impact   string
	}{
		{"double-quoted workaround", "echo a: b", `"echo a: b"`, schema.CategoryDoubleQuoted, schema.ImpactBreak},
		{"single-quoted workaround", "echo a: b", "'echo a: b'", schema.CategoryDoubleQuoted, schema.ImpactBreak},
		{"' #' treated as a comment", "--note", "--note #keep", schema.CategoryCommentTruncated, schema.ImpactRoll},
		{"leading # emptied the item", "", "#not-a-comment", schema.CategoryLeadingHash, schema.ImpactRoll},
		{"padding trimmed", "padded", "  padded  ", schema.CategoryPadded, schema.ImpactRoll},
		{"null delivered as empty string", nil, "", schema.CategoryNullItem, schema.ImpactNone},
		{"empty string vs null (reverse)", "", nil, schema.CategoryNullItem, schema.ImpactNone},
		{"8080 retyped to number", float64(8080), "8080", schema.CategoryRetyped, schema.ImpactRoll},
		{"yes retyped to bool", true, "yes", schema.CategoryRetyped, schema.ImpactRoll},
		{"map item rendered as Go text", map[string]any{"a": "b"}, "a: b", schema.CategoryRetyped, schema.ImpactRoll},
		{"index missing on one side", Absent, "x", schema.CategoryLengthChanged, schema.ImpactRoll},
		{"index missing on the other side", "x", Absent, schema.CategoryLengthChanged, schema.ImpactRoll},
		{"anything else", "a", "b", schema.CategoryOther, schema.ImpactRoll},
		{"non-string on both sides", float64(1), float64(2), schema.CategoryOther, schema.ImpactRoll},
	}
	for _, c := range cases {
		r := Classify(c.old, c.new)
		if r.Category != c.category || r.Impact != c.impact {
			t.Errorf("%s: got %s/%s, want %s/%s", c.name, r.Category, r.Impact, c.category, c.impact)
		}
		if len(r.Hint) < 10 {
			t.Errorf("%s: hint too short: %q", c.name, r.Hint)
		}
	}
}

func TestRetypedHintNamesTheYAMLType(t *testing.T) {
	for v, want := range map[string]any{"boolean": true, "map": map[string]any{"a": 1}, "list": []any{1}, "number": float64(1), "null": nil} {
		// nil → "" is null_item, so probe null with a non-empty new value.
		r := Classify(want, "x")
		if !strings.Contains(r.Hint, "YAML "+v) {
			t.Errorf("%v: hint %q does not name YAML %s", want, r.Hint, v)
		}
	}
}
