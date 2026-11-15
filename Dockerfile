# Stage 1: builder
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:eb61c3873c97416fff0116553fff65d67a3549f72e11d9758849a689cdd8b523 AS builder
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/katamaran/ cmd/katamaran/
COPY cmd/katamaran-factory/ cmd/katamaran-factory/
COPY cmd/containerd-shim-katamaran-adopted-v2/ cmd/containerd-shim-katamaran-adopted-v2/
COPY internal/ internal/
ARG VERSION=dev
ENV GOTOOLCHAIN=local
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -buildvcs=false -mod=readonly \
    -ldflags "-X github.com/maci0/katamaran/internal/buildinfo.Version=${VERSION} -B none -buildid=" \
    -o /katamaran ./cmd/katamaran/ && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -buildvcs=false -mod=readonly \
    -ldflags "-X github.com/maci0/katamaran/internal/buildinfo.Version=${VERSION} -B none -buildid=" \
    -o /katamaran-factory ./cmd/katamaran-factory/ && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -buildvcs=false -mod=readonly \
    -ldflags "-X github.com/maci0/katamaran/internal/buildinfo.Version=${VERSION} -B none -buildid=" \
    -o /containerd-shim-katamaran-adopted-v2 ./cmd/containerd-shim-katamaran-adopted-v2/

# Stage 2: runtime
FROM alpine:3.24@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf
# iproute2 provides the `ip` and `tc` the migration path shells out to
# (tunnels, tap devices, sch_plug qdiscs). kmod provides modprobe, which
# deploy/daemonset.yaml runs via nsenter to load the tunnel modules on the
# host; busybox's applet does not cover the full module syntax.
RUN apk add --no-cache iproute2 kmod
COPY --from=builder /katamaran /usr/local/bin/katamaran
COPY --from=builder /katamaran-factory /usr/local/bin/katamaran-factory
COPY --from=builder /containerd-shim-katamaran-adopted-v2 /usr/local/bin/containerd-shim-katamaran-adopted-v2
# No USER: the node-setup init container in deploy/daemonset.yaml runs this
# image privileged and needs root to write the host binaries under
# /host/usr/local/bin and to nsenter into the host for modprobe and the
# containerd restart. A non-root process drops the capability set even in a
# privileged container, which breaks that path. The CLI exits on SIGTERM
# (cmd/katamaran/main.go); declare it so a stop does not fall through to
# SIGKILL after the engine's default grace period.
ENTRYPOINT ["/usr/local/bin/katamaran"]
STOPSIGNAL SIGTERM

ARG VERSION=dev
LABEL org.opencontainers.image.title="katamaran" \
      org.opencontainers.image.source="https://github.com/maci0/katamaran" \
      org.opencontainers.image.documentation="https://github.com/maci0/katamaran/blob/main/docs/INSTALL.md" \
      org.opencontainers.image.description="Zero-packet-drop live migration for Kata Containers" \
      org.opencontainers.image.version="${VERSION}"
