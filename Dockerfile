# Кросс-компиляция Go на платформе сборщика: QEMU не нужен, в финальной стадии нет RUN.
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /metadata-proxy ./cmd/metadata-proxy

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /metadata-proxy /metadata-proxy
ENTRYPOINT ["/metadata-proxy"]
