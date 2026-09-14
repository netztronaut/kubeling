FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY pkg/ pkg/
RUN CGO_ENABLED=0 go build -o /out/kubeling ./cmd/kubeling

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/kubeling /kubeling
USER 65532:65532
ENTRYPOINT ["/kubeling"]
