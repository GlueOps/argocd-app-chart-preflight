// Package textreport prints a preflight report for an operator's terminal:
// one table of ApplicationSets, then per set the apps that need attention
// (findings with old and new values, render errors, missing verdicts), the
// pod-template diff of every app that changes, and a verdict line naming the
// sets that are safe to bump.
//
// It reads only the report and, for the diff, the two renders operator mode
// kept for each changing app. The report schema is not involved.
package textreport

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pmezard/go-difflib/difflib"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/glueops/argocd-app-chart-preflight/internal/manifests"
	"github.com/glueops/argocd-app-chart-preflight/internal/report"
	"github.com/glueops/argocd-app-chart-preflight/internal/schema"
)

// Options control what is printed.
type Options struct {
	Color    bool          // ANSI colours (only for a terminal)
	Wide     bool          // no truncation of values, hints and reasons
	All      bool          // also list clean and skipped apps one by one
	Diff     bool          // print the pod-template diff of every changing app
	Context  string        // kube context, for the header
	Duration time.Duration // how long the run took, for the header
}

// Pair is one app's two reduced renders: the current chart and the target.
type Pair struct {
	Current, Target []manifests.PodTemplate
}

// Renders holds the Pair of each changing app, keyed by Key(appSet, app).
type Renders map[string]Pair

// Key addresses an app in Renders.
func Key(appSet, app string) string { return appSet + "\x00" + app }

// Truncation limits when Options.Wide is off.
const (
	maxValue  = 40
	maxHint   = 60
	maxReason = 110
)

// ChartVersionKeys are pod-template labels and annotations that carry the
// chart version or name, so they change on every bump. The diff leaves them out.
var ChartVersionKeys = []string{"helm.sh/chart", "glueops.dev/chart", "app.kubernetes.io/version"}

// Write prints the report.
func Write(w io.Writer, rep *schema.Report, renders Renders, o Options) error {
	p := &printer{w: w, o: o}
	p.header(rep)
	p.setsTable(rep)
	for i := range rep.AppSets {
		p.set(&rep.AppSets[i])
	}
	if o.Diff {
		p.diffs(rep, renders)
	}
	p.verdict(rep)
	return p.err
}

type printer struct {
	w   io.Writer
	o   Options
	err error
}

func (p *printer) printf(format string, a ...any) {
	if p.err == nil {
		_, p.err = fmt.Fprintf(p.w, format, a...)
	}
}

// ---- colour -----------------------------------------------------------------------------------

const (
	red     = "31"
	green   = "32"
	yellow  = "33"
	magenta = "35"
	cyan    = "36"
	dim     = "2"
	bold    = "1"
)

func (p *printer) c(code, s string) string {
	if !p.o.Color || code == "" || s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func statusColor(s string) string {
	switch s {
	case schema.AppOK, schema.AppOKNoopDiff:
		return green
	case schema.AppDiff:
		return yellow
	case schema.AppError:
		return red
	case schema.AppUnknown:
		return magenta
	case schema.AppSkipped:
		return dim
	}
	return ""
}

func impactColor(s string) string {
	switch s {
	case schema.ImpactBreak:
		return red + ";" + bold
	case schema.ImpactRoll:
		return yellow
	case schema.ImpactNone:
		return dim
	}
	return ""
}

// ---- tables -----------------------------------------------------------------------------------

type cell struct {
	text  string
	color string
}

type table struct {
	head  []string
	right map[int]bool // right-aligned columns (counts)
	rows  [][]cell
}

func (t *table) add(cells ...cell) { t.rows = append(t.rows, cells) }

func plain(s string) cell { return cell{text: s} }

// render pads on the visible text, then colours, so escape codes never skew
// the columns (text/tabwriter counts them as width).
func (p *printer) table(t *table, indent string) {
	widths := make([]int, len(t.head))
	for i, h := range t.head {
		widths[i] = utf8.RuneCountInString(h)
	}
	for _, r := range t.rows {
		for i, c := range r {
			if n := utf8.RuneCountInString(c.text); n > widths[i] {
				widths[i] = n
			}
		}
	}
	line := func(cells []cell, header bool) {
		var b strings.Builder
		b.WriteString(indent)
		for i, c := range cells {
			pad := strings.Repeat(" ", widths[i]-utf8.RuneCountInString(c.text))
			text := c.text
			if header {
				text = p.c(bold, text)
			} else {
				text = p.c(c.color, text)
			}
			last := i == len(cells)-1
			switch {
			case t.right[i]:
				b.WriteString(pad + text)
			case last:
				b.WriteString(text)
			default:
				b.WriteString(text + pad)
			}
			if !last {
				b.WriteString("  ")
			}
		}
		p.printf("%s\n", strings.TrimRight(b.String(), " "))
	}
	head := make([]cell, len(t.head))
	for i, h := range t.head {
		head[i] = plain(h)
	}
	line(head, true)
	for _, r := range t.rows {
		line(r, false)
	}
}

// ---- values -----------------------------------------------------------------------------------

// Value renders a command/args item so quotes, spaces and empty strings stay
// visible: strings Go-quoted, an absent item as (none), anything else as JSON.
func Value(v any) string {
	switch x := v.(type) {
	case nil:
		return "(none)"
	case string:
		return strconv.Quote(x)
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return fmt.Sprint(x)
		}
		return string(b)
	}
}

func (p *printer) cut(s string, n int) string {
	if p.o.Wide || utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

func orDash(s *string) string {
	if s == nil || *s == "" {
		return "-"
	}
	return *s
}

// ---- sections ---------------------------------------------------------------------------------

func (p *printer) header(rep *schema.Report) {
	r := rep.Run
	parts := []string{p.c(bold, "App chart preflight"), r.CaptainDomain}
	if p.o.Context != "" {
		parts = append(parts, "context "+p.o.Context)
	}
	if r.ArgocdCLIVersion != nil {
		parts = append(parts, "argocd "+*r.ArgocdCLIVersion)
	}
	p.printf("%s\n", strings.Join(parts, "  "))
	apps := 0
	for _, s := range rep.AppSets {
		apps += s.Summary.Apps
	}
	target := "per ApplicationSet"
	if r.TargetRevision != nil {
		target = *r.TargetRevision
	}
	line := fmt.Sprintf("Target %s · %d ApplicationSets · %d apps", target, len(rep.AppSets), apps)
	if p.o.Duration > 0 {
		line += " checked in " + p.o.Duration.Round(time.Second).String()
	}
	p.printf("%s\n\n", line)
}

func (p *printer) setsTable(rep *schema.Report) {
	if len(rep.AppSets) == 0 {
		p.printf("No ApplicationSet or Application uses the app chart.\n")
		return
	}
	t := &table{head: []string{"APPLICATIONSET", "SOURCE", "CURRENT", "TARGET", "STATUS", "APPS", "OK", "DIFF", "ERROR", "UNKNOWN", "SKIPPED"},
		right: map[int]bool{5: true, 6: true, 7: true, 8: true, 9: true, 10: true}}
	n := func(i int) cell { return plain(strconv.Itoa(i)) }
	for _, s := range rep.AppSets {
		m := s.Summary
		t.add(plain(s.Name), plain(orDash(s.SourceType)), plain(orDash(s.CurrentRevision)), plain(orDash(s.TargetRevision)),
			cell{s.Status, statusColor(s.Status)}, n(m.Apps), n(m.OK+m.OKNoopDiff), n(m.Diff), n(m.Error), n(m.Unknown), n(m.Skipped))
	}
	p.table(t, "")
}

var setMeaning = map[string]string{
	schema.GroupOK:      "safe to bump",
	schema.GroupDiff:    "bumping changes these pods; fix the values first, or accept the rollout",
	schema.GroupError:   "the target does not render for these apps; fix the values first",
	schema.GroupUnknown: "no verdict for these apps; check the reason and run again",
	schema.GroupSkipped: "not checked",
}

func (p *printer) set(s *schema.AppSet) {
	var changed, errs, unknown, clean, skipped []*schema.App
	for i := range s.Apps {
		a := &s.Apps[i]
		switch a.Status {
		case schema.AppDiff:
			changed = append(changed, a)
		case schema.AppOKNoopDiff:
			clean = append(clean, a) // null -> "" only: nothing changes in the pod
		case schema.AppError:
			errs = append(errs, a)
		case schema.AppUnknown:
			unknown = append(unknown, a)
		case schema.AppSkipped:
			skipped = append(skipped, a)
		default:
			clean = append(clean, a)
		}
	}
	attention := len(changed)+len(errs)+len(unknown) > 0 || s.Reason != nil
	if !attention && !p.o.All && len(skipped) == 0 {
		return
	}
	p.printf("\n%s — %s: %s\n", p.c(bold, s.Name), p.c(statusColor(s.Status), strings.ToUpper(s.Status)), setMeaning[s.Status])
	if s.Reason != nil {
		p.printf("  reason: %s\n", *s.Reason)
	}
	if len(changed) > 0 {
		p.findings(changed)
	}
	if len(errs) > 0 {
		p.reasons("Render errors", errs)
	}
	if len(unknown) > 0 {
		p.reasons("No verdict", unknown)
	}
	if p.o.All && len(clean) > 0 {
		t := &table{head: []string{"APP", "NAMESPACE", "SYNC", "STATUS"}}
		for _, a := range clean {
			t.add(plain(a.Name), plain(a.Namespace), plain(orDash(a.SyncStatus)), cell{a.Status, statusColor(a.Status)})
		}
		p.printf("\n  Clean\n")
		p.table(t, "  ")
	}
	if len(skipped) > 0 {
		if p.o.All {
			p.reasons("Skipped", skipped)
		} else {
			p.skippedSummary(skipped)
		}
	}
}

func (p *printer) findings(apps []*schema.App) {
	t := &table{head: []string{"APP", "SYNC", "RESOURCE", "CONTAINER", "FIELD", "IMPACT", "OLD", "NEW", "HINT"}}
	for _, a := range apps {
		lines := report.Findings(a)
		for i, f := range lines {
			name, sync := "", ""
			if i == 0 {
				name, sync = a.Name, orDash(a.SyncStatus)
			}
			container := f.Container
			if f.ContainerType == schema.InitContainersKey {
				container = "init:" + container
			}
			t.add(plain(name), plain(sync), plain(f.Resource), plain(container), plain(fmt.Sprintf("%s[%s]", f.Field, f.Index)),
				cell{f.Impact, impactColor(f.Impact)}, plain(p.cut(Value(f.Old), maxValue)), plain(p.cut(Value(f.New), maxValue)), plain(p.cut(f.Hint, maxHint)))
		}
	}
	p.printf("\n  Changes (command/args items, current chart -> target)\n")
	p.table(t, "  ")
	for _, a := range apps {
		extra := ""
		if n := len(a.OtherChanges); n > 0 {
			extra = fmt.Sprintf("; also changes other pod-template fields in %d resource(s), see the diff", n)
		}
		p.printf("  %s (namespace %s) values: %s%s\n", p.c(dim, a.Name), a.Namespace, strings.Join(a.ValuesFiles, ", "), extra)
	}
}

func (p *printer) reasons(title string, apps []*schema.App) {
	t := &table{head: []string{"APP", "NAMESPACE", "SYNC", "REASON"}}
	for _, a := range apps {
		reason := ""
		if a.Reason != nil {
			reason = *a.Reason
		}
		t.add(plain(a.Name), plain(a.Namespace), plain(orDash(a.SyncStatus)), cell{p.cut(reason, maxReason), statusColor(a.Status)})
	}
	p.printf("\n  %s\n", title)
	p.table(t, "  ")
}

func (p *printer) skippedSummary(apps []*schema.App) {
	counts := map[string]int{}
	for _, a := range apps {
		r := "skipped"
		if a.Reason != nil {
			r = *a.Reason
		}
		counts[r]++
	}
	reasons := make([]string, 0, len(counts))
	for r := range counts {
		reasons = append(reasons, r)
	}
	sort.Strings(reasons)
	for _, r := range reasons {
		p.printf("  %s\n", p.c(dim, fmt.Sprintf("%d skipped: %s", counts[r], p.cut(r, maxReason))))
	}
}

// ---- diff -------------------------------------------------------------------------------------

func (p *printer) diffs(rep *schema.Report, renders Renders) {
	first := true
	for _, s := range rep.AppSets {
		for i := range s.Apps {
			a := &s.Apps[i]
			if a.Status != schema.AppDiff {
				continue
			}
			pair, ok := renders[Key(s.Name, a.Name)]
			if !ok {
				continue
			}
			text := PodTemplateDiff(pair, orDash(a.CurrentRevision), orDash(s.TargetRevision))
			if text == "" {
				continue
			}
			if first {
				p.printf("\n%s\n", p.c(bold, "Pod template diffs (current chart -> target, chart-version labels left out)"))
				first = false
			}
			p.printf("\n%s\n", p.c(bold, s.Name+" / "+a.Name))
			for _, l := range strings.SplitAfter(text, "\n") {
				switch {
				case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"):
					p.printf("%s", p.c(bold, l))
				case strings.HasPrefix(l, "@@"):
					p.printf("%s", p.c(cyan, l))
				case strings.HasPrefix(l, "+"):
					p.printf("%s", p.c(green, l))
				case strings.HasPrefix(l, "-"):
					p.printf("%s", p.c(red, l))
				default:
					p.printf("%s", l)
				}
			}
		}
	}
}

// PodTemplateDiff is a unified diff, resource by resource, of the two renders'
// pod templates as YAML with ChartVersionKeys removed. "" when nothing else differs.
func PodTemplateDiff(pair Pair, fromRev, toRev string) string {
	index := func(ts []manifests.PodTemplate) map[string]manifests.PodTemplate {
		m := map[string]manifests.PodTemplate{}
		for _, t := range ts {
			m[t.Key()] = t
		}
		return m
	}
	oldM, newM := index(pair.Current), index(pair.Target)
	keys := map[string]bool{}
	for k := range oldM {
		keys[k] = true
	}
	for k := range newM {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	var out strings.Builder
	for _, k := range sorted {
		a, b := templateYAML(oldM[k]), templateYAML(newM[k])
		if a == b {
			continue
		}
		d, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
			A: lines(a), B: lines(b),
			FromFile: k + " @ " + fromRev, ToFile: k + " @ " + toRev, Context: 3,
		})
		if err != nil {
			continue
		}
		out.WriteString(d)
	}
	return out.String()
}

// lines splits keeping the newlines, without difflib.SplitLines' extra empty
// last line (which shows up as a blank context line).
func lines(s string) []string {
	if s == "" {
		return nil
	}
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	out := strings.SplitAfter(s, "\n")
	return out[:len(out)-1] // SplitAfter leaves "" after the final newline
}

func templateYAML(t manifests.PodTemplate) string {
	if t.Template == nil {
		return ""
	}
	var v map[string]any
	b, _ := json.Marshal(t.Template)
	_ = json.Unmarshal(b, &v)
	if meta, ok := v["metadata"].(map[string]any); ok {
		for _, field := range []string{"labels", "annotations"} {
			m, ok := meta[field].(map[string]any)
			if !ok {
				continue
			}
			for _, k := range ChartVersionKeys {
				delete(m, k)
			}
			if len(m) == 0 {
				delete(meta, field)
			}
		}
	}
	y, err := sigsyaml.Marshal(v)
	if err != nil {
		return ""
	}
	return string(y)
}

// ---- verdict ----------------------------------------------------------------------------------

func (p *printer) verdict(rep *schema.Report) {
	var safe, stop, none, skipped []string
	for _, s := range rep.AppSets {
		m := s.Summary
		switch s.Status {
		case schema.GroupOK:
			safe = append(safe, s.Name)
		case schema.GroupDiff, schema.GroupError:
			var why []string
			if m.Diff > 0 {
				why = append(why, fmt.Sprintf("%d diff", m.Diff))
			}
			if m.Error > 0 {
				why = append(why, fmt.Sprintf("%d error", m.Error))
			}
			stop = append(stop, fmt.Sprintf("%s (%s)", s.Name, strings.Join(why, ", ")))
		case schema.GroupUnknown:
			none = append(none, fmt.Sprintf("%s (%d unknown)", s.Name, m.Unknown))
		default:
			skipped = append(skipped, s.Name)
		}
	}
	p.printf("\n%s\n", p.c(bold, "Verdict"))
	if len(stop) > 0 {
		p.printf("  %s %s\n", p.c(red+";"+bold, "Do NOT bump:"), strings.Join(stop, ", "))
	}
	if len(none) > 0 {
		p.printf("  %s %s\n", p.c(magenta, "No verdict, run again:"), strings.Join(none, ", "))
	}
	if len(safe) > 0 {
		p.printf("  %s %s\n", p.c(green, "Safe to bump:"), strings.Join(safe, ", "))
	}
	if len(skipped) > 0 {
		p.printf("  %s %s\n", p.c(dim, "Not checked:"), strings.Join(skipped, ", "))
	}
	if len(rep.AppSets) == 0 {
		p.printf("  nothing to check\n")
	}
}
