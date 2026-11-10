# ---- the argocd CLI: core mode is how the agent talks to Argo CD ----
# Pinned to the Argo CD version the platform chart ships (argocd_app_version); bump together.
FROM quay.io/argoproj/argocd:v3.2.12@sha256:a38c44a3b5013f867de870ea42d39710e0443a0cc8aa507bbdf94ec0f6883467 AS argocd

# ---- build stage ----
FROM golang:1.26-alpine@sha256:28d89ee9cc0ff9fec75c82ca201e6bf7fdf9a679d4b7b24dfa04f2bb766bb468 AS builder

# Set by the container_image workflow to the git tag (vX.Y.Z) and commit;
# surface as agent_version / agent_commit in every report.
ARG VERSION=dev
ARG COMMIT_SHA=local

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT_SHA}" -o /out/argocd-app-chart-preflight .

# ---- runtime stage ----
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

WORKDIR /

COPY --from=builder /out/argocd-app-chart-preflight /argocd-app-chart-preflight
# Static Go binary; exec'd directly by the agent (no shell). ARGOCD_BIN defaults to this path.
COPY --from=argocd /usr/local/bin/argocd /argocd

ARG VERSION=dev
ARG COMMIT_SHA=local
LABEL org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT_SHA}"

# No port needed; it runs once per CronJob invocation and exits 0. /tmp must be
# writable (an emptyDir): the kubeconfig and the CLI's HOME live there.
ENTRYPOINT ["/argocd-app-chart-preflight"]
