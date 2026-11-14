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
RUN go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/tailcat-socks .

# ---- runtime stage ----
# Alpine, not debian-slim: the binary is fully static (CGO off), so the
# 116MB glibc base buys nothing; alpine's sh keeps the env-expanding CMD intact.
FROM alpine:3.22

RUN apk add --no-cache ca-certificates

COPY --from=build /out/tailcat-socks /usr/local/bin/tailcat-socks

WORKDIR /app
# dns.txt holds real tokens and is git-ignored; bake the example as a build
# fallback (compose mounts your real dns.txt over it at runtime).
COPY data/dns.txt.example /app/dns.txt

ENV LISTEN=0.0.0.0:1080 \
    DNS_FILE=/app/dns.txt \
    TAILCAT_CLIENT_KEY_FILE=/data/tailcat-client-key \
    ADMIN_ADDR=127.0.0.1:8080

# 1080 = SOCKS5 to clients. Admin stays container-internal (127.0.0.1) unless
# you deliberately expose it; see docker-compose.yml.
EXPOSE 1080
VOLUME ["/data"]

# exec so PID 1 forwards signals -> proxy runs its shutdown order
# (SOCKS listener, relays, admin Shutdown(1s), tailcat clients).
CMD ["sh", "-c", "exec tailcat-socks --listen \"$LISTEN\" --dns-file \"$DNS_FILE\" --client-key-file \"$TAILCAT_CLIENT_KEY_FILE\" --admin-addr \"$ADMIN_ADDR\""]
