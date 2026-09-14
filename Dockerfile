# syntax=docker/dockerfile:1

# Cross-compile on the build host's native platform instead of emulating
# every target architecture.
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY pkg/ pkg/
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/kubeling ./cmd/kubeling

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/kubeling /kubeling
USER 65532:65532
ENTRYPOINT ["/kubeling"]
