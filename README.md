# argocd-app-chart-preflight

A read-only pre-bump gate for the GlueOps [`app` Helm chart](https://github.com/GlueOps/project-template-helm-chart-app),
run **inside each GlueOps captain** as a CronJob (deployed by
[platform-helm-chart-platform](https://github.com/GlueOps/platform-helm-chart-platform), like
`inventory-agent`). Before anyone bumps an ApplicationSet's chart `targetRevision`, it answers:
*"what would change if this environment's tenant apps rendered at chart version X?"* — per
ApplicationSet, per app, down to the exact old/new `command`/`args` item. It never reads a tenant
repository itself: the cluster's own Argo CD renders both versions with its own credentials
(see [AUTH.md](./AUTH.md)).

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

## Run it from a workstation (against a captain you can reach with kubectl)

Copy your kubeconfig, set its context namespace to Argo CD's, point the agent at it and the `argocd`
CLI on your PATH, keep the report local:

```bash
kubectl config view --raw > /tmp/kc-captain && KUBECONFIG=/tmp/kc-captain kubectl config set-context --current --namespace=glueops-core
go build -o bin/preflight . && KUBECONFIG=/tmp/kc-captain ARGOCD_BIN=$(which argocd) \
  CAPTAIN_DOMAIN=nonprod.venus.onglueops.rocks TARGET_REVISION=v0.18.0 INGEST_URL= STATUS_CONFIGMAP= DEV_MODE=true ./bin/preflight
```

In production nothing is run by hand: set `target_revision` in the captain's `platform.yaml`, let
the CronJob run (or `kubectl -n glueops-core-argocd-app-chart-preflight create job --from=cronjob/argocd-app-chart-preflight-check preflight-now`),
read the verdict (`kubectl -n glueops-core-argocd-app-chart-preflight get cm app-chart-preflight-status -o jsonpath='{.data.status}'`,
Loki, or the fleet dashboard), bump the ApplicationSet only when the set is `ok`.

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
main.go              entrypoint: config → kubeconfig → run → outputs; always exits 0 once running
internal/agent       one run: discover → per-app check (semaphore) → per-set roll-up → ConfigMap/POST/summary
internal/argocd      the CLI runner (exec, timeout), core-mode client, in-cluster kubeconfig
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
