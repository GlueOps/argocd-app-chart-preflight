# argocd-app-chart-preflight

A read-only pre-bump gate for the GlueOps [`app` Helm chart](https://github.com/GlueOps/project-template-helm-chart-app).
Before anyone bumps an ApplicationSet's chart `targetRevision`, it answers: *"what would change if
this environment's tenant apps rendered at chart version X?"*, per ApplicationSet, per app, down to
the exact old/new `command`/`args` item. It never reads a tenant repository itself: the cluster's own
Argo CD renders both versions with its own repository credentials (see [AUTH.md](./AUTH.md)).

One binary, two ways to run it:

- **Operator mode (how chart bumps are done today).** An operator runs `app-chart-preflight` from the
  captain codespace right before bumping, and reads a table in the terminal. It uses their kube
  context, writes nothing to the cluster and sends nothing. See [Operator use](#operator-use).
- **CronJob.** Deployed on every captain by
  [platform-helm-chart-platform](https://github.com/GlueOps/platform-helm-chart-platform) (parked
  for now), it reports to a ConfigMap and the fleet receiver:

```
captain (CronJob, daily while a target is set)                       glueops-prod                      lake
argocd admin dashboard on 127.0.0.1, once per run                    glueops-inventory-receiver
  appset list · list apps                                              POST /v1/preflight (gzipped YAML) ──▶ preflight_runs / preflight_apps / preflight_findings
  per app: app manifests ×2 → compare → classify                                                            → ClickHouse views → Grafana
  ConfigMap app-chart-preflight-status (YAML) · JSON log lines (Loki)
```

Built for the 0.18.0 breaking release of the chart
([#147](https://github.com/GlueOps/project-template-helm-chart-app/pull/147)); the diff machinery is
generic and only `internal/classify` knows that release's table.

## Operator use

From the captain repo in the captain codespace, with the kube context of that captain:

```bash
app-chart-preflight --target 0.18.0                                    # every ApplicationSet that uses the chart
app-chart-preflight --target v0.18.0 --appset nonprod-application-set  # just that set
app-chart-preflight --appset prod=0.18.0 --appset previews=v0.18.0      # a target per set
app-chart-preflight --target 0.18.0 --save report.yaml                 # also keep the full YAML report
```

The target's form follows how the ApplicationSet sources the chart: `0.18.0` for helm.gpkg.io or
OCI, `v0.18.0` (a tag) or a branch for the chart's git repository. A target of the wrong kind is
reported as no verdict, with the expected form. `--appset standalone` selects the app-chart
Applications that no ApplicationSet generated.

| Flag | Default | |
|---|---|---|
| `--target REV` | | Chart revision for every selected set. |
| `--appset NAME[=REV]` | every set | Check only this set; `=REV` also sets its target. Repeatable. |
| `--context` | current | Kube context. |
| `--argocd-namespace` | `glueops-core` | Where Argo CD runs. |
| `--argocd-bin` | `argocd` | The Argo CD CLI. |
| `--concurrency` | `4` | Apps checked in parallel. |
| `--timeout` | `60m` | Stop starting checks after this; unfinished apps are reported as not checked. |
| `--diff` / `--no-diff` | on | The pod-template diff of every app that changes. |
| `--all` | off | Also list clean and skipped apps one by one. |
| `--wide` | off | Do not truncate values, hints and reasons. |
| `--no-color` | | Colour is also off when stdout is not a terminal, or `NO_COLOR` is set. |
| `--save FILE` | | Also write the full report as YAML. |
| `--format text\|yaml` | `text` | What goes to stdout. |

**Exit code:** `0` every checked set is `ok`, so it is safe to bump; `1` a set has a `diff` or an
`error`; `2` a set has no verdict, nothing was checked, a named set does not exist, or the run
could not start.

Output from venus nonprod, with test apps injected into `standalone` (trimmed):

```
App chart preflight  nonprod.venus.onglueops.rocks  context k3d-captain  argocd v3.4.12+590e562
Target per ApplicationSet · 5 ApplicationSets · 34 apps checked in 9s

APPLICATIONSET           SOURCE  CURRENT      TARGET                      STATUS  APPS  OK  DIFF  ERROR  UNKNOWN  SKIPPED
nonprod-application-set  git     v0.12.0-rc1  fix/container-args-quoting  ok        14  14     0      0        0        0
pf-helm-set              helm    0.16.1       0.17.1                      ok         2   2     0      0        0        0
standalone               -       -            fix/container-args-quoting  error     14   0     2      1        0       11

standalone — ERROR: the target does not render for these apps; fix the values first

  Changes (command/args items, current chart -> target)
  APP               SYNC       RESOURCE                     CONTAINER         FIELD    IMPACT  OLD          NEW              HINT
  pf-inject-padded  OutOfSync  Deployment/pf-inject-padded  pf-inject-padded  args[0]  roll    "padded"     "  padded  "     leading/trailing spaces are preserved from the target revis…
  pf-inject-quoted  OutOfSync  Deployment/pf-inject-quoted  pf-inject-quoted  args[0]  break   "echo a: b"  "\"echo a: b\""  the item is wrapped in a second pair of quotes; 0.17.x stri…

  Render errors
  APP            NAMESPACE  SYNC       REASON
  pf-inject-map  nonprod    OutOfSync  deployment.args: every item must be a string (got map) - quote the item in your values file; null, map and li…

  11 skipped: target "fix/container-args-quoting" is not a chart version; a helm-sourced chart needs X.Y.Z

Pod template diffs (current chart -> target, chart-version labels left out)

standalone / pf-inject-quoted
--- Deployment/pf-inject-quoted @ v0.17.1
+++ Deployment/pf-inject-quoted @ fix/container-args-quoting
@@ -6,7 +6,7 @@
 spec:
   containers:
   - args:
-    - 'echo a: b'
+    - '"echo a: b"'
     command:

Verdict
  Do NOT bump: standalone (2 diff, 1 error)
  Safe to bump: nonprod-application-set, pf-helm-set
```

Values are quoted so quotes, spaces and empty strings show. `IMPACT` is `break` (the pod gets an
argument it did not get before and the command fails), `roll` (the pods restart with a changed
argument) or `none`. The diff is built from the two renders the check already made; labels and
annotations that carry the chart version are left out, because they change on every bump. Progress
and errors go to stderr.

How it runs: the operator's kubeconfig is copied to a temporary directory, reduced to the one
context, with its namespace set to Argo CD's. Exec auth plugins are kept. The same local Argo CD
server as the CronJob is started with it, and both are removed when the command ends.

## What a run does

1. **Discover** — `argocd appset list`: every ApplicationSet whose template sources the `app` chart,
   in any of its three published forms (helm.gpkg.io · OCI `ghcr.io/glueops/project-template-helm-chart-app` ·
   the chart's git repo). Its `targetRevision` is the set's `current_revision`.
   Applications are listed from the Kubernetes API in pages, keeping only the fields used: apps are attributed to their set (label `argocd.argoproj.io/application-set-name`,
   else ownerReferences); app-chart apps nobody generated form the `standalone` group.
2. **Per app**, `CONCURRENCY` at a time: render current and target (`argocd app manifests`), reduce
   both to pod templates, compare `command`/`args` item by item per container (findings), report
   every other pod-template difference as paths only (`other_changes`), classify each finding.
   There is no `app diff` first: a chart bump always changes the `helm.sh/chart` label, so a diff
   against live never comes back clean.
3. **Statuses** (ladder, worst wins): app `ok` · `ok-noop-diff` (only `null ↔ ""`) · `diff` ·
   `error` (target fails to render: a `null`/map item — `ComparisonError` after the bump) ·
   `unknown` (CLI failure, timeout, revision drift from the set's template, wrong kind of target) ·
   `skipped`. Set and run: `ok | diff | error | unknown | skipped`, `error > diff > unknown > ok`.
   **The gate is `appsets[<name>].status == ok`**; `unknown` is never a verdict.
4. **Emit** — ConfigMap `app-chart-preflight-status` in the pod's namespace (`data.status`,
   `data.target_revision`, `data.finished_at`, `data.schema_version`, `data.report_truncated`,
   `data["report.yaml"]`); one
   JSON log line per set and per non-ok app plus a `run summary` line (all
   `component=argocd-app-chart-preflight`); the YAML report gzipped to the receiver.
   **Exit 0 no matter what** once running — a verdict is not a Job failure, nor is an unreachable
   receiver (inventory-agent's contract). Only failures before the agent starts (image pull, OOM,
   `activeDeadlineSeconds`) fail the Job.
5. **Stopping early** — at `RUN_TIMEOUT_MS`, or on SIGTERM (node drain, eviction), the run starts no
   new work. Finished apps keep their verdict, the rest are `unknown` with a "not checked" reason,
   and the report still goes out. A partial run is never `ok`, so the gate stays closed.

### Finding categories (`internal/classify`)

| category | rendered (old → new) | impact |
|---|---|---|
| `double_quoted` | `echo a: b` → `"echo a: b"` | **break** — the 0.17.x `: ` workaround; remove the outer quotes first |
| `comment_truncated` | `--note` → `--note #keep` | roll |
| `leading_hash` | `""` → `#x` | roll |
| `padded` | `x` → `  x  ` | roll |
| `retyped` | `8080` (number) → `"8080"`, `true` → `"yes"`, `map[a:b]` | roll |
| `null_item` | `null` ↔ `""` | none → app is `ok-noop-diff` |
| `length_changed` / `other` | — | roll |

## Report

`schema/report.schema.json` (closed at every level; `internal/schema` tests keep it equal to the Go
types). YAML in the ConfigMap and on the wire — every string double-quoted so a value that *is*
quotes, spaces or `#` reads unambiguously, `null` bare — JSON in log lines.

```yaml
schema_version: 1
run: {id, captain_domain, target_revision, argocd_cli_version, agent_version, agent_commit, started_at, finished_at, status}
summary: {appsets, ok, diff, error, unknown, skipped, apps_ignored}
appsets:
  - name: "nonprod-application-set"
    source_type: "git"            # helm | oci | git | null (standalone)
    current_revision: "v0.17.1"   # the set's template pin — what you will bump
    target_revision: "v0.18.0"
    status: "diff"
    reason: null
    summary: {apps, ok, ok-noop-diff, diff, error, unknown, skipped}
    apps:
      - name: "keda-demo-newrelic-nonprod"
        namespace: "nonprod"      # destination namespace
        status: "diff"
        sync_status: "Synced"
        reason: null
        values_files: ["common/common-values.yaml", "…", "apps/keda-demo-newrelic/envs/nonprod/values.yaml"]
        diff:
          Deployment/keda-demo-newrelic:
            containers:
              keda-demo-newrelic:
                args:
                  "0": {old: "set -e && …", new: "\"set -e && …\"", category: "double_quoted", impact: "break", hint: "…"}
        other_changes: {Deployment/keda-demo-newrelic: ["spec.template.metadata.labels.helm.sh/chart"]}
        argocd_diff: null         # Argo's text diff for pod-template resources when ARGOCD_DIFF_TEXT=true
```

## Configuration

All configuration is via environment variables (the platform chart renders them from
`app_chart_preflight.*` in a captain's `platform.yaml`).

| Variable | Default | Description |
|---|---|---|
| `CAPTAIN_DOMAIN` | (required) | Cluster identity; keys every row and log line. A DNS name; rejected if it contains `placeholder`. |
| `TARGET_REVISION` | `""` | Revision to check every set against: `0.18.0` for helm/OCI sources, `v0.18.0` or a branch for git. Empty ⇒ every set `skipped`. |
| `TARGETS_JSON` | `{}` | Per-set override `{"<set>": "<rev>"}`; `""` skips that set. |
| `CONCURRENCY` | `2` | Parallel per-app checks. The pod is CPU bound, so more mostly adds peak memory. |
| `RUN_TIMEOUT_MS` | `840000` | When the run stops starting work and reports what it has. Keep it below `activeDeadlineSeconds`. |
| `ARGOCD_NAMESPACE` | `glueops-core` | Argo CD's namespace (the kubeconfig context the agent writes). |
| `ARGOCD_BIN` | `/argocd` | The CLI (shipped in the image). |
| `ARGOCD_TIMEOUT_MS` | `120000` | Per CLI call. |
| `ARGOCD_DIFF_TEXT` | `false` | Attach Argo's text diff to `diff` apps (≤ 64 KiB). |
| `INGEST_URL` | `https://inventory-agent.observability.glueopshosted.com/v1/preflight` | Unset ⇒ default; **set but empty ⇒ no POST**; must be `https://`. |
| `INGEST_TIMEOUT_MS` | `30000` | POST timeout. |
| `STATUS_CONFIGMAP` | `app-chart-preflight-status` | Empty ⇒ skip the write. Written only in-cluster. |
| `STATUS_NAMESPACE` | pod namespace | Override for the ConfigMap's namespace. |
| `KUBECONFIG` | — | Dev only: use this kubeconfig as-is (its context namespace must be Argo CD's); the ConfigMap write is skipped. |
| `KUBECONFIG_DIR` | `/tmp` | Where the in-cluster kubeconfig and the CLI's HOME are written (an emptyDir). |
| `SERVICE_ACCOUNT_DIR` | `/var/run/secrets/kubernetes.io/serviceaccount` | The ServiceAccount mount. |
| `DEV_MODE` | `false` | Print the YAML report to stdout after the summary line. |
| `LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error`. |

## Load and limits

Measured on venus with the real image in a pod at the chart's limits (500m CPU, 512Mi memory),
concurrency 2, and throwaway Applications padded to real object sizes. Two full runs:

| Apps rendered | Run time | Process memory peak | Failed checks |
|---|---|---|---|
| 14 | 10 s | not sampled | 0 |
| 314 | 3.5 min | not sampled | 0 |
| 717 | 7.2 min | 317 MiB | 0 |

That is about 0.6 s per app, so the default 14-minute `RUN_TIMEOUT_MS` covers about 1,400 apps.
Memory is only proven up to 717 apps. It rises through a run, so re-measure before relying on a
larger captain. With no target set, nothing is rendered and a run costs almost nothing.

Three design choices keep it this small, and each has a measured reason:

- **One local API server per run.** `argocd --core` in every call cost about 42 API requests and
  0.6 CPU-seconds a call, and listed every Application each time.
- **Applications come from the Kubernetes API in pages.** `argocd app list -o json` OOM-killed a
  512Mi pod 10 seconds into a 743-app run.
- **Two renders, no `app diff`.** A chart bump always changes the `helm.sh/chart` label, so the diff
  never came back clean (0 of 112 checks).

If the repo-server restarts mid-run, the agent restarts its local server and retries. That was
tested on venus with no app lost.

## Run the CronJob path from a workstation (development)

Operator mode is the way to run it by hand. To exercise the job's own path (ConfigMap and POST
turned off, report printed), set its environment instead of flags:

```bash
kubectl config view --raw > /tmp/kc-captain && KUBECONFIG=/tmp/kc-captain kubectl config set-context --current --namespace=glueops-core
go build -o bin/preflight . && KUBECONFIG=/tmp/kc-captain ARGOCD_BIN=$(which argocd) \
  CAPTAIN_DOMAIN=nonprod.venus.onglueops.rocks TARGET_REVISION=v0.18.0 INGEST_URL= STATUS_CONFIGMAP= DEV_MODE=true ./bin/preflight
```

Without flags the binary runs as the job when `CAPTAIN_DOMAIN` is set or a ServiceAccount token is
mounted; otherwise it prints the operator usage.

## Development

```bash
go test -race ./...     # everything runs against a scripted argocd, a fake clientset and httptest — no cluster, no network
go vet ./... && gofmt -l .
docker build -t argocd-app-chart-preflight:dev .
```

`testdata/` holds renders of `keda-demo-newrelic-nonprod` recorded from venus nonprod Argo CD at
`v0.12.0-rc1`, `v0.17.1` and the PR #147 branch, plus a recorded `argocd app diff`; the compare
tests run on them. The report schema is closed: adding a field means editing `internal/schema`,
`schema/report.schema.json` and bumping `schema_version` together (the tests enforce the pairing).

## Layout

```
main.go              entrypoint: picks the mode; the CronJob path (config → kubeconfig → run → outputs, always exits 0)
operator.go          operator mode: flags → one-context kubeconfig → preflight → text report → exit code
internal/cli         operator flags, exit codes, which mode to run
internal/textreport  the operator's tables, values, pod-template diff and verdict
internal/agent       one run: discover → per-app check (semaphore) → per-set roll-up → ConfigMap/POST/summary
internal/argocd      the CLI runner (exec, timeout), local server client, in-cluster and operator kubeconfigs
internal/manifests   rendered YAML stream → pod templates (YAML 1.1 types preserved)
internal/discover    ApplicationSets/apps → groups; the three chart-source forms; target validation
internal/compare     pod-template compare: command/args findings + other_changes paths
internal/classify    the 0.18.0 category/impact/hint table (the only release-specific package)
internal/schema      the closed report types (+ schema sync tests)
internal/report      status ladder, summaries, ordered double-quoted YAML, findings for log lines
internal/kube        clientset + status ConfigMap upsert
internal/send        gzipped YAML POST to the receiver (bounded retries, never fails the run)
internal/config      environment contract
internal/logging     JSON log lines with reason codes
schema/              report.schema.json
testdata/            recorded venus renders
```
