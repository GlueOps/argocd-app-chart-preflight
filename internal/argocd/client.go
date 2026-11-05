package argocd

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/glueops/argocd-app-chart-preflight/internal/discover"
)

// Client wraps the CLI subcommands the agent uses.
type Client struct {
	Runner Runner
}

// ChartRenderErrorRE matches the app chart's own failure text for a null/map
// item (the target would be a ComparisonError after the bump); anything else
// that fails is unknown.
var ChartRenderErrorRE = regexp.MustCompile(`every item must be a string|must be a list of strings|got (map|null|list|slice)\b`)

// ClientVersion returns the embedded CLI version ("" when unavailable).
func (c Client) ClientVersion(ctx context.Context) string {
	r := c.Runner.Run(ctx, "version", "--client", "-o", "json")
	if r.Code != 0 {
		return ""
	}
	var v struct {
		Client struct {
			Version string `json:"Version"`
		} `json:"client"`
	}
	if json.Unmarshal(r.Stdout, &v) != nil {
		return ""
	}
	return v.Client.Version
}

// ListApplicationSets runs `appset list --core -o json`.
func (c Client) ListApplicationSets(ctx context.Context) ([]map[string]any, error) {
	return c.listJSON(ctx, "appset", "list", "--core", "-o", "json")
}

// ListApplications runs `app list --core -o json`.
func (c Client) ListApplications(ctx context.Context) ([]map[string]any, error) {
	return c.listJSON(ctx, "app", "list", "--core", "-o", "json")
}

func (c Client) listJSON(ctx context.Context, args ...string) ([]map[string]any, error) {
	r := c.Runner.Run(ctx, args...)
	what := strings.Join(args[:2], " ")
	if r.TimedOut {
		return nil, fmt.Errorf("argocd %s timed out", what)
	}
	if r.Code != 0 {
		return nil, fmt.Errorf("argocd %s failed: %s", what, StderrReason(r.Stderr))
	}
	if strings.TrimSpace(string(r.Stdout)) == "" || strings.TrimSpace(string(r.Stdout)) == "null" {
		return nil, nil
	}
	var out []map[string]any
	if err := json.Unmarshal(r.Stdout, &out); err != nil {
		return nil, fmt.Errorf("argocd %s: expected a JSON array: %w", what, err)
	}
	return out, nil
}

// Diff runs `app diff --exit-code` with the chart source overridden: exit 0 =
// no diff vs live, 1 = diff, anything else = the CLI failed.
func (c Client) Diff(ctx context.Context, app string, ref discover.SourceRef, revision string) Result {
	args := append([]string{"app", "diff", app, "--core"}, ref.RevisionArgs(revision)...)
	return c.Runner.Run(ctx, append(args, "--exit-code")...)
}

// Manifests renders the app at its pinned revision, or at revision for the
// chart source when override is non-nil.
func (c Client) Manifests(ctx context.Context, app string, override *Override) Result {
	args := []string{"app", "manifests", app, "--core"}
	if override != nil {
		args = append(args, override.Ref.RevisionArgs(override.Revision)...)
	}
	return c.Runner.Run(ctx, args...)
}

// Override names a chart-source revision to render at.
type Override struct {
	Ref      discover.SourceRef
	Revision string
}
