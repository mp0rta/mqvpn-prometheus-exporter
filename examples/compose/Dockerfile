# syntax=docker/dockerfile:1.7
#
# Image build for examples/compose. Used by local development and self-host
# production deployments via docker compose. The systemd unit at
# examples/systemd/mqvpn-exporter.service is an alternative for Docker-free
# hosts (see docs/README.md §4.2).

FROM golang:1.27-alpine AS build
WORKDIR /src

# Cache module downloads in their own layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" \
    -o /out/mqvpn-prometheus-exporter .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/mqvpn-prometheus-exporter /mqvpn-prometheus-exporter
USER nonroot:nonroot
ENTRYPOINT ["/mqvpn-prometheus-exporter"]
