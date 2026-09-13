FROM --platform=$BUILDPLATFORM golang:1.26.8-alpine3.24@sha256:ce864e7223ac17b1775e6fd0b4c0db580c2eb50e7953a427916379e4b92a1628 AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG REVISION=dev
ARG BUILDTIME=""
ENV CGO_ENABLED=0 \
    GOFLAGS=-mod=readonly \
    GOWORK=off
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
# timetzdata embeds zoneinfo: scratch has none, so TZ would otherwise be ignored.
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -tags timetzdata \
    -ldflags="-s -w -X main.version=${VERSION} -X main.revision=${REVISION} -X main.buildTime=${BUILDTIME}" \
    -o /out/exportarr ./cmd/exportarr

FROM scratch
# The CA bundle ships with the digest-pinned golang image; no apk packages needed.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/exportarr /exportarr
USER 65532:65532
EXPOSE 9707/tcp
ENTRYPOINT ["/exportarr"]
LABEL \
    org.opencontainers.image.title="exportarr" \
    org.opencontainers.image.source="https://github.com/avargaskun/exportarr"
