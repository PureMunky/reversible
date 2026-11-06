# The demos, plus the Go toolchain they need at runtime: the factory
# compiles every version of the generated service inside the container.
# Built for deploy/kind (see docs/USER-GUIDE.md); nothing here is kind-specific.
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/ ./cmd/factory ./cmd/incidents ./cmd/fakeclaude ./cmd/designer ./cmd/reach ./cmd/runner ./cmd/modelgateway

FROM golang:1.24-alpine
COPY --from=build /out/ /usr/local/bin/
# Generated code is standard-library only: no module downloads, no
# toolchain switching, no telemetry. The root filesystem is read-only in
# kind; HOME, the build cache and temp dirs are writable volumes.
ENV HOME=/work \
    GOCACHE=/work/.cache/go-build \
    GOPATH=/work/go \
    TMPDIR=/tmp \
    GOTOOLCHAIN=local \
    GOPROXY=off \
    GOFLAGS=-mod=mod \
    GOTELEMETRY=off \
    CGO_ENABLED=0
USER 65532:65532
WORKDIR /work
EXPOSE 8080
CMD ["incidents", "-serve", ":8080"]
