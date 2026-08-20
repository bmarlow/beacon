# Build the manager binary
FROM registry.access.redhat.com/ubi9/go-toolset:1.22 AS builder
ARG TARGETOS
ARG TARGETARCH
# Operator version, injected at link time. Passed by CI (git tag or short SHA).
ARG VERSION=dev

# Run the build stage as root so the module cache and workdir are always
# writable (avoids permission issues across build environments).
USER root
WORKDIR /opt/app-root/src

# Copy the Go Modules manifests and download deps first for better caching.
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

# Copy the go source.
COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/

# Build a static binary. CGO disabled for a minimal, distroless-compatible image.
# The version is stamped into internal/version.Version.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -a -ldflags="-s -w -X github.com/beacon-operator/beacon/internal/version.Version=${VERSION}" \
    -o manager cmd/main.go

# Use Red Hat's UBI micro base for a minimal, supportable runtime image.
FROM registry.access.redhat.com/ubi9/ubi-micro:latest
WORKDIR /
COPY --from=builder /opt/app-root/src/manager .
# 65532 is the conventional "nonroot" UID.
USER 65532:65532

LABEL org.opencontainers.image.title="beacon" \
      org.opencontainers.image.description="OpenShift operator integrating Gateway API health with MetalLB BGP advertisements" \
      org.opencontainers.image.source="https://github.com/beacon-operator/beacon" \
      org.opencontainers.image.licenses="Apache-2.0"

ENTRYPOINT ["/manager"]
