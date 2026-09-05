# ---- build stage: compile the Go proxy and the tailcat binary ----
FROM golang:1.27.1-bookworm AS build

# Dependency-heavy layers first: installing tailcat (and its module downloads)
# must not re-run when proxy sources change. GOPROXY is pre-approved for this
# network (proxy.golang.org unreachable). CGO off keeps both binaries static so
# the slim runtime stage needs nothing else.
ENV GOPROXY=https://goproxy.cn,direct \
    CGO_ENABLED=0
RUN go install github.com/tailscale/tailcat/cmd/tailcat@latest

WORKDIR /src
COPY go/ /src/go/
RUN cd /src/go && go build -trimpath -ldflags="-s -w" -o /out/tailcat-dns-proxy .

# ---- runtime stage ----
FROM debian:bookworm-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/tailcat-dns-proxy /usr/local/bin/tailcat-dns-proxy
COPY --from=build /go/bin/tailcat /usr/local/bin/tailcat

WORKDIR /app
# dns.txt holds real tokens and is git-ignored; bake the example as a build
# fallback (compose mounts your real dns.txt over it at runtime).
COPY dns.txt.example /app/dns.txt

ENV LISTEN=0.0.0.0:1080 \
    UPSTREAM=127.0.0.1:0 \
    DNS_FILE=/app/dns.txt \
    TAILCAT_BIN=tailcat

# 1080 = SOCKS5 to clients; tailcat socks uses an internal random high port.
EXPOSE 1080

# exec so PID 1 forwards signals -> proxy cleans up its tailcat child.
CMD ["sh", "-c", "exec tailcat-dns-proxy --listen \"$LISTEN\" --dns-file \"$DNS_FILE\" --upstream \"$UPSTREAM\" --tailcat-bin \"$TAILCAT_BIN\""]
