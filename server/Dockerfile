# syntax=docker/dockerfile:1
#
# Conference server image: a static binary on an empty base, running as a non-root user.
#
#   docker build -t confserver -f server/Dockerfile server
#   docker run --rm -p 6100:6100/tcp -p 6100:6100/udp -p 127.0.0.1:6101:6101 confserver
#
# Behind a TLS-intercepting proxy, pass its CA bundle as a build secret (optional):
#   docker build --network host --build-arg HTTPS_PROXY \
#     --secret id=ca,src=/path/to/ca-bundle.crt -t confserver -f server/Dockerfile server

FROM golang:1.24.7-alpine AS build
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
ENTRYPOINT ["/confserver"]
# Inside the container the server must listen on all interfaces; publish the metrics
# port only on the host's loopback (see the docker run line above).
CMD ["-control-addr", "0.0.0.0:6100", "-media-addr", "0.0.0.0:6100", "-metrics-addr", "0.0.0.0:6101"]
