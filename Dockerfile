
# The control plane. The web UI is bundled by cmd/buildweb (esbuild via its Go
# API) and go:embed'ed, so no Node toolchain exists in this image or in CI.
FROM alpine:3.21 AS build
ADD --chmod=755 "https://dl.pazer.build/go-toolchain?os=linux&arch=amd64" /usr/local/bin/go-toolchain
# The binary is the go command when it is named go; go-toolchain has no "go" subcommand of its own.
RUN ln -s /usr/local/bin/go-toolchain /usr/local/bin/go
# go-toolchain resolves some modules through git and fetches over TLS, so the build stage needs both.
RUN apk add --no-cache git ca-certificates
# Docker mounts /dev/shm noexec, so an APE unpacks its loader into /tmp.
ENV GO_TOOLCHAIN_LINKED_GO=1 APE_LOADERDIR=/tmp
WORKDIR /src

RUN --mount=type=cache,target=/root/go/pkg/mod \
	--mount=type=bind,source=go.mod,target=go.mod \
	--mount=type=bind,source=go.sum,target=go.sum \
	go mod download

COPY . .

ARG VERSION=dev
RUN --mount=type=cache,target=/root/go/pkg/mod \
	--mount=type=cache,target=/root/.cache/go-build \
	go build -trimpath \
	-ldflags "-s -w -X main.version=${VERSION}" \
	-o /out/ciplatform ./cmd/ciplatform

FROM alpine:3.21 AS control-plane
RUN apk add --no-cache ca-certificates tzdata \
	&& adduser -D -u 10001 ciplatform \
	&& mkdir -p /var/lib/ciplatform \
	&& chown ciplatform:ciplatform /var/lib/ciplatform
COPY --from=build /out/ciplatform /usr/local/bin/ciplatform
ENV APE_LOADERDIR=/tmp
USER ciplatform
# The binary is an APE, which the kernel will not exec directly; /bin/sh runs its header, which boots it through the loader.
RUN ["/bin/sh", "/usr/local/bin/ciplatform", "-version"]
EXPOSE 8080
VOLUME /var/lib/ciplatform
ENTRYPOINT ["/bin/sh", "/usr/local/bin/ciplatform"]
