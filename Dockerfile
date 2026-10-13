# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/pingcord .

# raw ICMP ソケットのため root で動かす (付与する capability は NET_RAW のみ)
FROM gcr.io/distroless/static-debian12
COPY --from=build /out/pingcord /pingcord
ENTRYPOINT ["/pingcord"]
CMD ["-config", "/etc/pingcord/config.yaml"]
