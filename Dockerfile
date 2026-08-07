# Build the manager binary
FROM registry.access.redhat.com/ubi9/go-toolset:1.22 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /opt/app-root/src
# Copy the Go Modules manifests and download deps first for better caching.
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

# Copy the go source
COPY cmd/main.go cmd/main.go
COPY api/ api/
COPY internal/ internal/

# Build a static binary. CGO disabled for a minimal, distroless-compatible image.
USER root
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -a -ldflags="-s -w" -o manager cmd/main.go

# Use Red Hat's UBI micro base for a minimal, supportable runtime image.
FROM registry.access.redhat.com/ubi9/ubi-micro:latest
WORKDIR /
COPY --from=builder /opt/app-root/src/manager .
# 65532 is the conventional "nonroot" UID.
USER 65532:65532

LABEL org.opencontainers.image.title="beacon" \
      org.opencontainers.image.description="OpenShift operator integrating Gateway API health with MetalLB BGP advertisements" \
      org.opencontainers.image.source="https://github.com/bmarlow/beacon" \
      org.opencontainers.image.licenses="Apache-2.0"

ENTRYPOINT ["/manager"]
