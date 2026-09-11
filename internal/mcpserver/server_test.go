package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// recorder is a client that answers every elicitation the same way and counts
// how many it saw, which is how the round-trip tests observe the server.
type recorder struct {
	mu      sync.Mutex
	seen    []*mcp.ElicitParams
	action  string
	content map[string]any
	err     error
}

func (r *recorder) handle(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, req.Params)
	if r.err != nil {
		return nil, r.err
	}
	if r.action != "accept" {
		return &mcp.ElicitResult{Action: r.action}, nil
	}
	// The SDK validates the answer against the schema before handing it back,
	// so a fixed map would only answer the one form it was written for.
	return &mcp.ElicitResult{Action: r.action, Content: fillSchema(req.Params.RequestedSchema, r.content)}, nil
}

// fillSchema invents an answer that satisfies schema, letting overrides pin
// the values a test wants to recognize later.
func fillSchema(schema any, overrides map[string]any) map[string]any {
	content := map[string]any{}
	encoded, err := json.Marshal(schema)
	if err != nil {
		return content
	}
	var decoded struct {
		Properties map[string]struct {
			Type      string  `json:"type"`
			Enum      []any   `json:"enum"`
			Default   any     `json:"default"`
			MinLength int     `json:"minLength"`
			MaxLength int     `json:"maxLength"`
			Minimum   float64 `json:"minimum"`
			Maximum   float64 `json:"maximum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return content
	}
	for name, field := range decoded.Properties {
		switch {
		case field.Default != nil:
			content[name] = field.Default
		case len(field.Enum) > 0:
			content[name] = field.Enum[0]
		case field.Type == "boolean":
			content[name] = true
		case field.Type == "integer" || field.Type == "number":
			value := 1.0
			if field.Maximum != 0 && value > field.Maximum {
				value = field.Maximum
			}
			if value < field.Minimum {
				value = field.Minimum
			}
			content[name] = value
		default:
			length := max(field.MinLength, 8)
			if field.MaxLength > 0 && length > field.MaxLength {
				length = field.MaxLength
			}
			content[name] = strings.Repeat("a", length)
		}
		if override, ok := overrides[name]; ok {
			content[name] = override
		}
	}
	return content
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.seen)
}

type harness struct {
	client   *mcp.ClientSession
	recorder *recorder
}

func connect(t *testing.T, answers *recorder, opts *mcp.ClientOptions) *harness {
	t.Helper()
	server, err := New(testConfig())
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if opts == nil {
		opts = &mcp.ClientOptions{}
	}
	opts.ElicitationHandler = answers.handle
	opts.Capabilities = &mcp.ClientCapabilities{
		Elicitation: &mcp.ElicitationCapabilities{
			Form: &mcp.FormElicitationCapabilities{},
			URL:  &mcp.URLElicitationCapabilities{},
		},
	}

	ctx := t.Context()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("connect server: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, opts).
		Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })
	return &harness{client: clientSession, recorder: answers}
}

func (h *harness) call(t *testing.T, name string, args any) *mcp.CallToolResult {
	t.Helper()
	result, err := h.client.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	if result.IsError {
		t.Fatalf("call %s returned an error result: %s", name, renderContent(result))
	}
	return result
}

func decode[T any](t *testing.T, result *mcp.CallToolResult) T {
	t.Helper()
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("re-encode structured content: %v", err)
	}
	var out T
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatalf("decode structured content %s: %v", encoded, err)
	}
	return out
}

func renderContent(result *mcp.CallToolResult) string {
	var parts []string
	for _, item := range result.Content {
		if text, ok := item.(*mcp.TextContent); ok {
			parts = append(parts, text.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func TestDeferredRoundTripCompletes(t *testing.T) {
	answers := &recorder{action: "accept", content: map[string]any{"token": "abcdefgh"}}
	h := connect(t, answers, nil)

	out := decode[RunOutput](t, h.call(t, "run_scenario", map[string]any{"scenario": "form/minimal"}))
	if out.Status != "complete" || out.Delivery != "deferred" {
		t.Fatalf("got status %q delivery %q", out.Status, out.Delivery)
	}
	if answers.count() != 1 {
		t.Fatalf("client saw %d elicitations, want 1", answers.count())
	}
	if len(out.Responses) != 1 || out.Responses[0].Action != "accept" {
		t.Fatalf("got responses %+v", out.Responses)
	}
	fields := out.Responses[0].Fields
	if len(fields) != 1 || fields[0].Name != "token" || fields[0].JSONType != "string" {
		t.Fatalf("got fields %+v", fields)
	}
	if fields[0].Digest == "" || fields[0].Chars == 0 {
		t.Fatalf("got field summary %+v, want a digest and a length", fields[0])
	}
	// The summary must not carry the value itself; that is what the log is for.
	if encoded, _ := json.Marshal(out); strings.Contains(string(encoded), "abcdefgh") {
		t.Errorf("run_scenario echoed the submitted value: %s", encoded)
	}
}

func TestMultiRoundScenarioElicitsOncePerRound(t *testing.T) {
	answers := &recorder{action: "accept", content: map[string]any{"token": "abcdefgh"}}
	h := connect(t, answers, nil)

	out := decode[RunOutput](t, h.call(t, "run_scenario", map[string]any{"scenario": "rounds/two"}))
	if out.Rounds != 2 || out.Round != 2 || out.Status != "complete" {
		t.Fatalf("got %+v", out)
	}
	if answers.count() != 2 {
		t.Fatalf("client saw %d elicitations, want 2", answers.count())
	}
}

func TestDeclineIsReportedRatherThanRetried(t *testing.T) {
	answers := &recorder{action: "decline"}
	h := connect(t, answers, nil)

	out := decode[RunOutput](t, h.call(t, "run_scenario", map[string]any{"scenario": "form/minimal"}))
	if len(out.Responses) != 1 || out.Responses[0].Action != "decline" {
		t.Fatalf("got responses %+v", out.Responses)
	}
	if len(out.Responses[0].Fields) != 0 {
		t.Errorf("a declined elicitation carried fields: %+v", out.Responses[0].Fields)
	}
}

func TestExchangeLogHoldsTheValueTheSummaryWithholds(t *testing.T) {
	answers := &recorder{action: "accept", content: map[string]any{"token": "abcdefgh"}}
	h := connect(t, answers, nil)
	h.call(t, "run_scenario", map[string]any{"scenario": "form/minimal"})

	withheld := decode[LogOutput](t, h.call(t, "exchange_log", map[string]any{"limit": 10}))
	if encoded, _ := json.Marshal(withheld); strings.Contains(string(encoded), "abcdefgh") {
		t.Errorf("the log returned the value without being asked: %s", encoded)
	}
	shown := decode[LogOutput](t, h.call(t, "exchange_log", map[string]any{"limit": 10, "include_values": true}))
	if encoded, _ := json.Marshal(shown); !strings.Contains(string(encoded), "abcdefgh") {
		t.Errorf("include_values did not return the value: %s", encoded)
	}

	// The sent side is recorded too, so a client that alters a URL or a
	// schema between receiving and rendering it can be caught.
	var sawSchema bool
	for _, entry := range shown.Entries {
		for _, request := range entry.Requested {
			if strings.Contains(request.Schema, `"token"`) {
				sawSchema = true
			}
		}
	}
	if !sawSchema {
		t.Errorf("the log did not record the schema that was sent: %+v", shown.Entries)
	}

	discarded := decode[ResetOutput](t, h.call(t, "reset_log", map[string]any{}))
	if discarded.Discarded == 0 {
		t.Errorf("reset_log discarded nothing")
	}
}

func TestProtocolErrorScenarioReachesTheClientAsAnError(t *testing.T) {
	h := connect(t, &recorder{action: "cancel"}, nil)
	_, err := h.client.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "run_scenario",
		Arguments: map[string]any{"scenario": "error/url_required"},
	})
	var wireErr *jsonrpc.Error
	if !errors.As(err, &wireErr) {
		t.Fatalf("got %v, want a JSON-RPC error", err)
	}
	if wireErr.Code != mcp.CodeURLElicitationRequired {
		t.Fatalf("got code %d, want %d", wireErr.Code, mcp.CodeURLElicitationRequired)
	}
	var data struct {
		Elicitations []struct {
			Mode          string `json:"mode"`
			URL           string `json:"url"`
			ElicitationID string `json:"elicitationId"`
		} `json:"elicitations"`
	}
	if err := json.Unmarshal(wireErr.Data, &data); err != nil {
		t.Fatalf("decode error data: %v", err)
	}
	if len(data.Elicitations) != 1 || data.Elicitations[0].Mode != "url" {
		t.Fatalf("got elicitations %+v", data.Elicitations)
	}
	if !strings.HasPrefix(data.Elicitations[0].URL, testConfig().AllowedURL) {
		t.Errorf("got URL %q, want one on the configured origin", data.Elicitations[0].URL)
	}
}

func TestTamperedRequestStateIsRejected(t *testing.T) {
	// Without the automatic retry middleware the input-required result is
	// visible, which is what lets the retry be forged by hand.
	h := connect(t, &recorder{action: "accept"}, &mcp.ClientOptions{
		MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true},
	})
	first := h.call(t, "run_scenario", map[string]any{"scenario": "form/minimal"})
	if !first.NeedsInput() || first.RequestState == "" {
		t.Fatalf("first call did not ask for input: %+v", first)
	}

	forged := first.RequestState[:len(first.RequestState)-2] + "xy"
	result, err := h.client.CallTool(t.Context(), &mcp.CallToolParams{
		Name:         "run_scenario",
		Arguments:    map[string]any{"scenario": "form/minimal"},
		RequestState: forged,
		InputResponses: mcp.InputResponseMap{
			"q1": &mcp.ElicitResult{Action: "accept", Content: map[string]any{"token": "abcdefgh"}},
		},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !result.IsError || !strings.Contains(renderContent(result), "signature") {
		t.Fatalf("a forged request state was accepted: %+v", result)
	}
}

func TestRequestStateIsBoundToItsScenario(t *testing.T) {
	h := connect(t, &recorder{action: "accept"}, &mcp.ClientOptions{
		MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true},
	})
	first := h.call(t, "run_scenario", map[string]any{"scenario": "form/minimal"})

	// The same signed state, replayed against a different scenario: the
	// arguments differ, so it must not verify.
	result, err := h.client.CallTool(t.Context(), &mcp.CallToolParams{
		Name:         "run_scenario",
		Arguments:    map[string]any{"scenario": "form/types"},
		RequestState: first.RequestState,
		InputResponses: mcp.InputResponseMap{
			"q1": &mcp.ElicitResult{Action: "accept", Content: map[string]any{"token": "abcdefgh"}},
		},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !result.IsError {
		t.Fatalf("a state issued for another scenario was accepted: %+v", result)
	}
}

func TestSamplingRequestReachesAClientThatCannotServeIt(t *testing.T) {
	h := connect(t, &recorder{action: "accept"}, nil)
	_, err := h.client.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "run_scenario",
		Arguments: map[string]any{"scenario": "unsupported/sampling"},
	})
	if err == nil {
		t.Fatal("a client with no sampling handler completed a sampling request")
	}
}

func TestEmptyInputRequestsAreTheLoadSheddingSignal(t *testing.T) {
	h := connect(t, &recorder{action: "accept"}, &mcp.ClientOptions{
		MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true},
	})
	result := h.call(t, "run_scenario", map[string]any{"scenario": "multi/empty"})
	if !result.NeedsInput() {
		t.Fatalf("multi/empty did not produce an input-required result: %+v", result)
	}
	if len(result.InputRequests) != 0 {
		t.Fatalf("multi/empty carried %d requests", len(result.InputRequests))
	}
}

func TestSessionInfoReportsWhatWasNegotiated(t *testing.T) {
	h := connect(t, &recorder{action: "accept"}, nil)
	out := decode[SessionInfoOutput](t, h.call(t, "session_info", map[string]any{}))
	if out.ClientName != "test-client" {
		t.Errorf("got client %q", out.ClientName)
	}
	if !out.ElicitationForm || !out.ElicitationURL || out.ElicitationLegacy {
		t.Errorf("got elicitation form=%v url=%v legacy=%v", out.ElicitationForm, out.ElicitationURL, out.ElicitationLegacy)
	}
	if out.AllowedURL != testConfig().AllowedURL || out.MaxFormFields != testConfig().MaxFormFields {
		t.Errorf("got allowed URL %q and %d fields", out.AllowedURL, out.MaxFormFields)
	}
	if out.DefaultDelivery != "deferred" {
		t.Errorf("got default delivery %q; the in-memory transport should negotiate the current revision", out.DefaultDelivery)
	}
}

func TestDirectDeliveryIsRefusedOnARevisionThatForbidsIt(t *testing.T) {
	h := connect(t, &recorder{action: "accept"}, nil)
	result, err := h.client.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "run_scenario",
		Arguments: map[string]any{"scenario": "form/minimal", "delivery": "direct"},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !result.IsError {
		t.Fatalf("direct delivery succeeded on a revision that forbids server-initiated requests: %+v", result)
	}
}

func TestCustomFormForwardsTheSchemaVerbatim(t *testing.T) {
	answers := &recorder{action: "accept", content: map[string]any{}}
	h := connect(t, answers, nil)
	schema := `{"type":"object","title":"Custom","properties":{"only":{"type":"boolean","title":"Only"}}}`
	h.call(t, "custom_form", map[string]any{"message": "Custom probe.", "schema": schema})

	if answers.count() != 1 {
		t.Fatalf("client saw %d elicitations, want 1", answers.count())
	}
	sent, ok := answers.seen[0].RequestedSchema.(map[string]any)
	if !ok {
		t.Fatalf("client received schema of type %T", answers.seen[0].RequestedSchema)
	}
	properties, _ := sent["properties"].(map[string]any)
	if _, ok := properties["only"]; !ok {
		t.Fatalf("schema did not survive the round trip: %+v", sent)
	}
}

func TestCustomFormRejectsSchemaThatIsNotJSON(t *testing.T) {
	h := connect(t, &recorder{action: "accept"}, nil)
	result, err := h.client.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "custom_form",
		Arguments: map[string]any{"message": "Broken.", "schema": "{not json"},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !result.IsError {
		t.Fatal("a schema that is not JSON was accepted")
	}
}

func TestUnknownScenarioNamesTheCatalogue(t *testing.T) {
	h := connect(t, &recorder{action: "accept"}, nil)
	result, err := h.client.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "run_scenario",
		Arguments: map[string]any{"scenario": "form/does-not-exist"},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !result.IsError || !strings.Contains(renderContent(result), "form/minimal") {
		t.Fatalf("got %+v", result)
	}
}

func TestListScenariosFilters(t *testing.T) {
	h := connect(t, &recorder{action: "accept"}, nil)
	all := decode[ListOutput](t, h.call(t, "list_scenarios", map[string]any{}))
	refusals := decode[ListOutput](t, h.call(t, "list_scenarios", map[string]any{"conformant": false}))
	urls := decode[ListOutput](t, h.call(t, "list_scenarios", map[string]any{"kind": "url"}))

	if all.Count != len(scenarios()) {
		t.Errorf("got %d scenarios, want %d", all.Count, len(scenarios()))
	}
	if refusals.Count == 0 || refusals.Count >= all.Count {
		t.Errorf("got %d refusals out of %d", refusals.Count, all.Count)
	}
	for _, scenario := range refusals.Scenarios {
		if scenario.Conformant {
			t.Errorf("scenario %s is conformant but was listed as a refusal", scenario.ID)
		}
	}
	for _, scenario := range urls.Scenarios {
		if scenario.Kind != KindURL {
			t.Errorf("scenario %s is %s but was listed under url", scenario.ID, scenario.Kind)
		}
	}
}
