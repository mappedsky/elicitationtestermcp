package mcpserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestNewHTTPHandlerRejectsUnusablePaths(t *testing.T) {
	server, err := New(testConfig())
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	for name, cfg := range map[string]HTTPConfig{
		"empty path":      {Path: ""},
		"relative path":   {Path: "mcp"},
		"path with query": {Path: "/mcp?x=1"},
		"legacy relative": {Path: "/mcp", LegacyPath: "legacy"},
		"colliding paths": {Path: "/mcp", LegacyPath: "/mcp"},
		"authorize clash": {Path: "/mcp", AuthorizePath: "/mcp"},
	} {
		if _, err := NewHTTPHandler(server, cfg); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := NewHTTPHandler(nil, DefaultHTTPConfig()); err == nil {
		t.Error("a nil server was accepted")
	}
}

func TestAuthorizePageEchoesTheURLItWasReachedAt(t *testing.T) {
	handler := newTestHandler(t)
	server := httptest.NewServer(handler)
	defer server.Close()

	res, err := http.Get(server.URL + "/authorize?scenario=url%2Fallowed&x=%3Cscript%3E")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer res.Body.Close()
	body := readAll(t, res)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("got status %d", res.StatusCode)
	}
	if !strings.Contains(body, "url/allowed") {
		t.Errorf("the page did not echo the query it was reached with")
	}
	// The page reflects whatever a client put in the URL, so it escapes it.
	if strings.Contains(body, "<script>") {
		t.Errorf("the page reflected markup unescaped")
	}
}

func TestAuthorizePageRefusesNonGET(t *testing.T) {
	server := httptest.NewServer(newTestHandler(t))
	defer server.Close()

	res, err := http.Post(server.URL+"/authorize", "text/plain", strings.NewReader(""))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("got status %d, want 405", res.StatusCode)
	}
}

// TestLegacyEndpointSendsElicitationRequests is the reason there are two
// endpoints. The stateless one serves the current revision, which forbids
// server-initiated elicitation; the stateful one caps below it, which is the
// only way left to exercise the elicitation/create path a client still has to
// support.
func TestLegacyEndpointSendsElicitationRequests(t *testing.T) {
	server := httptest.NewServer(newTestHandler(t))
	defer server.Close()

	answers := &recorder{action: "accept", content: map[string]any{"token": "abcdefgh"}}
	client := mcp.NewClient(&mcp.Implementation{Name: "legacy-test-client", Version: "0"}, &mcp.ClientOptions{
		ElicitationHandler: answers.handle,
		Capabilities: &mcp.ClientCapabilities{
			Elicitation: &mcp.ElicitationCapabilities{
				Form: &mcp.FormElicitationCapabilities{},
				URL:  &mcp.URLElicitationCapabilities{},
			},
		},
	})
	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp/legacy"}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer session.Close()

	info := session.InitializeResult()
	if info.ProtocolVersion >= protocolVersion20260728 {
		t.Fatalf("the stateful endpoint negotiated %s; it is supposed to cap below that", info.ProtocolVersion)
	}

	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "run_scenario",
		Arguments: map[string]any{"scenario": "form/minimal", "delivery": "direct"},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if result.IsError {
		t.Fatalf("direct delivery failed on the legacy endpoint: %s", renderContent(result))
	}
	if answers.count() != 1 {
		t.Fatalf("client saw %d elicitation requests, want 1", answers.count())
	}
	out := decode[RunOutput](t, result)
	if out.Delivery != "direct" || out.Status != "complete" {
		t.Fatalf("got %+v", out)
	}
}

func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	server, err := New(testConfig())
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	handler, err := NewHTTPHandler(server, DefaultHTTPConfig())
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	return handler
}

func readAll(t *testing.T, res *http.Response) string {
	t.Helper()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := res.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return b.String()
}
