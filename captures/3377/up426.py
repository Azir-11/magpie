"""Answers every WebSocket upgrade with 426, logs what the client asks for.

Stands in for a gateway that speaks HTTP only, to watch what Codex does
next: it should fall back to an SSE POST.
"""
import os
import socket
import sys
import threading
import time

LOG = open(os.environ.get("UPGRADE_LOG", "/tmp/up426.log"), "a", buffering=1)


def log(msg):
    LOG.write("%s %s\n" % (time.strftime("%H:%M:%S"), msg))


def serve(client):
    try:
        data = b""
        while b"\r\n\r\n" not in data:
            b = client.recv(65536)
            if not b:
                return
            data += b
        head, rest = data.split(b"\r\n\r\n", 1)
        lines = head.decode("latin1").split("\r\n")
        method, path, _ = lines[0].split(" ", 2)
        headers = {}
        for line in lines[1:]:
            k, _, v = line.partition(":")
            headers[k.strip().lower()] = v.strip()
        upgrade = headers.get("upgrade", "")
        log("%s %s upgrade=%r accept=%r len=%d" % (
            method, path, upgrade, headers.get("accept", ""), len(rest)))
        if upgrade.lower() == "websocket":
            body = b"magpie speaks HTTP\n"
            client.sendall(b"HTTP/1.1 426 Upgrade Required\r\nContent-Length: %d\r\n\r\n" % len(body) + body)
        elif method == "GET" and path.endswith("/models"):
            body = b'{"models":[]}'
            client.sendall(b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n" % len(body) + body)
        else:
            body = b'{"error":"not here"}'
            client.sendall(b"HTTP/1.1 404 Not Found\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n" % len(body) + body)
    except Exception as e:
        log("ERR %r" % (e,))
    finally:
        client.close()


def main():
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 9998
    s = socket.socket()
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    s.bind(("127.0.0.1", port))
    s.listen(16)
    log("listening %d" % port)
    while True:
        c, _ = s.accept()
        threading.Thread(target=serve, args=(c,), daemon=True).start()


main()
