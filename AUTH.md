# Access & minimal permissions (read-only)

This agent runs **inside a GlueOps captain** and talks to that cluster's Argo CD only. It holds no
credential of its own: no Argo CD token, no GitHub token, no lake key. Every tenant-repo access it
causes is performed by Argo CD with **Argo CD's own repository credentials**, exactly as a sync does.

## How it reaches Argo CD — core mode

The agent exec's the `argocd` CLI (copied into the image from the Argo CD release) with `--core`.
Core mode runs Argo CD's API server in-process, authenticated as the pod's **Kubernetes
ServiceAccount**, and hands renders to `argocd-repo-server`. The only subcommands invoked:

| command | why |
|---|---|
| `argocd appset list --core -o json` | find the ApplicationSets that template the `app` chart and their pinned revision |
| `argocd app list --core -o json` | attribute Applications to their set; read sync status and values-file list |
| `argocd app diff <app> --core --revisions <target> --source-positions <i> --exit-code` | fast path: exit 0 ⇒ nothing changes |
| `argocd app manifests <app> --core` / `… --revisions <target> …` | the two renders compared for old/new values |
| `argocd version --client` | recorded in the report |

Core mode bypasses Argo CD's own RBAC (it *is* the server), so the boundary is Kubernetes RBAC.

## Kubernetes RBAC (granted by the platform chart template; verified on venus nonprod, 2026-11-04)

**Role in Argo CD's namespace (`glueops-core`)**, bound to the agent's ServiceAccount:

| resource | verbs | why |
|---|---|---|
| `applications`, `applicationsets`, `appprojects` (argoproj.io) | get, list | discovery and the Application specs the renders are built from |
| `secrets`, `configmaps` | get, list | core mode reads `argocd-cm`/`argocd-rbac-cm` and the repository-credential Secrets to brief the repo-server |
| `services`, `pods` | get, list | the headless client discovers `argocd-repo-server` / redis by Service and pod |
| `pods/portforward` | create | the headless client port-forwards to them (even from inside the cluster) |

The `secrets` grant is every Secret in `glueops-core` — RBAC cannot scope by type. It is accepted for
the same reason inventory-agent's Helm-metadata Role is: a platform-owned namespace, and **this
agent's own code never reads a Secret**; the embedded Argo CD code does, to do what `argocd-server`
already does with the same Secrets. It is the read half of `argocd-server`'s own RBAC, no write verbs.

**Role in the agent's own namespace** (`glueops-core-argocd-app-chart-preflight`): `configmaps`
get/create/update on the status ConfigMap only. That is the agent's only Kubernetes write.

No ClusterRole. Nothing cluster-scoped.

## What leaves the cluster

One gzipped YAML report per run, POSTed to the inventory receiver (`INGEST_URL`, default
`https://inventory-agent.observability.glueopshosted.com/v1/preflight`): per ApplicationSet and per
app, the verdict, and for apps whose pod template changes, the **rendered** `command`/`args` items
that differ (old and new value). Tenant values files, Secrets, env values and other pod-spec fields
are never collected. Set `INGEST_URL` to an empty string to keep everything in-cluster (ConfigMap +
log lines).

The receiver has no authentication in v1 (hostname-only, as for inventory-agent); the data it accepts
is the same class as inventory's pod images — operational, not secret — but it does name tenant
applications and shell fragments from their args, so the endpoint must not be announced beyond the
agent's default URL. `BearerToken` is a reserved option in the sender so adding authentication later
is a config change, not a code change.
