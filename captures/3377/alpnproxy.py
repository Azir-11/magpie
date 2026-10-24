"""A CONNECT proxy that records what a TLS client offers before it tunnels.

Logs, per connection: the CONNECT target, the ClientHello's SNI and ALPN
list, and the byte counts each way. It never decrypts.
"""
import os
import socket
import struct
import sys
import threading
import time

LOG = open(os.environ.get("ALPN_LOG", "/tmp/alpn.log"), "a", buffering=1)


def log(msg):
    LOG.write("%s %s\n" % (time.strftime("%H:%M:%S"), msg))


def read_client_hello(sock):
    """Return (sni, alpns, raw) for the first record, or None."""
    head = sock.recv(5, socket.MSG_WAITALL)
    if len(head) < 5 or head[0] != 0x16:
        return None
    length = struct.unpack(">H", head[3:5])[0]
    body = b""
    while len(body) < length:
        chunk = sock.recv(length - len(body))
        if not chunk:
            return None
        body += chunk
    raw = head + body
    if len(body) < 4 or body[0] != 0x01:
        return None
    p = 4  # handshake type + 3-byte length
    p += 2 + 32  # version + random
    sid = body[p]
    p += 1 + sid
    cs = struct.unpack(">H", body[p:p + 2])[0]
    p += 2 + cs
    comp = body[p]
    p += 1 + comp
    if p + 2 > len(body):
        return None
    ext_len = struct.unpack(">H", body[p:p + 2])[0]
    p += 2
    end = min(p + ext_len, len(body))
    sni, alpns = "", []
    while p + 4 <= end:
        etype, elen = struct.unpack(">HH", body[p:p + 4])
        p += 4
        data = body[p:p + elen]
        p += elen
        if etype == 0:  # server_name
            q = 2
            while q + 3 <= len(data):
                ntype = data[q]
                nlen = struct.unpack(">H", data[q + 1:q + 3])[0]
                name = data[q + 3:q + 3 + nlen].decode("latin1")
                if ntype == 0:
                    sni = name
                q += 3 + nlen
        elif etype == 16:  # ALPN
            q = 2
            while q < len(data):
                n = data[q]
                alpns.append(data[q + 1:q + 1 + n].decode("latin1"))
                q += 1 + n
    return sni, alpns, raw


def pump(src, dst, counter, key):
    try:
        while True:
            b = src.recv(65536)
            if not b:
                break
            counter[key] = counter.get(key, 0) + len(b)
            dst.sendall(b)
    except OSError:
        pass
    finally:
        try:
            dst.shutdown(socket.SHUT_WR)
        except OSError:
            pass


def handle(client):
    try:
        head = b""
        while b"\r\n\r\n" not in head:
            b = client.recv(4096)
            if not b:
                return
            head += b
        line = head.split(b"\r\n", 1)[0].decode("latin1")
        log("REQ " + line)
        if not line.startswith("CONNECT "):
            client.sendall(b"HTTP/1.1 405 Method Not Allowed\r\n\r\n")
            return
        hostport = line.split()[1]
        host, _, port = hostport.rpartition(":")
        up = socket.create_connection((host, int(port)), timeout=20)
        client.sendall(b"HTTP/1.1 200 Connection Established\r\n\r\n")
        hello = read_client_hello(client)
        if hello:
            sni, alpns, raw = hello
            log("TLS sni=%s alpn=%s hello=%dB" % (sni or "-", alpns or "(none)", len(raw)))
            up.sendall(raw)
        else:
            log("TLS (no ClientHello read)")
        counts = {}
        t1 = threading.Thread(target=pump, args=(client, up, counts, "up"), daemon=True)
        t2 = threading.Thread(target=pump, args=(up, client, counts, "down"), daemon=True)
        t1.start()
        t2.start()
        t1.join()
        t2.join()
        log("CLOSED %s up=%dB down=%dB" % (hostport, counts.get("up", 0), counts.get("down", 0)))
    except Exception as e:  # a diagnostic, it logs and moves on
        log("ERR %r" % (e,))
    finally:
        client.close()


def main():
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 8899
    s = socket.socket()
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    s.bind(("127.0.0.1", port))
    s.listen(32)
    log("listening %d" % port)
    while True:
        c, _ = s.accept()
        threading.Thread(target=handle, args=(c,), daemon=True).start()


main()
