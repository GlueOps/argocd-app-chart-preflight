package argocd

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/glueops/argocd-app-chart-preflight/internal/discover"
)

// Client wraps the CLI subcommands the agent uses.
type Client struct {
	Runner Runner
	// Conn is the connection flags appended to every call that reaches Argo CD.
	Conn []string
	// Recovery restarts the local API server when a call loses it; nil retries in place.
	Recovery Recovery
	// Retries is how many extra attempts a transient failure gets (default 2).
	Retries int
	// Backoff is the wait before a retry, times the attempt number.
	Backoff time.Duration
}

// Recovery is the part of Server the client needs.
type Recovery interface {
	Generation() uint64
	Recover(ctx context.Context, seen uint64) error
}

// TransientRE matches a lost connection, not a failed render: the local API
// server died, or the repo-server/redis pod behind its port-forward went away.
// Those are retried after a restart; a chart error never is.
var TransientRE = regexp.MustCompile(`bind: address already in use|dial tcp 127\.0\.0\.1:[0-9]+: connect: connection refused|error upgrading connection|port forward|code = Unavailable|lost connection to pod|error reading from server|connection reset by peer`)

// run executes args against Argo CD, retrying transient failures.
func (c Client) run(ctx context.Context, args ...string) Result {
	retries := c.Retries
	if retries == 0 {
		retries = 2
	}
	args = append(args[:len(args):len(args)], c.Conn...)
	var r Result
	for attempt := 0; ; attempt++ {
		var seen uint64
		if c.Recovery != nil {
			seen = c.Recovery.Generation()
		}
		r = c.Runner.Run(ctx, args...)
		if r.Code == 0 || r.TimedOut || !TransientRE.Match(r.Stderr) || ctx.Err() != nil || attempt == retries {
			return r
		}
		if args[0] == "app" && args[1] == "diff" && r.Code == 1 {
			return r // exit 1 is "there is a diff", never transient
		}
		select {
		case <-ctx.Done():
			return r
		case <-time.After(time.Duration(attempt+1) * c.Backoff):
		}
		if c.Recovery != nil {
			if err := c.Recovery.Recover(ctx, seen); err != nil {
				r.Stderr = append(r.Stderr, []byte("\n"+err.Error())...)
				return r
			}
		}
	}
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

// ListApplicationSets runs `appset list -o json`.
func (c Client) ListApplicationSets(ctx context.Context) ([]map[string]any, error) {
	return c.listJSON(ctx, "appset", "list", "-o", "json")
}

func (c Client) listJSON(ctx context.Context, args ...string) ([]map[string]any, error) {
	r := c.run(ctx, args...)
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
	args := append([]string{"app", "diff", app}, ref.RevisionArgs(revision)...)
	return c.run(ctx, append(args, "--exit-code")...)
}

// Manifests renders the app at its pinned revision, or at revision for the
// chart source when override is non-nil.
func (c Client) Manifests(ctx context.Context, app string, override *Override) Result {
	args := []string{"app", "manifests", app}
	if override != nil {
		args = append(args, override.Ref.RevisionArgs(override.Revision)...)
	}
	return c.run(ctx, args...)
}

// Override names a chart-source revision to render at.
type Override struct {
	Ref      discover.SourceRef
	Revision string
}
