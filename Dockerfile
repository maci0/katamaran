# Stage 1: builder
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc AS builder
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
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags "-X github.com/maci0/katamaran/internal/buildinfo.Version=${VERSION}" \
    -o /katamaran ./cmd/katamaran/ && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags "-X github.com/maci0/katamaran/internal/buildinfo.Version=${VERSION}" \
    -o /katamaran-factory ./cmd/katamaran-factory/ && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags "-X github.com/maci0/katamaran/internal/buildinfo.Version=${VERSION}" \
    -o /containerd-shim-katamaran-adopted-v2 ./cmd/containerd-shim-katamaran-adopted-v2/

# Stage 2: runtime
FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b
RUN apk add --no-cache iproute2 kmod
COPY --from=builder /katamaran /usr/local/bin/katamaran
COPY --from=builder /katamaran-factory /usr/local/bin/katamaran-factory
COPY --from=builder /containerd-shim-katamaran-adopted-v2 /usr/local/bin/containerd-shim-katamaran-adopted-v2
ENTRYPOINT ["/usr/local/bin/katamaran"]

ARG VERSION=dev
LABEL org.opencontainers.image.source="https://github.com/maci0/katamaran" \
      org.opencontainers.image.description="Zero-packet-drop live migration for Kata Containers" \
      org.opencontainers.image.version="${VERSION}"
