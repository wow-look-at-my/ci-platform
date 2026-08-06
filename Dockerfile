# syntax=docker/dockerfile:1

# The control plane. The web UI is bundled by cmd/buildweb (esbuild via its Go
# API) and go:embed'ed, so no Node toolchain exists in this image or in CI.
FROM golang:1.25-alpine AS build
WORKDIR /src

RUN --mount=type=cache,target=/go/pkg/mod \
	--mount=type=bind,source=go.mod,target=go.mod \
	--mount=type=bind,source=go.sum,target=go.sum \
	go mod download

COPY . .

ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
	--mount=type=cache,target=/root/.cache/go-build \
	CGO_ENABLED=0 go build -trimpath \
	-ldflags "-s -w -X main.version=${VERSION}" \
	-o /out/ciplatform ./cmd/ciplatform

FROM alpine:3.21 AS control-plane
RUN apk add --no-cache ca-certificates tzdata \
	&& adduser -D -u 10001 ciplatform \
	&& mkdir -p /var/lib/ciplatform \
	&& chown ciplatform:ciplatform /var/lib/ciplatform
COPY --from=build /out/ciplatform /usr/local/bin/ciplatform
USER ciplatform
EXPOSE 8080
VOLUME /var/lib/ciplatform
ENTRYPOINT ["/usr/local/bin/ciplatform"]
