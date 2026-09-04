"""Tests for tailcat_dns_proxy: dns.txt parsing, host rewriting, hot-reload,
and end-to-end SOCKS5 host rewriting through a fake upstream.

Run:  python3 -m pytest tests -v     (or: python3 tests/test_tailcat_dns_proxy.py)
"""
import os
import socket
import struct
import sys
import tempfile
import threading
import time

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "src"))

from tailcat_dns_proxy import load_dns_map, rewrite_host, Socks5Proxy, watch_dns_file


# ---------- dns.txt parsing ----------

def test_load_dns_map_basic():
    txt = """
# comment line
tcXYZabc123   www.example.com  api.example.com
tcDEF456ghi   foo.com

tcGGG         bar.com\tbaz.com
"""
    with tempfile.NamedTemporaryFile("w", suffix=".txt", delete=False) as f:
        f.write(txt)
        path = f.name
    try:
        m = load_dns_map(path)
    finally:
        os.unlink(path)
    assert m["www.example.com"] == "tcXYZabc123"
    assert m["api.example.com"] == "tcXYZabc123"
    assert m["foo.com"] == "tcDEF456ghi"
    assert m["bar.com"] == "tcGGG"
    assert m["baz.com"] == "tcGGG"
    assert "tcXYZabc123" not in m


def test_load_dns_map_line_missing_domains():
    with tempfile.NamedTemporaryFile("w", delete=False) as f:
        f.write("tcONLYONE\n"); path = f.name
    try:
        assert load_dns_map(path) == {}
    finally:
        os.unlink(path)


# ---------- host rewriting ----------

def test_rewrite_exact():
    assert rewrite_host("www.example.com", {"www.example.com": "tcXYZ"}) == "tcXYZ"

def test_rewrite_case_insensitive():
    assert rewrite_host("WWW.Example.COM", {"www.example.com": "tcXYZ"}) == "tcXYZ"

def test_rewrite_no_match_returns_original():
    assert rewrite_host("other.com", {"www.example.com": "tcXYZ"}) == "other.com"


# ---------- hot reload ----------

def test_watch_dns_file_reloads():
    class Fake: pass
    p = Fake(); p.dns_map = {}
    stop = threading.Event()
    with tempfile.NamedTemporaryFile("w", suffix=".txt", delete=False) as f:
        f.write("tcA alpha.com\n"); path = f.name
    try:
        t = threading.Thread(target=watch_dns_file, args=(path, p, 0.1, stop), daemon=True)
        t.start()
        time.sleep(0.3)
        assert p.dns_map.get("alpha.com") == "tcA"
        # mutate the file; mtime change should trigger reload
        time.sleep(1.1)  # ensure mtime granularity differs
        with open(path, "w") as f:
            f.write("tcB beta.com\n")
        deadline = time.time() + 3
        while time.time() < deadline and p.dns_map.get("beta.com") != "tcB":
            time.sleep(0.1)
        assert p.dns_map.get("beta.com") == "tcB"
        assert "alpha.com" not in p.dns_map
    finally:
        stop.set(); os.unlink(path)


# ---------- integration: end-to-end through a fake upstream ----------

def _fake_upstream(server_sock, received):
    conn, _ = server_sock.accept()
    with conn:
        hdr = conn.recv(2); conn.recv(hdr[1]); conn.sendall(b"\x05\x00")
        req = conn.recv(4); atyp = req[3]
        if atyp == 0x03:
            dlen = conn.recv(1)[0]; host = conn.recv(dlen).decode()
        elif atyp == 0x01:
            host = socket.inet_ntoa(conn.recv(4))
        else:
            host = "?"
        port = struct.unpack("!H", conn.recv(2))[0]
        received["host"] = host; received["port"] = port
        conn.sendall(b"\x05\x00\x00\x01" + b"\x00" * 6)
        while True:
            data = conn.recv(4096)
            if not data: break
            conn.sendall(b"ECHO:" + data)


def test_socks5_proxy_rewrites_host_and_relays():
    m = {"www.example.com": "tcXYZabc123"}
    upstream = socket.socket(); upstream.bind(("127.0.0.1", 0)); upstream.listen(1)
    up_port = upstream.getsockname()[1]
    received = {}
    threading.Thread(target=_fake_upstream, args=(upstream, received), daemon=True).start()

    proxy = Socks5Proxy(dns_map=m, upstream_addr=("127.0.0.1", up_port), listen=("127.0.0.1", 0))
    threading.Thread(target=proxy.serve_forever, daemon=True).start()
    port = proxy.port

    c = socket.create_connection(("127.0.0.1", port))
    c.sendall(b"\x05\x01\x00")
    assert c.recv(2) == b"\x05\x00"
    host = b"www.example.com"
    c.sendall(b"\x05\x01\x00\x03" + bytes([len(host)]) + host + struct.pack("!H", 8081))
    assert c.recv(10)[1] == 0x00
    c.sendall(b"hello"); got = c.recv(1024)
    c.close(); upstream.close()

    assert received["host"] == "tcXYZabc123", received.get("host")
    assert received["port"] == 8081
    assert got == b"ECHO:hello"


if __name__ == "__main__":
    import traceback
    fails = 0
    for name, fn in sorted(globals().items()):
        if name.startswith("test_") and callable(fn):
            try:
                fn(); print(f"PASS {name}")
            except Exception:
                fails += 1; print(f"FAIL {name}"); traceback.print_exc()
    sys.exit(1 if fails else 0)
