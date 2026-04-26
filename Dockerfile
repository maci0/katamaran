# Stage 1 — builder
FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod ./
COPY cmd/katamaran/ cmd/katamaran/
COPY internal/ internal/
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-X github.com/maci0/katamaran/internal/buildinfo.Version=${VERSION}" \
    -o /katamaran ./cmd/katamaran/

# Stage 2 — runtime
FROM alpine:3.23
RUN apk add --no-cache iproute2 kmod
COPY --from=builder /katamaran /usr/local/bin/katamaran
ENTRYPOINT ["/usr/local/bin/katamaran"]

ARG VERSION=dev
LABEL org.opencontainers.image.source="https://github.com/maci0/katamaran" \
      org.opencontainers.image.description="Zero-packet-drop live migration for Kata Containers" \
      org.opencontainers.image.version="${VERSION}"
