# Build the manager binary.
FROM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH
# Stamped into the binary so a running operator can say which build it is. Defaults to "dev", so a
# local `docker build` is not silently labelled as a release.
ARG VERSION=dev

WORKDIR /workspace

# Dependencies first, so a source-only change reuses the cached layer.
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/

# CGO off and a static binary, so the image can be distroless-static.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -a -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o manager cmd/main.go

# distroless-static: no shell, no package manager, nonroot by default. The operator holds broad
# RBAC, so keeping its container surface minimal is worth the debugging inconvenience.
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
