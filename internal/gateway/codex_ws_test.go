package gateway

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

// wsDial asks the gateway for a turn over a WebSocket, as Codex 0.163 does,
// and returns the connection and the status it was answered with.
func wsDial(t *testing.T, srv *httptest.Server, headers map[string]string) (net.Conn, *bufio.Reader, int) {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	var b strings.Builder
	b.WriteString("GET /backend-api/codex/responses HTTP/1.1\r\n")
	b.WriteString("Host: " + u.Host + "\r\n")
	b.WriteString("Connection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\n")
	b.WriteString("Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n")
	for k, v := range headers {
		b.WriteString(k + ": " + v + "\r\n")
	}
	b.WriteString("\r\n")
	if _, err := c.Write([]byte(b.String())); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	res, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("reading the answer: %v", err)
	}
	return c, br, res.StatusCode
}

// wsBackend stands in for the ChatGPT backend's socket: it answers the
// upgrade and echoes what comes through it.
func wsBackend(t *testing.T, seen func(*http.Request)) {
	t.Helper()
	chatgpt(t, func(w http.ResponseWriter, r *http.Request) {
		seen(r)
		conn, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijacking: %v", err)
			return
		}
		defer conn.Close()
		brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		brw.Flush()
		msg := make([]byte, 4)
		if _, err := io.ReadFull(brw, msg); err != nil {
			return
		}
		brw.WriteString("pong")
		brw.Flush()
	})
}

// A turn of Codex's own model, on its own sign-in, goes on over the socket
// Codex asked for: the backend closes an SSE turn of the same two minutes in,
// losing a longer reply.
func TestCodexOwnTurnGoesOnOverWebSocket(t *testing.T) {
	type seen struct{ path, meta string }
	asked := make(chan seen, 1)
	wsBackend(t, func(r *http.Request) {
		asked <- seen{r.URL.Path, r.Header.Get("x-codex-turn-metadata")}
	})
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	c, br, status := wsDial(t, srv, map[string]string{
		"Authorization":         "Bearer chatgpt-token",
		"chatgpt-account-id":    "acct-1",
		"x-codex-turn-metadata": `{"model":"gpt-6-astra","request_kind":"turn"}`,
	})
	if status != http.StatusSwitchingProtocols {
		t.Fatalf("answered %d, want 101", status)
	}
	at := <-asked
	if at.path != "/backend-api/codex/responses" {
		t.Errorf("backend asked at %q", at.path)
	}
	if !strings.Contains(at.meta, "gpt-6-astra") {
		t.Errorf("backend got metadata %q", at.meta)
	}
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(br, got); err != nil {
		t.Fatalf("reading the echo: %v", err)
	}
	if string(got) != "pong" {
		t.Errorf("echoed %q", got)
	}
}

// What magpie does to a turn it can't do to a copied socket keeps the 426:
// a model of magpie's, an account named, and a turn whose model is unknown.
func TestWebSocketStaysRefusedWhereMagpieServesTheTurn(t *testing.T) {
	for _, c := range []struct {
		name    string
		headers map[string]string
	}{
		{"model of magpie's", map[string]string{"x-codex-turn-metadata": `{"model":"anthropic/claude-sonnet-4.5"}`}},
		{"account named", map[string]string{"x-codex-turn-metadata": `{"model":"gpt-6-astra"}`, AccountHeader: "someone"}},
		{"model unknown", map[string]string{"Authorization": "Bearer chatgpt-token"}},
		{"api key", map[string]string{"x-codex-turn-metadata": `{"model":"gpt-6-astra"}`, "Authorization": "Bearer sk-test"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var hit atomic.Bool
			wsBackend(t, func(*http.Request) { hit.Store(true) })
			srv := httptest.NewServer(New().Handler())
			defer srv.Close()
			_, _, status := wsDial(t, srv, c.headers)
			if status != http.StatusUpgradeRequired {
				t.Fatalf("answered %d, want 426", status)
			}
			if hit.Load() {
				t.Error("the backend was asked for a turn magpie serves")
			}
		})
	}
}

// A turn a second ChatGPT account of magpie's is on for keeps the 426: the
// accounts are magpie's to pick between, and a socket copied as it came
// would spend the sign-in's alone (codexAccounts).
func TestWebSocketRefusedWithAnotherCodexAccountOn(t *testing.T) {
	codexSignedIn(t, "spare")
	var hit atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.Store(true)
	}))
	defer up.Close()
	was := provider.CodexBase
	provider.CodexBase = up.URL + "/backend-api/codex"
	defer func() { provider.CodexBase = was }()
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	_, _, status := wsDial(t, srv, map[string]string{
		"Authorization":         "Bearer chatgpt-token",
		"chatgpt-account-id":    "acct-1",
		"x-codex-turn-metadata": `{"model":"gpt-5.5"}`,
	})
	if status != http.StatusUpgradeRequired {
		t.Fatalf("answered %d, want 426", status)
	}
	if hit.Load() {
		t.Error("the backend was asked for a turn magpie's accounts route")
	}
}
