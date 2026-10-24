# Captures for #3377

Two local captures taken while investigating "direct works, magpie doesn't" on the
same host, same account, same model. Neither touches the live gateway: Codex ran
with a sandbox `CODEX_HOME` and its `openai_base_url` (or `CHATGPT_BASE_URL`)
pointed at a throwaway loopback listener.

Client: codex-cli 0.162.1 on macOS 27.0.0 (arm64). No magpie build was in the
path of either capture: the first went to `chatgpt.com` directly, the second to a
stand-in that answers the way magpie does. The fix under discussion is on the
`fix/codex-stream-cut-mid-reply` branch in the fork, commit `bc448bf`.

Context: yetone/magpie#3377.

## 1. `alpn-proxy.log` — what a *direct* Codex offers on the wire

`alpnproxy.py` is a CONNECT proxy that reads the ClientHello before tunnelling.
It logs the SNI, the ALPN list, and per-connection byte counts; it never decrypts.

Codex was pointed at `chatgpt.com` directly with `HTTPS_PROXY=127.0.0.1:8899`, so
every TLS connection of the conversation went through it.

Result — 13/13 connections to `chatgpt.com:443`, `alpn=(none)`:

```
00:16:17 TLS sni=chatgpt.com alpn=(none) hello=171B
00:16:20 TLS sni=chatgpt.com alpn=(none) hello=1468B
00:16:24 TLS sni=chatgpt.com alpn=(none) hello=171B
00:16:28 TLS sni=chatgpt.com alpn=(none) hello=171B
00:16:32 CLOSED chatgpt.com:443 up=39803B down=8317451B
00:16:32 CLOSED chatgpt.com:443 up=4565B down=128385B
```

(at 00:16:17 that line appears eight times in the log, verbatim)

and the same for `ab.chatgpt.com` and `*.oaiusercontent.com`.

An empty ALPN extension means the client asked for neither `h2` nor `http/1.1`
and the connection settles on HTTP/1.1. So a direct Codex does **not** speak h2
to this backend: the HTTP version is not what separates the two paths, and the
`#1700` decision (empty ALPN + HTTP/1.1 upstream) needs no revisit. The
discriminator is the WebSocket upgrade, which the second capture shows.

The direct turn in the same run completed, with the long download above
(`down=8317451B`) on one of the `chatgpt.com` connections.

## 2. `upgrade-426.log` — what Codex does when the upgrade is refused

`up426.py` answers every `Upgrade: websocket` with `426 Upgrade Required` — a
stand-in for a gateway that speaks HTTP only — and logs what arrives next.

```
00:17:28 GET /backend-api/codex/models?client_version=0.162.1 upgrade='' accept='*/*' len=0
00:17:30 GET /backend-api/codex/responses upgrade='websocket' accept='' len=0
00:17:32 POST /backend-api/codex/responses upgrade='' accept='text/event-stream' len=28618
00:17:33 POST /backend-api/codex/responses upgrade='' accept='text/event-stream' len=28621
      ... 4 more POST retries, same body
```

One upgrade attempt, then the turn is re-sent as an SSE `POST` with the whole
request body — no second WS attempt, no negotiation. That is the path magpie puts
Codex on today, and the path the backend cuts ~120 s in.

The `len` values are the request body of the POST (the `response.create` JSON).
In the upgrade case `len=0` because the body is the first WS frame, not a body.

The stand-in reproduces magpie's own answer, so this is what production does, not
only what a mock does: `internal/gateway/codex_backend.go` replies 426 "magpie
speaks HTTP" on the `/backend-api/codex` path (pinned by
`TestCodexWebSocketUpgradeRequired`), and `responsesOverHTTP` in
`internal/gateway/gateway.go` answers the same 426 on `/v1/responses` (added in
a87dc1ff, #1005). A client that gets either falls back to SSE within seconds —
which is the leg the backend cuts at 121–123 s.

## Reproducing

```sh
python3 alpnproxy.py 8899          # capture 1
HTTPS_PROXY=127.0.0.1:8899 codex ... # with a sandbox CODEX_HOME

python3 up426.py 9998              # capture 2
# CODEX_HOME with openai_base_url = http://127.0.0.1:9998
codex exec 'hi' </dev/null
```

Both scripts append to `/tmp/alpn.log` and `/tmp/up426.log` unless `ALPN_LOG` /
`UPGRADE_LOG` say otherwise.

## Caveats, stated rather than hidden

- The ALPN capture runs through a CONNECT proxy. The proxy reads and forwards the
  ClientHello unchanged, so the ALPN list is the client's own — but the probe
  itself is a local Python listener, not a packet capture.
- The 426 run is plain HTTP to loopback, so Codex dials `ws://`, not `wss://`.
  The WebSocket-vs-SSE choice and the fallback are the same either way; only the
  transport underneath differs.
- Only `chatgpt.com` and the subdomains it uses (`ab.chatgpt.com`,
  `*.oaiusercontent.com`) went through the proxy in capture 1. Anything else Codex
  dialled is not in the log.
