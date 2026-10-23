package gateway

import (
	"bufio"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/usage"
)

// codexWSTunnel hands a turn Codex asked for over a WebSocket to the ChatGPT
// backend as it came, and says whether it did; false leaves the answer to
// codexBackend, which tells Codex 426 and Codex asks over plain HTTP instead.
//
// Codex 0.163 opens a turn as a WebSocket, and takes 426 for "speak HTTP":
// the turn then streams as SSE, which the ChatGPT backend now ends about two
// minutes in, whatever the body, headers or tier (2026-10-23: a turn that ran
// 168s and one that ran 198s the day before, every one of them cut at 121–123s
// since), so a longer reply is lost and a compaction never finishes. magpie
// can't hold the SSE open where the backend closes it; what it can do is let
// the socket through.
//
// Only a turn of Codex's own model, on Codex's own sign-in, goes through: one
// for a model of magpie's (an id with a "/") is served where magpie routes it
// and keeps the 426, as does one for an account of magpie's to pick (#882,
// #905) and one a subagent's model is set to move. Those are things magpie
// does to a turn, and a socket it only copies can't. A relayed turn is
// recorded as the call it was; its frames aren't read, so nothing of it
// reaches the usage sniffer, the request hooks or the archive.
func (s *Server) codexWSTunnel(w http.ResponseWriter, r *http.Request, rest string) bool {
	if rest != "/responses" || apiKey(r.Header) || r.Header.Get(AccountHeader) != "" {
		return false
	}
	model := codexWSModel(r.Header)
	if model == "" || strings.Contains(model, "/") {
		return false
	}
	// one of Codex's own models the user disabled (#1867) is refused by
	// codexBackend, as it is over HTTP
	if _, off := codexNativeOff(model); off {
		return false
	}
	// an account of magpie's to route it to, or choose between: the relay
	// below asks nothing of the key, and the accounts would go unused
	if _, ok := codexAccounts(r, model); ok {
		return false
	}
	// a subagent put on the model set for Codex's subagents (codexSubagentPick)
	if pick := codexSubagentPick(r, agentOf(r), requestCallKind(r.Header, requestSessionMetadata(r.Header, nil)), model, nil); pick.Moved() {
		return false
	}
	up, err := url.Parse(provider.CodexBase + rest)
	if err != nil {
		return false
	}
	up.RawQuery = r.URL.RawQuery
	start := time.Now()
	usage.Saw(agentOf(r))
	ctx := provider.ViaSignedIn(r.Context(), "codex")
	tw := &tunnelWriter{ResponseWriter: w}
	(&httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out = pr.Out.WithContext(ctx)
			// SetURL would join the backend's path to the one asked for,
			// which is the same path magpie was asked at
			pr.Out.URL.Scheme, pr.Out.URL.Host = up.Scheme, up.Host
			pr.Out.URL.Path, pr.Out.URL.RawPath = up.Path, ""
			pr.Out.URL.RawQuery = up.RawQuery
			pr.Out.Host = ""
		},
		Transport:     s.client.Transport,
		FlushInterval: -1,
	}).ServeHTTP(tw, r)
	status := tw.status
	if status == 0 {
		status = http.StatusBadGateway
	}
	s.record(Call{Time: start, Agent: agentOf(r), Model: model, Provider: "openai",
		From: provider.Responses, To: provider.Responses, Kind: callKind(r.Header),
		Status: status, Millis: time.Since(start).Milliseconds()})
	return true
}

// codexWSModel is the model a turn's WebSocket asks for, "" when it says
// none. Codex sends it in x-codex-turn-metadata before the socket opens (the
// body naming it comes as a frame after), and repeats it as
// x-codex-routing-hint.
func codexWSModel(h http.Header) string {
	if m := turnMetadataModel(h.Get("x-codex-turn-metadata")); m != "" {
		return m
	}
	for _, part := range strings.Split(h.Get("x-codex-routing-hint"), ";") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(part), "model="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// turnMetadataModel is the model x-codex-turn-metadata names, "" when it
// says none or can't be read.
func turnMetadataModel(meta string) string {
	if meta == "" {
		return ""
	}
	var m struct {
		Model string `json:"model"`
	}
	if json.Unmarshal([]byte(meta), &m) != nil {
		return ""
	}
	return strings.TrimSpace(m.Model)
}

// tunnelWriter is the ResponseWriter a tunneled turn is written through: it
// records the status the turn got, and hijacks the connection for the
// backend's 101 as the proxy asks it to.
type tunnelWriter struct {
	http.ResponseWriter
	status int
}

func (t *tunnelWriter) WriteHeader(code int) {
	if t.status == 0 {
		t.status = code
	}
	t.ResponseWriter.WriteHeader(code)
}

func (t *tunnelWriter) Write(b []byte) (int, error) {
	if t.status == 0 {
		t.status = http.StatusOK
	}
	return t.ResponseWriter.Write(b)
}

func (t *tunnelWriter) Flush() {
	_ = http.NewResponseController(t.ResponseWriter).Flush()
}

func (t *tunnelWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	t.status = http.StatusSwitchingProtocols
	return http.NewResponseController(t.ResponseWriter).Hijack()
}
