# ---- build stage: compile the Go proxy (tailcat is a library dependency now) ----
FROM golang:1.27.1-bookworm AS build

# Dependency-heavy layers first: the big tailcat/tailscale module set downloads
# from go.sum and must not re-run when proxy sources change. GOPROXY defaults to
# goproxy.cn for the author's network (proxy.golang.org unreachable there);
# CI overrides it with --build-arg GOPROXY=https://proxy.golang.org,direct.
# CGO off keeps the binary static so the slim runtime stage needs nothing else.
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY} \
    CGO_ENABLED=0

# The version lives only in .version at the repo root; the build hands it to
# the linker, so the binary and the image tag can never disagree. Unset (a
# plain `docker build`) falls back to "dev".
ARG VERSION=dev

WORKDIR /build
COPY src/go.mod src/go.sum ./
RUN go mod download
COPY src/ ./
RUN go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/tailcat-proxy .

# ALPINE runtime keeps the image small; the binary is fully static (CGO off).
FROM alpine:3.22

RUN apk add --no-cache ca-certificates

COPY --from=build /out/tailcat-proxy /usr/local/bin/tailcat-proxy

WORKDIR /app
# dns.txt and config.yaml hold real tokens and plaintext passwords and are
# git-ignored; bake the examples into the data dir as a build fallback so the
# container starts on its own. compose mounts the real data over /data.
RUN mkdir -p /data
COPY data/dns.txt.example /data/dns.txt
COPY data/config.yaml.example /data/config.yaml

ENV LISTEN=0.0.0.0:1080 \
    ADMIN_ADDR=127.0.0.1:8080 \
    DATA_PATH=/data

# dns.txt, config.yaml and the client key all come from DATA_PATH; the image
# only bakes example files as a build fallback so it can start on its own.
# compose mounts the real data directory over /data at runtime.
EXPOSE 1080
VOLUME ["/data"]

# exec so PID 1 forwards signals -> proxy runs its shutdown order
# (SOCKS listener, relays, admin Shutdown(1s), tailcat clients).
CMD ["sh", "-c", "exec tailcat-proxy --listen \"$LISTEN\" --admin-addr \"$ADMIN_ADDR\" --data-path \"$DATA_PATH\""]
