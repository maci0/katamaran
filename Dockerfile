# Stage 1: builder
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS builder
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
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -buildvcs=false -mod=readonly \
    -ldflags "-X github.com/maci0/katamaran/internal/buildinfo.Version=${VERSION}" \
    -o /katamaran ./cmd/katamaran/ && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -buildvcs=false -mod=readonly \
    -ldflags "-X github.com/maci0/katamaran/internal/buildinfo.Version=${VERSION}" \
    -o /katamaran-factory ./cmd/katamaran-factory/ && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -buildvcs=false -mod=readonly \
    -ldflags "-X github.com/maci0/katamaran/internal/buildinfo.Version=${VERSION}" \
    -o /containerd-shim-katamaran-adopted-v2 ./cmd/containerd-shim-katamaran-adopted-v2/

# Stage 2: runtime
FROM alpine:3.24@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a
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
# privileged container, which breaks that path.
ENTRYPOINT ["/usr/local/bin/katamaran"]

ARG VERSION=dev
LABEL org.opencontainers.image.title="katamaran" \
      org.opencontainers.image.source="https://github.com/maci0/katamaran" \
      org.opencontainers.image.documentation="https://github.com/maci0/katamaran/blob/main/docs/INSTALL.md" \
      org.opencontainers.image.description="Zero-packet-drop live migration for Kata Containers" \
      org.opencontainers.image.version="${VERSION}"
