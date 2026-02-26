# Stage 1 — builder
FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY go.mod ./
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 go build -o /katamaran ./cmd/katamaran/

# Stage 2 — runtime
FROM alpine:3.20
RUN apk add --no-cache iproute2
COPY --from=builder /katamaran /usr/local/bin/katamaran
ENTRYPOINT ["/usr/local/bin/katamaran"]

LABEL org.opencontainers.image.source="https://github.com/maci0/katamaran"
LABEL org.opencontainers.image.description="Zero-packet-drop live migration for Kata Containers"
