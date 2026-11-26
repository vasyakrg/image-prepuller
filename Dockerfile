# syntax=docker/dockerfile:1

# ---------- build stage -------------------------------------------------------
# Go 1.24 matches the go directive in go.mod (AGENTS.md section 3).
FROM golang:1.24-alpine AS builder

ARG VERSION=dev
ARG COMMIT=unknown

# Cross-compilation targets are passed by buildx (linux/amd64, linux/arm64, ...).
ARG TARGETOS=linux
ARG TARGETARCH=amd64

# ADR-001: modules come from a GOPROXY mirror, no vendor/ directory.
# Override with --build-arg GOPROXY=direct for an isolated build.
ARG GOPROXY=https://goproxy.io,direct

ENV CGO_ENABLED=0 \
    GOOS=${TARGETOS} \
    GOARCH=${TARGETARCH} \
    GOPROXY=${GOPROXY}

WORKDIR /src

# Module layer: invalidated only when go.mod/go.sum change.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

# NOTE (2026-11-26): cmd/prepuller/main.go has no `version`/`commit` package
# variables yet, so -X main.version/-X main.commit are intentionally NOT passed.
# VERSION/COMMIT are baked as OCI labels instead. When the integrator adds the
# variables, extend the flags below to:
#   -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}"
RUN go build \
      -trimpath \
      -buildvcs=false \
      -ldflags="-s -w" \
      -o /out/prepuller ./cmd/prepuller

# ---------- runtime stage -----------------------------------------------------
# Static distroless image: CA certificates, /etc/passwd and tzdata only; runs as
# uid/gid 65532 (nonroot), no shell, no package manager.
FROM gcr.io/distroless/static-debian12:nonroot

ARG VERSION=dev
ARG COMMIT=unknown

LABEL org.opencontainers.image.title="image-prepuller" \
      org.opencontainers.image.description="Kubernetes image cache prewarmer (hub/spoke/node-agent)" \
      org.opencontainers.image.source="https://github.com/vasyakrg/image-prepuller" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}"

COPY --from=builder /out/prepuller /prepuller

USER 65532:65532

# 8080/9090 hub HTTP+metrics, 8081/9091 spoke HTTP+metrics, 9443 agent HTTP/JSON.
EXPOSE 8080 9090 8081 9091 9443

ENTRYPOINT ["/prepuller"]

# Overridden by Helm (.Values.hub/.spoke/.agent args). Without auth env the hub
# exits on purpose (AGENTS.md section 8), so pass PREPULLER_AUTH_USER/PASS.
CMD ["controller", "-role=hub", "-mock", "-http-addr=:8080", "-metrics-addr=:9090"]
