// Package mcpserver exposes a catalogue of MCP elicitation shapes as tools,
// so an MCP client can be driven through every one of them on demand.
//
// The server asks for input it does not want and will not keep. Every
// scenario is either a shape a conforming client should render or a shape it
// should refuse, and the point of a run is to find out which one happened.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	Name    = "elicitationtestermcp"
	Version = "0.3.0"

	// protocolVersion20260728 is the first revision that forbids
	// server-initiated elicitation requests: from here on a server must embed
	// them in an input-required result instead. It is the switch between the
	// two delivery modes.
	protocolVersion20260728 = "2026-07-28"
)

const instructions = `A test fixture for MCP elicitation. It asks for input it does not want and
never keeps a value, only its length and digest.

Start with list_scenarios, then run_scenario with one id. Each scenario is
either a shape a conforming client should render, or a shape it should refuse;
the scenario's expect field says which. A refusal is a pass, not a failure.

session_info reports what the client negotiated, which decides how elicitations
are delivered. exchange_log reports what the server sent and what came back,
which is the read-back when the client redacts submitted values from its own
transcript. custom_form and custom_url take an arbitrary schema or URL for
probing something the catalogue does not cover.`

// Config bounds the catalogue and anchors every URL scenario.
type Config struct {
	// AllowedURL is the URL the client under test is configured to trust,
	// typically its reauthorize URL. Every URL scenario is a mutation of this
	// one value, so re-pointing it re-bases all of them at once.
	AllowedURL string
	// MaxFormFields sizes the form/max_fields scenario and, one higher, the
	// form/too_many_fields scenario. Set it to whatever ceiling the client
	// enforces.
	MaxFormFields int
	// StateTTL bounds how long a signed request state stays answerable. It
	// must exceed however long the client parks an elicitation.
	StateTTL time.Duration
	// StateSecret fixes the signing key. Empty means a random per-process
	// key, which invalidates parked elicitations across a restart.
	StateSecret string
	// LogSize is how many exchanges the log retains.
	LogSize int
}

// DefaultConfig returns a configuration pointed at this server's own
// authorize page on its default address.
func DefaultConfig() Config {
	return Config{
		AllowedURL:    "http://localhost:8080/authorize",
		MaxFormFields: 32,
		StateTTL:      2 * time.Hour,
		LogSize:       50,
	}
}

type service struct {
	cfg       Config
	signer    *signer
	log       *exchangeLog
	scenarios map[string]Scenario
}

// New builds the MCP server. It fails rather than starting with a
// configuration whose URL scenarios could not be derived.
func New(cfg Config) (*mcp.Server, error) {
	if cfg.MaxFormFields <= 0 {
		return nil, fmt.Errorf("max form fields must be positive")
	}
	if cfg.LogSize <= 0 {
		return nil, fmt.Errorf("log size must be positive")
	}
	sign, err := newSigner(cfg.StateSecret, cfg.StateTTL)
	if err != nil {
		return nil, err
	}
	svc := &service{cfg: cfg, signer: sign, log: newExchangeLog(cfg.LogSize), scenarios: catalogue()}
	// Building every scenario at startup turns a broken allowed URL into a
	// refusal to start rather than one tool call that fails oddly.
	for _, scenario := range scenarios() {
		for i, build := range scenario.Rounds {
			if _, err := build(cfg); err != nil {
				return nil, fmt.Errorf("scenario %s round %d: %w", scenario.ID, i+1, err)
			}
		}
		if scenario.Fail != nil {
			if _, err := scenario.Fail(cfg); err != nil {
				return nil, fmt.Errorf("scenario %s: %w", scenario.ID, err)
			}
		}
	}

	server := mcp.NewServer(
		&mcp.Implementation{Name: Name, Version: Version, Title: "Elicitation tester"},
		&mcp.ServerOptions{Instructions: instructions},
	)
	svc.register(server)
	return server, nil
}

func (s *service) register(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_scenarios",
		Description: "List the elicitation scenarios this server can run, with what each one emits and what a conforming client should do about it.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
	}, s.listScenarios)

	mcp.AddTool(server, &mcp.Tool{
		Name: "run_scenario",
		Description: "Run one scenario from list_scenarios. Most scenarios ask for input; answering resumes the call, " +
			"and the result reports the action and a fingerprint of each value rather than the value itself.",
	}, s.runScenario)

	mcp.AddTool(server, &mcp.Tool{
		Name: "custom_form",
		Description: "Send a form elicitation carrying a schema supplied verbatim, for probing a shape the catalogue does not cover. " +
			"The schema is passed through byte for byte, including one that is not valid JSON Schema.",
	}, s.customForm)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "custom_url",
		Description: "Send a URL elicitation, or a -32042 URL-elicitation-required error, for an arbitrary URL.",
	}, s.customURL)

	mcp.AddTool(server, &mcp.Tool{
		Name: "session_info",
		Description: "Report what this client negotiated: protocol version, elicitation capabilities, and which delivery mode " +
			"the server will therefore use. Start here when a scenario behaves unexpectedly.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
	}, s.sessionInfo)

	mcp.AddTool(server, &mcp.Tool{
		Name: "exchange_log",
		Description: "Report what the server sent and what came back, newest first. This is the read-back when the client " +
			"redacts submitted values from its own transcript.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.exchangeLog)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "reset_log",
		Description: "Discard the exchange log.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true, DestructiveHint: ptr(false)},
	}, s.resetLog)
}

func ptr[T any](v T) *T { return &v }

// ---- list_scenarios --------------------------------------------------------

type ListInput struct {
	Kind string `json:"kind,omitempty" jsonschema:"Restrict to one kind: form, url, mixed, unsupported or protocol_error. Omit for all of them."`
	// Conformant is a pointer so that "show me only the refusals" is
	// expressible; a plain bool could not tell false from unset.
	Conformant *bool `json:"conformant,omitempty" jsonschema:"true lists only the shapes a conforming client should render; false lists only the ones it should refuse."`
}

type ListOutput struct {
	Scenarios []Scenario `json:"scenarios" jsonschema:"The matching scenarios."`
	Count     int        `json:"count" jsonschema:"Number of scenarios returned."`
	Note      string     `json:"note" jsonschema:"How to read the list."`
}

func (s *service) listScenarios(_ context.Context, _ *mcp.CallToolRequest, in ListInput) (*mcp.CallToolResult, *ListOutput, error) {
	var out []Scenario
	for _, scenario := range scenarios() {
		if in.Kind != "" && string(scenario.Kind) != in.Kind {
			continue
		}
		if in.Conformant != nil && scenario.Conformant != *in.Conformant {
			continue
		}
		out = append(out, scenario)
	}
	if out == nil {
		out = []Scenario{}
	}
	return nil, &ListOutput{
		Scenarios: out,
		Count:     len(out),
		Note: "Pass an id to run_scenario. A scenario with conformant=false is expected to be refused; " +
			"a clean refusal is the pass condition, and a crash, a rendered form or a retry loop is the failure.",
	}, nil
}

// ---- run_scenario ----------------------------------------------------------

type RunInput struct {
	Scenario string `json:"scenario" jsonschema:"Scenario id from list_scenarios, for example form/minimal."`
	Delivery string `json:"delivery,omitempty" jsonschema:"How to deliver the elicitations: auto (default) picks from the negotiated protocol version, deferred embeds them in an input-required result, direct sends elicitation/create requests. Direct works only below protocol version 2026-07-28, which forbids it."`
}

type RunOutput struct {
	Scenario  string            `json:"scenario" jsonschema:"The scenario that ran."`
	Delivery  string            `json:"delivery" jsonschema:"The delivery mode actually used."`
	Round     int               `json:"round" jsonschema:"1-based round just completed."`
	Rounds    int               `json:"rounds" jsonschema:"Total rounds in this scenario."`
	Status    string            `json:"status" jsonschema:"complete when the scenario finished."`
	Expect    string            `json:"expect" jsonschema:"What a conforming client should have done."`
	Responses []ResponseSummary `json:"responses" jsonschema:"What the client answered in the final round."`
	Echoed    map[string]any    `json:"echoed,omitempty" jsonschema:"The submitted values repeated verbatim. Only an integrity probe sets this."`
	Canary    string            `json:"canary,omitempty" jsonschema:"A fixed sentence the server sent unaltered. Only an integrity probe sets this."`
	CanaryOK  *bool             `json:"canary_intact,omitempty" jsonschema:"Whether canary still matches canary_digest as the reader received it. The server always sends true; a false reading means the client altered the result in transit."`
	CanaryHex string            `json:"canary_digest,omitempty" jsonschema:"Digest of the canary as sent. Re-derive it from canary to prove the result was not rewritten."`
	Note      string            `json:"note,omitempty" jsonschema:"Anything worth saying about the run."`
}

// withEcho fills in the integrity probe's fields. The canary and its digest
// travel together so a reader needs nothing but the result to tell whether the
// result reached it intact.
func withEcho(out *RunOutput, scenario Scenario, logged []loggedResponse) *RunOutput {
	if !scenario.Echo {
		return out
	}
	out.Echoed = map[string]any{}
	for _, item := range logged {
		for name, raw := range item.Values {
			var value any
			if err := json.Unmarshal(raw, &value); err != nil {
				value = string(raw)
			}
			out.Echoed[name] = value
		}
	}
	intact := true
	out.Canary = CanarySentence
	out.CanaryOK = &intact
	out.CanaryHex = digest([]byte(CanarySentence))
	out.Note = "Re-derive canary_digest from canary. A mismatch means the client rewrote this result, " +
		"most likely while stripping the echoed values; whatever it damaged here it damaged everywhere."
	return out
}

// ResponseSummary is one answered input request. It carries no values: a
// type, a length and a digest are a stable assertion that a value arrived
// intact, for any value, including ones this fixture would rather not read
// back into an agent's context. exchange_log serves the value on request.
type ResponseSummary struct {
	RequestID string         `json:"request_id" jsonschema:"The id the server assigned to this input request."`
	Method    string         `json:"method" jsonschema:"The request this answers."`
	Action    string         `json:"action,omitempty" jsonschema:"accept, decline or cancel."`
	Fields    []FieldSummary `json:"fields,omitempty" jsonschema:"One entry per submitted field."`
}

type FieldSummary struct {
	Name     string `json:"name" jsonschema:"Field name."`
	JSONType string `json:"json_type" jsonschema:"JSON type of the submitted value."`
	Chars    int    `json:"chars" jsonschema:"Length of the value's JSON encoding."`
	Digest   string `json:"digest" jsonschema:"Short digest of the value, for comparing runs without reprinting it."`
}

func (s *service) runScenario(ctx context.Context, req *mcp.CallToolRequest, in RunInput) (*mcp.CallToolResult, *RunOutput, error) {
	scenario, ok := s.scenarios[in.Scenario]
	if !ok {
		return nil, nil, fmt.Errorf("unknown scenario %q; list_scenarios reports: %s",
			in.Scenario, strings.Join(scenarioIDs(), ", "))
	}
	if scenario.Fail != nil {
		wireErr, err := scenario.Fail(s.cfg)
		if err != nil {
			return nil, nil, err
		}
		s.log.append(Exchange{
			Tool: "run_scenario", Scenario: scenario.ID, Delivery: "protocol_error",
			Requested: describeErrorElicitations(wireErr.Data),
			Note:      fmt.Sprintf("returned JSON-RPC error %d", wireErr.Code),
		})
		// A *jsonrpc.Error returned from a handler propagates as a protocol
		// error rather than being wrapped into an error result, which is the
		// only way to put -32042 on the wire.
		return nil, nil, wireErr
	}
	return s.deliver(ctx, req, "run_scenario", scenario, scenario.Rounds, in.Delivery)
}

// deliver runs one step of a scenario: either the next round's requests, or
// the completion of a scenario whose rounds are all answered.
func (s *service) deliver(
	ctx context.Context,
	req *mcp.CallToolRequest,
	tool string,
	scenario Scenario,
	rounds []roundFunc,
	delivery string,
) (*mcp.CallToolResult, *RunOutput, error) {
	mode, err := s.resolveDelivery(req.Session, delivery)
	if err != nil {
		return nil, nil, err
	}
	if mode == "direct" {
		return s.deliverDirect(ctx, req, tool, scenario, rounds)
	}

	argsDigest := digest(req.Params.Arguments)
	next := 0
	var answered []ResponseSummary
	var lastLogged []loggedResponse
	if req.Params.RequestState != "" || len(req.Params.InputResponses) > 0 {
		st, err := s.signer.verify(req.Params.RequestState, tool, argsDigest)
		if err != nil {
			return nil, nil, err
		}
		if st.Scenario != scenario.ID {
			return nil, nil, fmt.Errorf("request state was issued for scenario %q", st.Scenario)
		}
		next = st.Round + 1
		logged := describeResponses(req.Params.InputResponses)
		answered, lastLogged = summarize(logged), logged
		s.log.append(Exchange{
			Tool: tool, Scenario: scenario.ID, Round: st.Round + 1, Delivery: "deferred", Answered: logged,
		})
	}

	if next < len(rounds) {
		requested, err := rounds[next](s.cfg)
		if err != nil {
			return nil, nil, err
		}
		token, err := s.signer.sign(state{Scenario: scenario.ID, Tool: tool, Round: next, ArgsDigest: argsDigest})
		if err != nil {
			return nil, nil, err
		}
		s.log.append(Exchange{
			Tool: tool, Scenario: scenario.ID, Round: next + 1, Delivery: "deferred",
			Requested: describeRequests(requested),
		})
		// Content and inputRequests are mutually exclusive on the wire, so
		// this result carries no output value.
		return &mcp.CallToolResult{InputRequests: requested, RequestState: token}, nil, nil
	}

	return nil, withEcho(&RunOutput{
		Scenario: scenario.ID, Delivery: "deferred", Round: len(rounds), Rounds: len(rounds),
		Status: "complete", Expect: scenario.Expect, Responses: answered,
		Note: "Values are reported as digests. Call exchange_log with include_values to see what actually arrived.",
	}, scenario, lastLogged), nil
}

// deliverDirect resolves every round inside one tool call by sending
// elicitation/create requests. The protocol forbids this from 2026-07-28 on,
// so it exists to exercise the older path a client still has to support.
func (s *service) deliverDirect(
	ctx context.Context,
	req *mcp.CallToolRequest,
	tool string,
	scenario Scenario,
	rounds []roundFunc,
) (*mcp.CallToolResult, *RunOutput, error) {
	var answered []ResponseSummary
	var lastLogged []loggedResponse
	for index, build := range rounds {
		requested, err := build(s.cfg)
		if err != nil {
			return nil, nil, err
		}
		results := make(mcp.InputResponseMap, len(requested))
		for _, id := range sortedKeys(requested) {
			params, ok := requested[id].(*mcp.ElicitParams)
			if !ok {
				return nil, nil, fmt.Errorf(
					"scenario %s round %d asks for %T, which direct delivery cannot send; run it with delivery=deferred",
					scenario.ID, index+1, requested[id])
			}
			result, err := req.Session.Elicit(ctx, params)
			if err != nil {
				return nil, nil, fmt.Errorf("elicitation %q: %w", id, err)
			}
			results[id] = result
		}
		logged := describeResponses(results)
		answered, lastLogged = summarize(logged), logged
		s.log.append(Exchange{
			Tool: tool, Scenario: scenario.ID, Round: index + 1, Delivery: "direct",
			Requested: describeRequests(requested), Answered: logged,
		})
	}
	return nil, withEcho(&RunOutput{
		Scenario: scenario.ID, Delivery: "direct", Round: len(rounds), Rounds: len(rounds),
		Status: "complete", Expect: scenario.Expect, Responses: answered,
		Note: "Values are reported as digests. Call exchange_log with include_values to see what actually arrived.",
	}, scenario, lastLogged), nil
}

// resolveDelivery picks the mode. auto follows the negotiated version, which
// is what a server that was not a test fixture would do.
func (s *service) resolveDelivery(session *mcp.ServerSession, requested string) (string, error) {
	switch strings.TrimSpace(strings.ToLower(requested)) {
	case "", "auto":
		if supportsDeferred(session) {
			return "deferred", nil
		}
		return "direct", nil
	case "deferred":
		return "deferred", nil
	case "direct":
		return "direct", nil
	default:
		return "", fmt.Errorf("unsupported delivery %q; use auto, deferred or direct", requested)
	}
}

// supportsDeferred reports whether the client negotiated a revision that
// carries input requests in results.
func supportsDeferred(session *mcp.ServerSession) bool {
	return negotiatedVersion(session) >= protocolVersion20260728
}

func negotiatedVersion(session *mcp.ServerSession) string {
	if session == nil {
		return ""
	}
	if params := session.InitializeParams(); params != nil {
		return params.ProtocolVersion
	}
	return ""
}

// ---- custom_form and custom_url --------------------------------------------

type CustomFormInput struct {
	Message string `json:"message" jsonschema:"The message to show above the form."`
	Schema  string `json:"schema" jsonschema:"The requestedSchema, as a JSON document in a string. It is forwarded byte for byte, so it may be anything, including invalid JSON Schema. It must still parse as JSON."`
	// Repeat asks the same form again, which is how a client's own round
	// ceiling gets tested without a purpose-built scenario.
	Repeat   int    `json:"repeat,omitempty" jsonschema:"How many times to ask, default 1."`
	Delivery string `json:"delivery,omitempty" jsonschema:"auto, deferred or direct, as in run_scenario."`
}

func (s *service) customForm(ctx context.Context, req *mcp.CallToolRequest, in CustomFormInput) (*mcp.CallToolResult, *RunOutput, error) {
	if !json.Valid([]byte(in.Schema)) {
		return nil, nil, fmt.Errorf("schema must parse as JSON; it is forwarded verbatim after that")
	}
	repeat := in.Repeat
	if repeat <= 0 {
		repeat = 1
	}
	if repeat > 20 {
		return nil, nil, fmt.Errorf("repeat is capped at 20")
	}
	rounds := make([]roundFunc, repeat)
	for i := range rounds {
		message := in.Message
		if repeat > 1 {
			message = fmt.Sprintf("%s (%d of %d)", in.Message, i+1, repeat)
		}
		rounds[i] = round(form(message, in.Schema))
	}
	scenario := Scenario{
		ID:     "custom/form",
		Kind:   KindForm,
		Expect: "Whatever the supplied schema warrants.",
	}
	return s.deliver(ctx, req, "custom_form", scenario, rounds, in.Delivery)
}

type CustomURLInput struct {
	Message       string `json:"message" jsonschema:"The message to show beside the link."`
	URL           string `json:"url" jsonschema:"The URL to send, forwarded verbatim."`
	ElicitationID string `json:"elicitation_id,omitempty" jsonschema:"The elicitationId to send. Omit to send an empty one."`
	Mode          string `json:"mode,omitempty" jsonschema:"elicitation (default) sends a URL elicitation; error returns a -32042 URL-elicitation-required error instead."`
	Delivery      string `json:"delivery,omitempty" jsonschema:"auto, deferred or direct, as in run_scenario. Ignored when mode is error."`
}

func (s *service) customURL(ctx context.Context, req *mcp.CallToolRequest, in CustomURLInput) (*mcp.CallToolResult, *RunOutput, error) {
	if in.URL == "" {
		return nil, nil, fmt.Errorf("url is required")
	}
	params := urlElicit(in.Message, in.URL, in.ElicitationID)
	switch strings.TrimSpace(strings.ToLower(in.Mode)) {
	case "", "elicitation":
		scenario := Scenario{ID: "custom/url", Kind: KindURL, Expect: "Whatever the supplied URL warrants."}
		return s.deliver(ctx, req, "custom_url", scenario, []roundFunc{round(params)}, in.Delivery)
	case "error":
		wireErr := urlRequiredError(params)
		s.log.append(Exchange{
			Tool: "custom_url", Scenario: "custom/url", Delivery: "protocol_error",
			Requested: describeRequests(requests(params)),
			Note:      fmt.Sprintf("returned JSON-RPC error %d", wireErr.Code),
		})
		return nil, nil, wireErr
	default:
		return nil, nil, fmt.Errorf("unsupported mode %q; use elicitation or error", in.Mode)
	}
}

// ---- session_info ----------------------------------------------------------

type SessionInfoOutput struct {
	Server            string `json:"server" jsonschema:"Server name and version."`
	ProtocolVersion   string `json:"protocol_version" jsonschema:"The negotiated protocol version, empty if the session did not report one."`
	ClientName        string `json:"client_name" jsonschema:"The client's reported name."`
	ClientVersion     string `json:"client_version" jsonschema:"The client's reported version."`
	ElicitationForm   bool   `json:"elicitation_form" jsonschema:"Whether the client advertised form elicitation."`
	ElicitationURL    bool   `json:"elicitation_url" jsonschema:"Whether the client advertised URL elicitation."`
	ElicitationLegacy bool   `json:"elicitation_legacy" jsonschema:"True when the client advertised elicitation without naming a mode, which older clients do and which means form."`
	Sampling          bool   `json:"sampling" jsonschema:"Whether the client advertised sampling."`
	Roots             bool   `json:"roots" jsonschema:"Whether the client advertised roots."`
	DefaultDelivery   string `json:"default_delivery" jsonschema:"The delivery mode auto will choose for this session."`
	AllowedURL        string `json:"allowed_url" jsonschema:"The URL every URL scenario is derived from; it must be an origin this client trusts."`
	MaxFormFields     int    `json:"max_form_fields" jsonschema:"The field count form/max_fields emits."`
	Note              string `json:"note,omitempty" jsonschema:"Anything about this session worth knowing before reading a result."`
}

func (s *service) sessionInfo(_ context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, *SessionInfoOutput, error) {
	out := &SessionInfoOutput{
		Server:          Name + " " + Version,
		ProtocolVersion: negotiatedVersion(req.Session),
		AllowedURL:      s.cfg.AllowedURL,
		MaxFormFields:   s.cfg.MaxFormFields,
	}
	if params := req.Session.InitializeParams(); params != nil {
		if params.ClientInfo != nil {
			out.ClientName = params.ClientInfo.Name
			out.ClientVersion = params.ClientInfo.Version
		}
		if caps := params.Capabilities; caps != nil {
			out.Sampling = caps.Sampling != nil
			out.Roots = caps.RootsV2 != nil || caps.Roots.ListChanged
			if caps.Elicitation != nil {
				out.ElicitationForm = caps.Elicitation.Form != nil
				out.ElicitationURL = caps.Elicitation.URL != nil
				out.ElicitationLegacy = caps.Elicitation.Form == nil && caps.Elicitation.URL == nil
			}
		}
	}
	if supportsDeferred(req.Session) {
		out.DefaultDelivery = "deferred"
		out.Note = "This revision forbids server-initiated elicitation requests, so delivery=direct will fail here."
	} else {
		out.DefaultDelivery = "direct"
		out.Note = "This revision predates input-required results. delivery=deferred will produce a result the client may not understand."
	}
	if out.ElicitationLegacy {
		out.Note += " The client advertised elicitation without naming form or url, which means form only."
	}
	return nil, out, nil
}

// ---- exchange_log ----------------------------------------------------------

type LogInput struct {
	Limit         int  `json:"limit,omitempty" jsonschema:"Maximum entries to return, newest first. Defaults to 10."`
	IncludeValues bool `json:"include_values,omitempty" jsonschema:"Include the values the client submitted, not just their digests. They are printed back into the conversation, so ask for this only when confirming a value arrived."`
}

type LogOutput struct {
	Entries []LogEntry `json:"entries" jsonschema:"Exchanges, newest first."`
	Count   int        `json:"count" jsonschema:"Number of entries returned."`
}

type LogEntry struct {
	At        string        `json:"at" jsonschema:"When the exchange happened, RFC 3339."`
	Tool      string        `json:"tool" jsonschema:"The tool that was called."`
	Scenario  string        `json:"scenario,omitempty" jsonschema:"The scenario it ran."`
	Round     int           `json:"round,omitempty" jsonschema:"1-based round."`
	Delivery  string        `json:"delivery" jsonschema:"deferred, direct or protocol_error."`
	Requested []LogRequest  `json:"requested,omitempty" jsonschema:"What the server sent."`
	Answered  []LogResponse `json:"answered,omitempty" jsonschema:"What the client sent back."`
	Note      string        `json:"note,omitempty" jsonschema:"Anything else recorded."`
}

type LogRequest struct {
	RequestID string `json:"request_id" jsonschema:"Server-assigned id."`
	Method    string `json:"method" jsonschema:"The request that was made."`
	Mode      string `json:"mode,omitempty" jsonschema:"form or url."`
	Message   string `json:"message,omitempty" jsonschema:"The message sent, truncated for readability."`
	URL       string `json:"url,omitempty" jsonschema:"The URL sent, verbatim."`
	Schema    string `json:"schema,omitempty" jsonschema:"The requestedSchema sent, verbatim."`
}

type LogResponse struct {
	RequestID string         `json:"request_id" jsonschema:"Server-assigned id."`
	Method    string         `json:"method" jsonschema:"The request this answered."`
	Action    string         `json:"action,omitempty" jsonschema:"accept, decline or cancel."`
	Fields    []FieldSummary `json:"fields,omitempty" jsonschema:"One entry per submitted field."`
	Values    map[string]any `json:"values,omitempty" jsonschema:"The submitted values, present only when include_values was set."`
}

func (s *service) exchangeLog(_ context.Context, _ *mcp.CallToolRequest, in LogInput) (*mcp.CallToolResult, *LogOutput, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = 10
	}
	entries := s.log.recent(limit)
	out := make([]LogEntry, 0, len(entries))
	for _, entry := range entries {
		rendered := LogEntry{
			At: entry.At.Format(time.RFC3339), Tool: entry.Tool, Scenario: entry.Scenario,
			Round: entry.Round, Delivery: entry.Delivery, Note: entry.Note,
		}
		for _, item := range entry.Requested {
			rendered.Requested = append(rendered.Requested, LogRequest{
				RequestID: item.RequestID, Method: item.Method, Mode: item.Mode,
				Message: truncate(item.Message, 200), URL: item.URL, Schema: string(item.Schema),
			})
		}
		for _, item := range entry.Answered {
			response := LogResponse{
				RequestID: item.RequestID, Method: item.Method, Action: item.Action,
				Fields: summarizeFields(item.Values),
			}
			if in.IncludeValues && len(item.Values) > 0 {
				response.Values = make(map[string]any, len(item.Values))
				for name, raw := range item.Values {
					var value any
					if err := json.Unmarshal(raw, &value); err != nil {
						value = string(raw)
					}
					response.Values[name] = value
				}
			}
			rendered.Answered = append(rendered.Answered, response)
		}
		out = append(out, rendered)
	}
	return nil, &LogOutput{Entries: out, Count: len(out)}, nil
}

type ResetOutput struct {
	Discarded int `json:"discarded" jsonschema:"Number of entries discarded."`
}

func (s *service) resetLog(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, *ResetOutput, error) {
	return nil, &ResetOutput{Discarded: s.log.reset()}, nil
}

// ---- shared helpers --------------------------------------------------------

func summarize(logged []loggedResponse) []ResponseSummary {
	out := make([]ResponseSummary, 0, len(logged))
	for _, item := range logged {
		out = append(out, ResponseSummary{
			RequestID: item.RequestID, Method: item.Method, Action: item.Action,
			Fields: summarizeFields(item.Values),
		})
	}
	return out
}

func summarizeFields(values map[string]json.RawMessage) []FieldSummary {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]FieldSummary, 0, len(names))
	for _, name := range names {
		raw := values[name]
		out = append(out, FieldSummary{
			Name: name, JSONType: jsonType(raw), Chars: len(raw), Digest: digest(raw),
		})
	}
	return out
}

func jsonType(raw json.RawMessage) string {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return "invalid"
	}
	switch value.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	default:
		return "object"
	}
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + fmt.Sprintf("… (%d characters total)", len(value))
}

func sortedKeys(reqs mcp.InputRequestMap) []string {
	keys := make([]string, 0, len(reqs))
	for key := range reqs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedResponseKeys(responses mcp.InputResponseMap) []string {
	keys := make([]string, 0, len(responses))
	for key := range responses {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
