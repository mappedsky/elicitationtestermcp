package mcpserver

import (
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// HTTPConfig describes the endpoints one listener exposes.
type HTTPConfig struct {
	// Path serves a stateless handler. Stateless is what the transport
	// requires for protocol version 2026-07-28, which is the revision that
	// carries elicitations in results.
	Path string
	// LegacyPath serves a stateful handler, which caps at 2025-11-25 and is
	// therefore the only endpoint that can send an elicitation/create
	// request. Empty disables it.
	LegacyPath string
	// AuthorizePath serves the stand-in authorization page that the URL
	// scenarios point at. Empty disables it.
	AuthorizePath string
}

// DefaultHTTPConfig returns the endpoint layout the flags default to.
func DefaultHTTPConfig() HTTPConfig {
	return HTTPConfig{Path: "/mcp", LegacyPath: "/mcp/legacy", AuthorizePath: "/authorize"}
}

// NewHTTPHandler serves the MCP server over streamable HTTP.
//
// Two endpoints, because one cannot do both jobs: the transport only allows
// the 2026-07-28 revision when it is stateless, and that revision forbids the
// server-initiated elicitation requests the older one is there to exercise.
func NewHTTPHandler(server *mcp.Server, cfg HTTPConfig) (http.Handler, error) {
	if server == nil {
		return nil, fmt.Errorf("MCP server must not be nil")
	}
	for name, path := range map[string]string{"path": cfg.Path, "legacy path": cfg.LegacyPath, "authorize path": cfg.AuthorizePath} {
		if path == "" && name != "path" {
			continue
		}
		if path == "" || !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "?#{} \t\r\n") {
			return nil, fmt.Errorf("HTTP %s must be an absolute path without a query or fragment, got %q", name, path)
		}
	}
	if cfg.LegacyPath == cfg.Path || (cfg.AuthorizePath != "" && cfg.AuthorizePath == cfg.Path) {
		return nil, fmt.Errorf("HTTP paths must differ")
	}

	mux := http.NewServeMux()
	mount := func(path string, stateless bool) {
		streamable := mcp.NewStreamableHTTPHandler(
			func(*http.Request) *mcp.Server { return server },
			&mcp.StreamableHTTPOptions{Stateless: stateless, PropagateRequestCancellation: true},
		)
		// Reject unsafe cross-origin browser requests. The MCP SDK separately
		// keeps its default localhost DNS-rebinding protection enabled.
		mux.Handle(path, http.NewCrossOriginProtection().Handler(streamable))
	}
	mount(cfg.Path, true)
	if cfg.LegacyPath != "" {
		mount(cfg.LegacyPath, false)
	}
	if cfg.AuthorizePath != "" {
		mux.HandleFunc(cfg.AuthorizePath, authorizePage)
	}
	return mux, nil
}

// authorizePage stands in for the page a URL elicitation sends the owner to.
// It grants nothing and completes nothing; it exists so the link a client
// rendered can be opened and read back, which is the only way to see what the
// client did to the URL between receiving it and showing it.
func authorizePage(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	var rows strings.Builder
	query := req.URL.Query()
	keys := make([]string, 0, len(query))
	for key := range query {
		keys = append(keys, key)
	}
	for _, key := range sortStrings(keys) {
		fmt.Fprintf(&rows, "<tr><th>%s</th><td>%s</td></tr>",
			html.EscapeString(key), html.EscapeString(strings.Join(query[key], ", ")))
	}
	if rows.Len() == 0 {
		rows.WriteString("<tr><td colspan=\"2\">No query parameters.</td></tr>")
	}
	fmt.Fprintf(w, authorizeHTML, html.EscapeString(requestURL(req)), rows.String())
}

func requestURL(req *http.Request) string {
	scheme := "http"
	if req.TLS != nil {
		scheme = "https"
	}
	target := url.URL{Scheme: scheme, Host: req.Host, Path: req.URL.Path, RawQuery: req.URL.RawQuery}
	return target.String()
}

func sortStrings(values []string) []string {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
	return values
}

const authorizeHTML = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Elicitation tester</title>
<style>
body{font:16px/1.5 system-ui,sans-serif;margin:0;padding:2rem;background:#0a0f22;color:#e8ecf7}
main{max-width:44rem;margin:0 auto}
h1{font-size:1.4rem;margin:0 0 .5rem}
p{color:#9fb0d0}
code{word-break:break-all;background:#151d38;padding:.15rem .35rem;border-radius:.25rem}
table{border-collapse:collapse;width:100%%;margin-top:1.5rem}
th,td{text-align:left;padding:.4rem .6rem;border-bottom:1px solid #222d4f;vertical-align:top}
th{color:#8fb4ff;width:12rem;font-weight:600}
</style></head>
<body><main>
<h1>Elicitation tester</h1>
<p>This page grants nothing. It is where the URL elicitation scenarios point,
so that the link a client rendered can be opened and compared with the one the
server sent.</p>
<p>Arrived as:</p><p><code>%s</code></p>
<table>%s</table>
</main></body></html>
`
