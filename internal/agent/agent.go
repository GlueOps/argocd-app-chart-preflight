// Package agent runs one preflight: discover → check every app (bounded
// parallelism) → roll up per ApplicationSet → emit (ConfigMap, log lines,
// receiver). Per app: `app diff --exit-code` first (exit 0 ⇒ ok, no renders);
// only an app that differs pays for the two `app manifests` renders and the
// structural compare that yields old/new values.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"k8s.io/client-go/kubernetes"

	"github.com/glueops/argocd-app-chart-preflight/internal/argocd"
	"github.com/glueops/argocd-app-chart-preflight/internal/compare"
	"github.com/glueops/argocd-app-chart-preflight/internal/config"
	"github.com/glueops/argocd-app-chart-preflight/internal/discover"
	"github.com/glueops/argocd-app-chart-preflight/internal/kube"
	"github.com/glueops/argocd-app-chart-preflight/internal/logging"
	"github.com/glueops/argocd-app-chart-preflight/internal/manifests"
	"github.com/glueops/argocd-app-chart-preflight/internal/report"
	"github.com/glueops/argocd-app-chart-preflight/internal/schema"
	"github.com/glueops/argocd-app-chart-preflight/internal/send"
)

// Run-level statuses for the summary line that are not report statuses.
const (
	StatusInvalidConfig = "invalid_config"
	StatusInternalError = "internal_error"
)

// Deps are the agent's collaborators; tests substitute all of them.
type Deps struct {
	Argocd  argocd.Client
	Kube    kubernetes.Interface // nil ⇒ the ConfigMap write is skipped
	Sender  *send.Sender         // nil ⇒ no POST
	Version string
	Commit  string
	Now     func() time.Time
}

// Summary is the one "run summary" log line every run ends with.
type Summary struct {
	RunID     string
	Status    string
	Reason    string
	Error     string
	Report    *schema.Report
	ConfigMap string
	Ingest    string
	Duration  time.Duration
}

// LogSummary emits the summary line.
func LogSummary(log *slog.Logger, s Summary) {
	attrs := []any{"run_id", s.RunID, "status", s.Status}
	if s.Reason != "" {
		attrs = append(attrs, "reason", s.Reason)
	}
	if s.Error != "" {
		attrs = append(attrs, "error", s.Error)
	}
	if r := s.Report; r != nil {
		attrs = append(attrs, "run_status", r.Run.Status, "captain_domain", r.Run.CaptainDomain, "target", deref(r.Run.TargetRevision), "argocd_cli_version", deref(r.Run.ArgocdCLIVersion),
			"appsets", r.Summary.AppSets, "ok", r.Summary.OK, "diff", r.Summary.Diff, "error", r.Summary.Error, "unknown", r.Summary.Unknown, "skipped", r.Summary.Skipped, "apps_ignored", r.Summary.AppsIgnored)
	}
	if s.ConfigMap != "" {
		attrs = append(attrs, "configmap", s.ConfigMap)
	}
	if s.Ingest != "" {
		attrs = append(attrs, "ingest", s.Ingest)
	}
	attrs = append(attrs, "duration_ms", s.Duration.Milliseconds())
	log.Info("run summary", attrs...)
}

// NewRunID is time-sortable and unique: YYYYMMDDHHMMSS_<8 hex>.
func NewRunID(now time.Time) string {
	return fmt.Sprintf("%s_%08x", now.UTC().Format("20060102150405"), uint32(now.UnixNano()))
}

// outputTimeout bounds the ConfigMap write and the POST together. It fits inside
// the pod's 60 s termination grace period, so a SIGTERM still gets a report out.
const outputTimeout = 40 * time.Second

// Run executes one preflight and returns the exit code (always 0) and the report.
func Run(ctx context.Context, cfg config.Config, deps Deps, log *slog.Logger) (int, *schema.Report) {
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	started := now()
	runID := NewRunID(started)
	rep, err := Preflight(ctx, cfg, deps, log, runID, started)
	if err != nil {
		log.Error("preflight could not run", "reason", logging.ReasonArgocdUnavailable, "error", err.Error())
		LogSummary(log, Summary{RunID: runID, Status: StatusInternalError, Reason: logging.ReasonArgocdUnavailable, Error: err.Error(), Duration: now().Sub(started)})
		return 0, nil
	}
	for i := range rep.AppSets {
		s := &rep.AppSets[i]
		log.Info("appset", "appset", s.Name, "source_type", deref(s.SourceType), "status", s.Status, "current", deref(s.CurrentRevision), "target", deref(s.TargetRevision), "reason", deref(s.Reason),
			"apps", s.Summary.Apps, "ok", s.Summary.OK, "ok_noop_diff", s.Summary.OKNoopDiff, "diff", s.Summary.Diff, "error", s.Summary.Error, "unknown", s.Summary.Unknown, "skipped", s.Summary.Skipped)
	}
	sum := Summary{RunID: runID, Status: rep.Run.Status, Report: rep, ConfigMap: "skipped", Ingest: "skipped"}
	yamlText, yerr := report.ToYAML(rep)
	if yerr != nil {
		log.Error("report could not be rendered", "reason", logging.ReasonInternalError, "error", yerr.Error())
		sum.Status, sum.Reason, sum.Error = StatusInternalError, logging.ReasonInternalError, yerr.Error()
	} else {
		// The run's context may already be over: a report for a stopped run matters most.
		outCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), outputTimeout)
		defer cancel()
		sum.ConfigMap = writeConfigMap(outCtx, cfg, deps, log, rep, yamlText)
		sum.Ingest = post(outCtx, cfg, deps, log, yamlText)
	}
	sum.Duration = now().Sub(started)
	LogSummary(log, sum)
	return 0, rep
}

func writeConfigMap(ctx context.Context, cfg config.Config, deps Deps, log *slog.Logger, rep *schema.Report, yamlText []byte) string {
	if cfg.StatusConfigMap == "" || deps.Kube == nil {
		return "skipped"
	}
	ns := cfg.StatusNamespace
	if ns == "" {
		var err error
		if ns, err = kube.PodNamespace(cfg.ServiceAccountDir); err != nil {
			log.Warn("status configmap skipped: pod namespace unknown", "reason", logging.ReasonConfigMapWriteFailed, "error", err.Error())
			return "failed: pod namespace unknown"
		}
	}
	res, err := kube.UpsertConfigMap(ctx, deps.Kube, kube.StatusConfigMap(ns, cfg.StatusConfigMap, rep, yamlText))
	if err != nil {
		log.Warn("status configmap write failed", "reason", logging.ReasonConfigMapWriteFailed, "error", err.Error())
		return "failed: " + err.Error()
	}
	return res
}

func post(ctx context.Context, cfg config.Config, deps Deps, log *slog.Logger, yamlText []byte) string {
	if cfg.IngestURL == "" || deps.Sender == nil {
		return "skipped"
	}
	body := yamlText
	if cfg.IngestGzip {
		var err error
		if body, err = send.Gzip(yamlText); err != nil {
			return "failed: " + err.Error()
		}
	}
	res := deps.Sender.Send(ctx, body)
	if !res.OK() {
		log.Warn("receiver post failed", "reason", logging.ReasonSendFailed, "http_status", res.HTTPStatus, "attempts", res.Attempts, "error", res.Err.Error())
		return fmt.Sprintf("failed: %v", res.Err)
	}
	return fmt.Sprintf("accepted (%d)", res.HTTPStatus)
}

// Preflight is the run without the outputs: discovery, per-app checks, roll-up.
func Preflight(ctx context.Context, cfg config.Config, deps Deps, log *slog.Logger, runID string, started time.Time) (*schema.Report, error) {
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	appSets, err := deps.Argocd.ListApplicationSets(ctx)
	if err != nil {
		return nil, err
	}
	apps, err := deps.Argocd.ListApplications(ctx)
	if err != nil {
		return nil, err
	}
	cli := deps.Argocd.ClientVersion(ctx)
	disc := discover.Discover(discover.Input{ApplicationSets: appSets, Applications: apps, TargetRevision: cfg.TargetRevision, Targets: cfg.Targets})

	type work struct {
		group *discover.Group
		app   discover.App
	}
	var items []work
	results := make(map[string][]schema.App, len(disc.Groups))
	for i := range disc.Groups {
		g := &disc.Groups[i]
		results[g.Name] = []schema.App{}
		for _, a := range g.Apps {
			items = append(items, work{g, a})
		}
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(1, cfg.Concurrency))
	for _, w := range items {
		wg.Add(1)
		sem <- struct{}{}
		go func(w work) {
			defer wg.Done()
			defer func() { <-sem }()
			a := CheckApp(ctx, cfg, deps.Argocd, w.group, w.app)
			if a.Status != schema.AppOK {
				log.Info("app", "appset", w.group.Name, "app", a.Name, "namespace", a.Namespace, "status", a.Status, "sync_status", deref(a.SyncStatus), "reason", deref(a.Reason),
					"current", deref(a.CurrentRevision), "target", deref(w.group.TargetRevision), "values_files", a.ValuesFiles, "findings", report.Findings(&a), "other_changes", len(a.OtherChanges))
			}
			mu.Lock()
			results[w.group.Name] = append(results[w.group.Name], a)
			mu.Unlock()
		}(w)
	}
	wg.Wait()

	sets := make([]schema.AppSet, 0, len(disc.Groups))
	for _, g := range disc.Groups {
		list := results[g.Name]
		sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
		status := report.GroupStatus(list)
		var reason *string
		if g.SkipReason != "" {
			status, reason = schema.GroupSkipped, schema.Str(g.SkipReason)
		} else if g.UnknownReason != "" {
			reason = schema.Str(g.UnknownReason)
		}
		sets = append(sets, schema.AppSet{Name: g.Name, SourceType: g.SourceType, CurrentRevision: g.CurrentRevision, TargetRevision: g.TargetRevision, Status: status, Reason: reason, Summary: report.SummarizeApps(list), Apps: list})
	}
	rep := &schema.Report{
		SchemaVersion: schema.SchemaVersion,
		Run: schema.Run{ID: runID, CaptainDomain: cfg.CaptainDomain, TargetRevision: nilIfEmpty(cfg.TargetRevision), ArgocdCLIVersion: nilIfEmpty(cli), AgentVersion: deps.Version, AgentCommit: deps.Commit,
			StartedAt: started.UTC().Format(logging.TimeLayout), FinishedAt: now().UTC().Format(logging.TimeLayout), Status: report.RunStatus(sets)},
		Summary: report.SummarizeSets(sets, disc.Ignored),
		AppSets: sets,
	}
	return rep, nil
}

// CheckApp produces one app's verdict.
func CheckApp(ctx context.Context, cfg config.Config, cli argocd.Client, g *discover.Group, app discover.App) schema.App {
	r := schema.NewApp(app.Name, app.Namespace)
	r.SyncStatus = app.SyncStatus
	if app.Chart == nil {
		return withStatus(r, schema.AppSkipped, "source is not the app chart")
	}
	r.CurrentRevision = schema.Str(app.Chart.Revision)
	r.ValuesFiles = append([]string{}, app.Chart.ValuesFiles...)
	target := deref(g.TargetRevision)
	if g.SkipReason != "" || target == "" {
		reason := g.SkipReason
		if reason == "" {
			reason = "no target revision configured"
		}
		return withStatus(r, schema.AppSkipped, reason)
	}
	if g.UnknownReason != "" {
		return withStatus(r, schema.AppUnknown, g.UnknownReason)
	}
	// standalone mixes source types (tenant git-sourced apps next to platform apps
	// from helm.gpkg.io): a target of the wrong kind for *this* app is skipped with
	// the expected form, not unknown — one TARGET_REVISION cannot fit both, and a
	// platform app's bump is a platform-chart release anyway.
	if g.SourceType == nil {
		if bad := discover.ValidateTarget(app.Chart.Type, target); bad != "" {
			return withStatus(r, schema.AppSkipped, bad)
		}
	}
	if g.CurrentRevision != nil && app.Chart.Revision != *g.CurrentRevision {
		return withStatus(r, schema.AppUnknown, fmt.Sprintf("revision drift from ApplicationSet: app pins %q, template pins %q", app.Chart.Revision, *g.CurrentRevision))
	}

	// Two renders, no `app diff` first: a chart version bump always changes the
	// helm.sh/chart label, so a diff against live never comes back clean and
	// would only add a third call to every app.
	oldR := cli.Manifests(ctx, app.Name, nil)
	newR := cli.Manifests(ctx, app.Name, &argocd.Override{Ref: app.Chart.Ref, Revision: target})
	if ctx.Err() != nil && (oldR.Code != 0 || newR.Code != 0) {
		return withStatus(r, schema.AppUnknown, stoppedReason(ctx))
	}
	if newR.Code != 0 && !newR.TimedOut && argocd.ChartRenderErrorRE.Match(newR.Stderr) {
		return withStatus(r, schema.AppError, argocd.ChartErrorReason(newR.Stderr))
	}
	for _, side := range []struct {
		name string
		res  argocd.Result
	}{{"current", oldR}, {"target", newR}} {
		if side.res.TimedOut {
			return withStatus(r, schema.AppUnknown, fmt.Sprintf("argocd app manifests (%s) timed out", side.name))
		}
		if side.res.Code != 0 {
			return withStatus(r, schema.AppUnknown, fmt.Sprintf("argocd app manifests (%s) failed: %s", side.name, argocd.StderrReason(side.res.Stderr)))
		}
	}
	oldT, err := manifests.ParseAndReduce(oldR.Stdout)
	if err != nil {
		return withStatus(r, schema.AppUnknown, "could not parse rendered manifests: "+err.Error())
	}
	newT, err := manifests.ParseAndReduce(newR.Stdout)
	if err != nil {
		return withStatus(r, schema.AppUnknown, "could not parse rendered manifests: "+err.Error())
	}
	c := compare.Compare(oldT, newT)
	r.Diff = c.Diff
	r.OtherChanges = c.OtherChanges
	switch c.Impact {
	case "":
		r.Status = schema.AppOK
	case schema.ImpactNone:
		r.Status = schema.AppOKNoopDiff
	default:
		r.Status = schema.AppDiff
		if cfg.ArgocdDiffText {
			if d := cli.Diff(ctx, app.Name, app.Chart.Ref, target); d.Code == 1 {
				r.ArgocdDiff = PodTemplateDiffText(string(d.Stdout), newT, 64*1024)
			}
		}
	}
	return r
}

// stoppedReason says why an app has no verdict, so a partial report reads as
// partial: the gate stays closed and nobody mistakes it for a render failure.
func stoppedReason(ctx context.Context) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "not checked: the run reached its time limit"
	}
	return "not checked: the run was stopped before this app finished"
}

var diffHeaderRE = regexp.MustCompile(`^=====\s+\S*/(\S+)\s+\S+\s+=====`)

// PodTemplateDiffText trims Argo's text diff to the resources that carry a
// pod template, capped so the ConfigMap stays small.
func PodTemplateDiffText(diffOut string, templates []manifests.PodTemplate, capBytes int) *string {
	kinds := map[string]bool{}
	for _, t := range templates {
		kinds[t.Kind] = true
	}
	// Blocks start at a "===== group/Kind ns/name =====" header line (RE2 has no lookahead, so split by hand).
	var blocks []string
	for _, line := range strings.Split(diffOut, "\n") {
		if strings.HasPrefix(line, "=====") || len(blocks) == 0 {
			blocks = append(blocks, line)
			continue
		}
		blocks[len(blocks)-1] += "\n" + line
	}
	var kept []string
	for _, block := range blocks {
		m := diffHeaderRE.FindStringSubmatch(strings.TrimSpace(block))
		if m != nil && kinds[m[1]] {
			kept = append(kept, strings.TrimSpace(block))
		}
	}
	if len(kept) == 0 {
		return nil
	}
	text := strings.Join(kept, "\n")
	if len(text) > capBytes {
		text = text[:capBytes] + "\n…truncated"
	}
	return &text
}

func withStatus(a schema.App, status, reason string) schema.App {
	a.Status = status
	a.Reason = schema.Str(reason)
	return a
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return schema.Str(s)
}
