#!/usr/bin/env python3
"""tailcat-dns-proxy: a SOCKS5 front proxy that rewrites real domain names to
tailcat tokens (tc...) and chains to a single standalone `tailcat socks` upstream.

Reads a dns.txt mapping (one token per line, then its domains space/tab separated):

    # token          domains (any number)
    tcXYZabc123      www.example.com  api.example.com
    tcDEF456ghi      foo.com

For each CONNECT, the requested host is looked up; on a hit it is replaced by
the token, then forwarded (as a SOCKS5 client) to the upstream `tailcat socks`
listener, which dials that token over its WireGuard/DERP tunnel. Hosts with no
match are forwarded unchanged (transparent passthrough).

On startup it auto-launches `tailcat socks` (unless --no-autolaunch). The socks
listen port is taken from --upstream host:port; if the port is 0 or omitted, a
high random free port is chosen. dns.txt is hot-reloaded when it changes.

Usage:
    python3 tailcat_dns_proxy.py --listen 127.0.0.1:1080 --dns-file dns.txt
"""
import argparse
import atexit
import os
import random
import select
import signal
import socket
import subprocess
import sys
import threading
import time


def load_dns_map(path):
    """Parse dns.txt into {domain: token}. Domains are stored lowercased.
    Blank lines and lines starting with '#' are ignored. A line with only a
    token (no domains) contributes nothing.
    """
    mapping = {}
    with open(path, "r", encoding="utf-8") as f:
        for raw in f:
            line = raw.strip()
            if not line or line.startswith("#"):
                continue
            parts = line.split()
            token, domains = parts[0], parts[1:]
            for d in domains:
                mapping[d.lower()] = token
    return mapping


def rewrite_host(host, dns_map):
    """Return the token for host if mapped, else host unchanged."""
    return dns_map.get(host.lower(), host)


# ---- SOCKS5 request parsing (client side of the proxy) ----

def _readn(sock, n):
    buf = b""
    while len(buf) < n:
        chunk = sock.recv(n - len(buf))
        if not chunk:
            raise ConnectionError("connection closed mid-read")
        buf += chunk
    return buf


def _read_atyp_addr(sock, atyp):
    """Return host_str (domain or textual IP)."""
    if atyp == 0x01:  # IPv4
        return socket.inet_ntoa(_readn(sock, 4))
    if atyp == 0x03:  # domain
        dlen = _readn(sock, 1)[0]
        return _readn(sock, dlen).decode("ascii")
    if atyp == 0x04:  # IPv6
        return socket.inet_ntop(socket.AF_INET6, _readn(sock, 16))
    raise ValueError(f"unsupported ATYP {atyp}")


def _relay(client, upstream):
    fds = [client, upstream]
    while fds:
        r, _, _ = select.select(fds, [], [], 300)
        if not r:
            break
        for s in r:
            try:
                data = s.recv(65536)
            except OSError:
                data = b""
            if not data:
                for x in fds:
                    try: x.shutdown(socket.SHUT_RDWR)
                    except OSError: pass
                return
            peer = upstream if s is client else client
            try:
                peer.sendall(data)
            except OSError:
                return


class Socks5Proxy:
    def __init__(self, dns_map, upstream_addr, listen=("127.0.0.1", 1080)):
        self.dns_map = dns_map
        self.upstream_addr = upstream_addr
        host, port = listen
        self._sock = socket.socket()
        self._sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        # On macOS/BSD a fresh listener can't bind over TIME_WAIT client
        # connections with SO_REUSEADDR alone; SO_REUSEPORT is the workaround.
        reuseport = getattr(socket, "SO_REUSEPORT", None)
        if reuseport is not None:
            try:
                self._sock.setsockopt(socket.SOL_SOCKET, reuseport, 1)
            except OSError:
                pass
        self._sock.bind((host, port))
        self._sock.listen(128)
        self.port = self._sock.getsockname()[1]

    def serve_forever(self):
        while True:
            try:
                conn, _ = self._sock.accept()
            except OSError:
                return
            threading.Thread(target=self._handle, args=(conn,), daemon=True).start()

    def _handle(self, client):
        try:
            self._handle_inner(client)
        except (ConnectionError, ValueError, OSError):
            pass
        finally:
            try: client.close()
            except OSError: pass

    def _handle_inner(self, client):
        # --- greeting ---
        ver = _readn(client, 1)
        if ver != b"\x05":
            client.close(); return
        nmethods = _readn(client, 1)[0]
        methods = _readn(client, nmethods)
        if 0x00 in methods:
            client.sendall(b"\x05\x00")           # no auth
        else:
            client.sendall(b"\x05\xFF")           # no acceptable methods
            return

        # --- request ---
        req = _readn(client, 4)
        cmd, atyp = req[1], req[3]
        host = _read_atyp_addr(client, atyp)
        port = int.from_bytes(_readn(client, 2), "big")

        if cmd != 0x01:                            # only CONNECT
            _socks_fail(client); return

        target_host = rewrite_host(host, self.dns_map)

        # --- forward to upstream as a SOCKS5 client ---
        try:
            up = socket.create_connection(self.upstream_addr, timeout=15)
        except OSError:
            _socks_fail(client); return
        try:
            if not _upstream_connect(up, target_host, port):
                up.close(); _socks_fail(client); return
        except OSError:
            up.close(); _socks_fail(client); return

        # success reply to client (bind 0.0.0.0:0)
        client.sendall(b"\x05\x00\x00\x01" + b"\x00" * 6)
        _relay(client, up)
        try: up.close()
        except OSError: pass


def _socks_fail(client):
    # general failure, unassigned address
    client.sendall(b"\x05\x01\x00\x01" + b"\x00" * 6)


def _upstream_connect(up, host, port):
    """Perform SOCKS5 client handshake with the upstream; return True on success."""
    up.sendall(b"\x05\x01\x00")                   # greeting, offer no-auth
    resp = _readn(up, 2)
    if resp[0] != 0x05 or resp[1] != 0x00:
        return False

    hb = host.encode("ascii")
    # prefer domain ATYP so the token/domain is resolved upstream (socks5h semantics)
    if _looks_like_ip(host):
        if ":" in host:  # ipv6
            req = b"\x05\x01\x00\x04" + socket.inet_pton(socket.AF_INET6, host)
        else:
            req = b"\x05\x01\x00\x01" + socket.inet_aton(host)
    else:
        req = b"\x05\x01\x00\x03" + bytes([len(hb)]) + hb
    req += port.to_bytes(2, "big")
    up.sendall(req)

    rep = _readn(up, 4)
    if rep[0] != 0x05 or rep[1] != 0x00:
        return False
    # drain bound address
    _read_atyp_addr(up, rep[3])
    _readn(up, 2)
    return True


def _looks_like_ip(host):
    try:
        socket.inet_aton(host); return True
    except OSError:
        pass
    try:
        socket.inet_pton(socket.AF_INET6, host); return True
    except OSError:
        return False


def parse_addr(s, default_port=None):
    host, _, port = s.rpartition(":")
    host = host or "127.0.0.1"
    return (host, int(port) if port else default_port)


def free_high_port(host="127.0.0.1"):
    """Return a free high port (ephemeral range, randomly probed) on host."""
    for _ in range(20):
        p = random.randint(20000, 60999)
        s = socket.socket()
        try:
            s.bind((host, p))
            return p
        except OSError:
            continue
        finally:
            s.close()
    # fall back to OS-assigned
    s = socket.socket()
    try:
        s.bind((host, 0))
        return s.getsockname()[1]
    finally:
        s.close()


def watch_dns_file(path, proxy, interval=1.0, stop=None):
    """Poll path mtime; hot-reload proxy.dns_map when it changes. Keeps the old
    map on read errors. Returns when stop Event is set (if provided). Performs an
    initial load so the map is populated even if the caller started empty."""
    try:
        proxy.dns_map = load_dns_map(path)
        last = os.stat(path).st_mtime
    except OSError:
        last = None
    while not (stop and stop.is_set()):
        time.sleep(interval)
        try:
            mtime = os.stat(path).st_mtime
        except OSError:
            continue
        if last is not None and mtime == last:
            continue
        try:
            new_map = load_dns_map(path)
        except OSError as e:
            print(f"[tailcat-dns-proxy] reload failed ({e}); keeping previous map",
                  file=sys.stderr)
            continue
        last = mtime
        proxy.dns_map = new_map
        print(f"[tailcat-dns-proxy] reloaded {path}: "
              f"{len(new_map)} domain(s) -> {len(set(new_map.values()))} token(s)")


def _wait_ready(addr, timeout=15.0):
    """Poll TCP-connect to addr until it accepts or timeout; return True if ready."""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            s = socket.create_connection(addr, timeout=1)
            s.close()
            return True
        except OSError:
            time.sleep(0.2)
    return False


def _spawn_tailcat_socks(bin_path, listen_addr):
    """Launch `tailcat socks --listen=<addr>` and return the Popen (or None on failure)."""
    host, port = listen_addr
    cmd = [bin_path, "socks", "--listen=%s:%d" % (host, port)]
    try:
        return subprocess.Popen(cmd)
    except OSError as e:
        print(f"[tailcat-dns-proxy] failed to launch {bin_path}: {e}", file=sys.stderr)
        return None


def _terminate(proc):
    if proc.poll() is not None:
        return
    proc.terminate()
    try:
        proc.wait(timeout=5)
    except subprocess.TimeoutExpired:
        proc.kill()


def main(argv=None):
    ap = argparse.ArgumentParser(description="tailcat domain->token SOCKS5 proxy")
    ap.add_argument("--listen", default="127.0.0.1:1080", help="SOCKS5 listen addr:port")
    ap.add_argument("--dns-file", default="dns.txt", help="domain->token mapping file")
    ap.add_argument("--upstream", default="127.0.0.1:0",
                    help="upstream tailcat socks addr:port; port 0 (default) = high random free port")
    ap.add_argument("--tailcat-bin", default="tailcat", help="path to tailcat binary (auto-launched)")
    ap.add_argument("--no-autolaunch", action="store_true",
                    help="do not spawn tailcat socks; use an already-running upstream")
    ap.add_argument("--no-watch", action="store_true", help="disable dns.txt hot-reload")
    args = ap.parse_args(argv)

    dns_map = load_dns_map(args.dns_file)

    # Resolve the tailcat socks listen port: explicit port wins, else high random.
    up_host, up_port = parse_addr(args.upstream)
    if not up_port:
        up_port = free_high_port(up_host)
    upstream = (up_host, up_port)

    proxy = Socks5Proxy(dns_map, upstream, parse_addr(args.listen))

    proc = None
    if not args.no_autolaunch:
        proc = _spawn_tailcat_socks(args.tailcat_bin, upstream)
        if proc is None:
            return 1
        atexit.register(_terminate, proc)
        if not _wait_ready(upstream):
            print(f"[tailcat-dns-proxy] upstream {upstream} not ready; aborting",
                  file=sys.stderr)
            _terminate(proc)
            return 1
        print(f"[tailcat-dns-proxy] auto-launched {args.tailcat_bin} socks on {up_host}:{up_port}")

    # Clean up the child on Ctrl-C or SIGTERM by surfacing them as exceptions.
    def _on_signal(signum, frame):
        raise KeyboardInterrupt
    try:
        signal.signal(signal.SIGTERM, _on_signal)
    except (ValueError, OSError):
        pass  # not on the main thread / unsupported

    if not args.no_watch:
        threading.Thread(target=watch_dns_file, args=(args.dns_file, proxy),
                         daemon=True).start()

    print(f"[tailcat-dns-proxy] listening socks5h://{proxy._sock.getsockname()[0]}:{proxy.port}")
    print(f"[tailcat-dns-proxy] {len(dns_map)} domain(s) mapped -> {len(set(dns_map.values()))} token(s)")
    print(f"[tailcat-dns-proxy] upstream {up_host}:{up_port}")
    try:
        proxy.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        if proc is not None:
            _terminate(proc)
    return 0


if __name__ == "__main__":
    sys.exit(main())
