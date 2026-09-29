# Stage 1: builder
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS builder
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
FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6
RUN apk add --no-cache iproute2 kmod
COPY --from=builder /katamaran /usr/local/bin/katamaran
COPY --from=builder /katamaran-factory /usr/local/bin/katamaran-factory
COPY --from=builder /containerd-shim-katamaran-adopted-v2 /usr/local/bin/containerd-shim-katamaran-adopted-v2
ENTRYPOINT ["/usr/local/bin/katamaran"]

ARG VERSION=dev
LABEL org.opencontainers.image.source="https://github.com/maci0/katamaran" \
      org.opencontainers.image.description="Zero-packet-drop live migration for Kata Containers" \
      org.opencontainers.image.version="${VERSION}"
