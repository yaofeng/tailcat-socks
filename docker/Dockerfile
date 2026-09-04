# ---- build stage: compile the tailcat binary ----
FROM golang:1.23-bookworm AS tailcatbuild
# Pin a version for reproducibility; bump as needed.
RUN go install github.com/tailscale/tailcat/cmd/tailcat@latest

# ---- runtime stage ----
FROM python:3.12-slim-bookworm

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/*

# tailcat from the build stage
COPY --from=tailcatbuild /go/bin/tailcat /usr/local/bin/tailcat

WORKDIR /app
COPY src/ /app/src/
# dns.txt holds real tokens and is git-ignored; bake the example as a build
# fallback (compose mounts your real dns.txt over it at runtime).
COPY dns.txt.example /app/dns.txt

ENV LISTEN=0.0.0.0:1080 \
    UPSTREAM=127.0.0.1:0 \
    DNS_FILE=/app/dns.txt \
    TAILCAT_BIN=tailcat \
    PYTHONUNBUFFERED=1

# 1080 = SOCKS5 to clients; tailcat socks uses an internal random high port.
EXPOSE 1080

# exec so PID 1 forwards signals -> proxy cleans up its tailcat child.
CMD ["sh", "-c", "exec python3 -u /app/src/tailcat_dns_proxy.py --listen \"$LISTEN\" --dns-file \"$DNS_FILE\" --upstream \"$UPSTREAM\" --tailcat-bin \"$TAILCAT_BIN\""]
