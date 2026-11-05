// Package argocd is the only way this agent talks to Argo CD: the argocd CLI in
// core mode, exec'd directly (no shell). Core mode runs the API server
// in-process with the pod's ServiceAccount and hands renders to
// argocd-repo-server — so the tenant repo credentials in play are Argo CD's
// own, never ours. Subcommands used: appset list, app list, app diff,
// app manifests, version. All read-only.
package argocd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Result of one CLI invocation.
type Result struct {
	Code     int // exit code; -1 when the process did not run or was killed
	Stdout   []byte
	Stderr   []byte
	TimedOut bool
}

// Runner executes the CLI. Tests substitute a scripted one.
type Runner interface {
	Run(ctx context.Context, args ...string) Result
}

// ExecRunner runs a real binary with a per-call timeout.
type ExecRunner struct {
	Bin        string
	Kubeconfig string
	Home       string // the CLI writes ~/.config/argocd; an emptyDir in the pod
	Timeout    time.Duration
}

// Run executes Bin with args. Stdout/stderr are captured in full (a render of a
// large app is a few hundred KiB at most).
func (r ExecRunner) Run(ctx context.Context, args ...string) Result {
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.Bin, args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + r.Home, "KUBECONFIG=" + r.Kubeconfig}
	cmd.Stdin = nil
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	res := Result{Code: 0, Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		res.TimedOut = true
		res.Code = -1
		return res
	}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		res.Code = ee.ExitCode()
	default:
		// Could not start (binary missing, not executable): surface it as stderr.
		res.Code = -1
		res.Stderr = append(res.Stderr, []byte(err.Error())...)
	}
	return res
}

// StderrReason distils the CLI's stderr to one line: the last JSON log line's
// msg, else the plain tail, capped at 1000 bytes.
func StderrReason(stderr []byte) string {
	lines := strings.Split(strings.TrimSpace(string(stderr)), "\n")
	var plain []string
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" {
			continue
		}
		if strings.HasPrefix(l, "{") {
			var o struct {
				Msg string `json:"msg"`
			}
			if json.Unmarshal([]byte(l), &o) == nil && o.Msg != "" {
				return cap1000(o.Msg)
			}
		}
	}
	for _, l := range lines {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "{") {
			plain = append(plain, l)
		}
	}
	if len(plain) > 3 {
		plain = plain[len(plain)-3:]
	}
	if len(plain) == 0 && len(lines) > 0 {
		return cap1000(strings.TrimSpace(lines[len(lines)-1]))
	}
	return cap1000(strings.Join(plain, " | "))
}

func cap1000(s string) string {
	if len(s) > 1000 {
		return s[:1000]
	}
	return s
}
