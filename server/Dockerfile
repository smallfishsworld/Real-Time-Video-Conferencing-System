# syntax=docker/dockerfile:1
#
# Conference server image: a static binary on an empty base, running as a non-root user.
#
#   docker build -t confserver -f server/Dockerfile server
#   docker run --rm -p 127.0.0.1:6100:6100/tcp -p 127.0.0.1:6100:6100/udp \
#     -p 127.0.0.1:6101:6101 confserver
#
# The ports are published on the host's loopback only. To serve other machines,
# publish them on a LAN address instead (-p 192.0.2.10:6100:6100/tcp ...). Arguments
# after the image name are appended to the server's flags, for example
# `docker run ... confserver -p2p=false -log-format json`.
#
# Behind a TLS-intercepting proxy, pass its CA bundle as a build secret (optional):
#   docker build --network host --build-arg HTTPS_PROXY \
#     --secret id=ca,src=/path/to/ca-bundle.crt -t confserver -f server/Dockerfile server
# On a shared machine, --build-arg GOMAXPROCS=2 --build-arg GOFLAGS=-p=2 limits the
# build to two threads.

FROM golang:1.24.7-alpine AS build
ARG GOMAXPROCS
ARG GOFLAGS
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=secret,id=ca \
    --mount=type=cache,target=/go/pkg/mod \
    if [ -s /run/secrets/ca ]; then export SSL_CERT_FILE=/run/secrets/ca; fi; \
    go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/confserver ./cmd/confserver

FROM scratch
COPY --from=build /out/confserver /confserver
# 65532 is the conventional "nonroot" user of distroless images; scratch has no
# /etc/passwd, so the numeric id is used directly.
USER 65532:65532
EXPOSE 6100/tcp 6100/udp 6101/tcp
# Inside the container the server must listen on all interfaces; which host
# interfaces reach it is decided by `docker run -p` (see above). The addresses are
# part of ENTRYPOINT, not CMD, so that flags given to `docker run` are appended to
# them instead of replacing them (a later -control-addr still overrides).
ENTRYPOINT ["/confserver", "-control-addr", "0.0.0.0:6100", "-media-addr", "0.0.0.0:6100", "-metrics-addr", "0.0.0.0:6101"]
CMD []
# Healthy means the control plane accepts a connection and answers hello, not just
# that the process or /metrics is up. If you move -control-addr, override this.
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD ["/confserver", "-healthcheck", "127.0.0.1:6100"]
