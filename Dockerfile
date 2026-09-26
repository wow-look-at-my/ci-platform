
# The control plane. The web UI is bundled by cmd/buildweb (esbuild via its Go
# API) and go:embed'ed, so no Node toolchain exists in this image or in CI.
FROM alpine:3.21 AS build
ADD --chmod=755 "https://dl.pazer.build/go-toolchain?os=linux&arch=amd64" /usr/local/bin/go-toolchain
ENV GO_TOOLCHAIN_LINKED_GO=1 GOPROXY=https://proxy.golang.org,direct GOSUMDB=sum.golang.org
WORKDIR /src

RUN --mount=type=cache,target=/root/go/pkg/mod \
	--mount=type=bind,source=go.mod,target=go.mod \
	--mount=type=bind,source=go.sum,target=go.sum \
	go-toolchain go mod download

COPY . .

ARG VERSION=dev
RUN --mount=type=cache,target=/root/go/pkg/mod \
	--mount=type=cache,target=/root/.cache/go-build \
	go-toolchain go build -trimpath \
	-ldflags "-s -w -X main.version=${VERSION}" \
	-o /out/ciplatform ./cmd/ciplatform
# The build is an APE; its first run rewrites it in place to a native binary, so the exec-form ENTRYPOINT below can start it.
RUN /out/ciplatform -version

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
