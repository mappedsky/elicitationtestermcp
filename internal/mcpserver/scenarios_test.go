package mcpserver

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func testConfig() Config {
	cfg := DefaultConfig()
	cfg.AllowedURL = "https://seizu.test/app/chat/connections"
	return cfg
}

func TestEveryScenarioBuilds(t *testing.T) {
	cfg := testConfig()
	seen := map[string]bool{}
	for _, scenario := range scenarios() {
		if seen[scenario.ID] {
			t.Errorf("duplicate scenario id %q", scenario.ID)
		}
		seen[scenario.ID] = true
		if scenario.Summary == "" || scenario.Expect == "" {
			t.Errorf("scenario %s must say what it emits and what to expect", scenario.ID)
		}
		if (len(scenario.Rounds) == 0) == (scenario.Fail == nil) {
			t.Errorf("scenario %s must have either rounds or a protocol error, not both or neither", scenario.ID)
		}
		for i, build := range scenario.Rounds {
			if _, err := build(cfg); err != nil {
				t.Errorf("scenario %s round %d: %v", scenario.ID, i+1, err)
			}
		}
		if scenario.Fail != nil {
			wireErr, err := scenario.Fail(cfg)
			if err != nil {
				t.Errorf("scenario %s: %v", scenario.ID, err)
			} else if wireErr.Code != mcp.CodeURLElicitationRequired {
				t.Errorf("scenario %s returned code %d, want %d", scenario.ID, wireErr.Code, mcp.CodeURLElicitationRequired)
			}
		}
	}
}

// TestConformantFormsAreInsideTheSubset keeps the catalogue honest. A
// scenario marked conformant is a claim that a client should render it, and a
// scenario not marked conformant is a claim that it should not; both claims
// are checked here against an independent reading of the elicitation subset,
// so a schema cannot drift out of the group it is filed under.
func TestConformantFormsAreInsideTheSubset(t *testing.T) {
	cfg := testConfig()
	for _, scenario := range scenarios() {
		if scenario.Kind != KindForm {
			continue
		}
		for i, build := range scenario.Rounds {
			requested, err := build(cfg)
			if err != nil {
				t.Fatalf("scenario %s round %d: %v", scenario.ID, i+1, err)
			}
			for id, item := range requested {
				params, ok := item.(*mcp.ElicitParams)
				if !ok {
					continue
				}
				err := checkFormSubset(params, cfg.MaxFormFields)
				if scenario.Conformant && err != nil {
					t.Errorf("scenario %s request %s is marked conformant but %v", scenario.ID, id, err)
				}
				if !scenario.Conformant && err == nil {
					t.Errorf("scenario %s request %s is marked non-conformant but is inside the subset", scenario.ID, id)
				}
			}
		}
	}
}

// TestConformantURLsShareTheAllowedOrigin applies the same check to the URL
// scenarios: a conformant one differs from the trusted URL only in path and
// query, and a non-conformant one differs in something that matters.
func TestConformantURLsShareTheAllowedOrigin(t *testing.T) {
	cfg := testConfig()
	allowed, err := url.Parse(cfg.AllowedURL)
	if err != nil {
		t.Fatalf("parse allowed URL: %v", err)
	}
	for _, scenario := range scenarios() {
		if scenario.Kind != KindURL {
			continue
		}
		for i, build := range scenario.Rounds {
			requested, err := build(cfg)
			if err != nil {
				t.Fatalf("scenario %s round %d: %v", scenario.ID, i+1, err)
			}
			for id, item := range requested {
				params := item.(*mcp.ElicitParams)
				err := checkURLSubset(params, allowed)
				if scenario.Conformant && err != nil {
					t.Errorf("scenario %s request %s is marked conformant but %v", scenario.ID, id, err)
				}
				if !scenario.Conformant && err == nil {
					t.Errorf("scenario %s request %s is marked non-conformant but is inside the subset", scenario.ID, id)
				}
			}
		}
	}
}

func TestURLScenariosDeriveFromTheConfiguredURL(t *testing.T) {
	// Re-pointing the allowed URL must re-base the conformant scenarios,
	// which is the whole reason they are expressed as mutations of it.
	cfg := testConfig()
	cfg.AllowedURL = "http://127.0.0.1:9999/elsewhere"
	allowed, err := url.Parse(cfg.AllowedURL)
	if err != nil {
		t.Fatalf("parse allowed URL: %v", err)
	}
	for _, scenario := range scenarios() {
		if scenario.Kind != KindURL || !scenario.Conformant {
			continue
		}
		requested, err := scenario.Rounds[0](cfg)
		if err != nil {
			t.Fatalf("scenario %s: %v", scenario.ID, err)
		}
		for id, item := range requested {
			if err := checkURLSubset(item.(*mcp.ElicitParams), allowed); err != nil {
				t.Errorf("scenario %s request %s: %v", scenario.ID, id, err)
			}
		}
	}
}

func TestOversizedProbesAreOneOverTheLine(t *testing.T) {
	if got := len(longText(1001)); got != 1001 {
		t.Errorf("longText(1001) produced %d characters", got)
	}
	if got := len(bulkySchema(32)); got <= 32768 {
		t.Errorf("bulkySchema(32) is %d bytes, want more than 32768", got)
	}
	cfg := testConfig()
	for id, want := range map[string]int{"form/max_fields": cfg.MaxFormFields, "form/too_many_fields": cfg.MaxFormFields + 1} {
		scenario := catalogue()[id]
		requested, err := scenario.Rounds[0](cfg)
		if err != nil {
			t.Fatalf("scenario %s: %v", id, err)
		}
		for _, item := range requested {
			var schema struct {
				Properties map[string]json.RawMessage `json:"properties"`
			}
			raw := item.(*mcp.ElicitParams).RequestedSchema.(json.RawMessage)
			if err := json.Unmarshal(raw, &schema); err != nil {
				t.Fatalf("scenario %s schema: %v", id, err)
			}
			if len(schema.Properties) != want {
				t.Errorf("scenario %s emitted %d fields, want %d", id, len(schema.Properties), want)
			}
		}
	}
}

func TestMalformedProtocolErrorCarriesUnparsableData(t *testing.T) {
	scenario := catalogue()["error/url_required_malformed"]
	wireErr, err := scenario.Fail(testConfig())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var data struct {
		Elicitations []json.RawMessage `json:"elicitations"`
	}
	if err := json.Unmarshal(wireErr.Data, &data); err == nil {
		t.Error("the malformed scenario decoded into the documented shape, so it probes nothing")
	}
}

// checkFormSubset is an independent reading of the flat elicitation subset: a
// root object, primitive properties only, an allowlisted set of keywords,
// bounded prose, and bounds that can be satisfied.
func checkFormSubset(params *mcp.ElicitParams, maxFields int) error {
	if params.Mode != "form" {
		return fmt.Errorf("mode is %q", params.Mode)
	}
	if len(params.Message) > 1000 {
		return fmt.Errorf("message is %d characters", len(params.Message))
	}
	raw, ok := params.RequestedSchema.(json.RawMessage)
	if !ok {
		return fmt.Errorf("schema is %T, not raw JSON", params.RequestedSchema)
	}
	if len(raw) > 32768 {
		return fmt.Errorf("schema is %d bytes", len(raw))
	}
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(raw, &schema); err != nil {
		return fmt.Errorf("schema does not decode as an object: %w", err)
	}
	for key := range schema {
		switch key {
		case "type", "properties", "required", "title", "description":
		default:
			return fmt.Errorf("unsupported root keyword %q", key)
		}
	}
	var kind string
	if err := json.Unmarshal(schema["type"], &kind); err != nil || kind != "object" {
		return fmt.Errorf("root type is not object")
	}
	properties := map[string]map[string]json.RawMessage{}
	if encoded, ok := schema["properties"]; ok {
		if err := json.Unmarshal(encoded, &properties); err != nil {
			return fmt.Errorf("properties does not decode: %w", err)
		}
	}
	if len(properties) > maxFields {
		return fmt.Errorf("%d fields exceeds the %d-field ceiling", len(properties), maxFields)
	}
	var required []string
	if encoded, ok := schema["required"]; ok {
		if err := json.Unmarshal(encoded, &required); err != nil {
			return fmt.Errorf("required does not decode: %w", err)
		}
	}
	for _, name := range required {
		if _, ok := properties[name]; !ok {
			return fmt.Errorf("required names absent field %q", name)
		}
	}
	if err := checkText(schema); err != nil {
		return err
	}
	for name, field := range properties {
		if err := checkField(name, field); err != nil {
			return err
		}
	}
	return nil
}

func checkField(name string, field map[string]json.RawMessage) error {
	switch {
	case len(name) == 0 || len(name) > 128:
		return fmt.Errorf("field name is %d characters", len(name))
	case name == "__proto__" || name == "constructor" || name == "prototype":
		return fmt.Errorf("field name %q is an inherited property", name)
	}
	for key := range field {
		switch key {
		case "type", "title", "description", "enum", "enumNames", "default",
			"minLength", "maxLength", "minimum", "maximum":
		default:
			return fmt.Errorf("field %q carries unsupported keyword %q", name, key)
		}
	}
	var kind string
	if err := json.Unmarshal(field["type"], &kind); err != nil {
		return fmt.Errorf("field %q has no type", name)
	}
	switch kind {
	case "string", "number", "integer", "boolean":
	default:
		return fmt.Errorf("field %q has type %q", name, kind)
	}
	if err := checkText(field); err != nil {
		return err
	}
	minLength, maxLength := numberOr(field, "minLength", 0), numberOr(field, "maxLength", 4096)
	minimum, maximum := numberOr(field, "minimum", -1e308), numberOr(field, "maximum", 1e308)
	if minLength > maxLength || minimum > maximum {
		return fmt.Errorf("field %q has inverted bounds", name)
	}
	var enum []any
	if encoded, ok := field["enum"]; ok {
		if err := json.Unmarshal(encoded, &enum); err != nil {
			return fmt.Errorf("field %q enum does not decode: %w", name, err)
		}
		if len(enum) < 1 || len(enum) > 64 {
			return fmt.Errorf("field %q has %d enum values", name, len(enum))
		}
	}
	if encoded, ok := field["enumNames"]; ok {
		var names []string
		if err := json.Unmarshal(encoded, &names); err != nil {
			return fmt.Errorf("field %q enumNames does not decode: %w", name, err)
		}
		if len(names) != len(enum) {
			return fmt.Errorf("field %q has %d enum values and %d labels", name, len(enum), len(names))
		}
	}
	if encoded, ok := field["default"]; ok {
		var value any
		if err := json.Unmarshal(encoded, &value); err != nil {
			return fmt.Errorf("field %q default does not decode: %w", name, err)
		}
		switch value.(type) {
		case string:
			if kind != "string" {
				return fmt.Errorf("field %q is %s with a string default", name, kind)
			}
		case float64:
			if kind != "number" && kind != "integer" {
				return fmt.Errorf("field %q is %s with a numeric default", name, kind)
			}
		case bool:
			if kind != "boolean" {
				return fmt.Errorf("field %q is %s with a boolean default", name, kind)
			}
		default:
			return fmt.Errorf("field %q has a non-primitive default", name)
		}
	}
	return nil
}

func checkText(item map[string]json.RawMessage) error {
	for _, key := range []string{"title", "description"} {
		encoded, ok := item[key]
		if !ok {
			continue
		}
		var text string
		if err := json.Unmarshal(encoded, &text); err != nil {
			return fmt.Errorf("%s does not decode as a string", key)
		}
		if len(text) > 1000 {
			return fmt.Errorf("%s is %d characters", key, len(text))
		}
	}
	return nil
}

func numberOr(field map[string]json.RawMessage, key string, fallback float64) float64 {
	encoded, ok := field[key]
	if !ok {
		return fallback
	}
	var value float64
	if err := json.Unmarshal(encoded, &value); err != nil {
		return fallback
	}
	return value
}

// checkURLSubset is an independent reading of what makes a URL elicitation
// openable: the trusted origin exactly, no userinfo, no fragment, nothing a
// second parser would read differently.
func checkURLSubset(params *mcp.ElicitParams, allowed *url.URL) error {
	if params.Mode != "url" {
		return fmt.Errorf("mode is %q", params.Mode)
	}
	if params.ElicitationID == "" {
		return fmt.Errorf("elicitationId is empty")
	}
	if len(params.Message) > 1000 {
		return fmt.Errorf("message is %d characters", len(params.Message))
	}
	if len(params.URL) > 4096 {
		return fmt.Errorf("URL is %d characters", len(params.URL))
	}
	for _, r := range params.URL {
		if r <= 32 || r == 127 {
			return fmt.Errorf("URL carries the control character %q", r)
		}
	}
	if strings.Contains(params.URL, `\`) {
		return fmt.Errorf("URL carries a backslash")
	}
	target, err := url.Parse(params.URL)
	if err != nil {
		return fmt.Errorf("URL does not parse: %w", err)
	}
	switch {
	case target.Scheme != "http" && target.Scheme != "https":
		return fmt.Errorf("scheme is %q", target.Scheme)
	case target.User != nil:
		return fmt.Errorf("URL embeds userinfo")
	case target.Fragment != "":
		return fmt.Errorf("URL carries a fragment")
	case origin(target) != origin(allowed):
		return fmt.Errorf("origin is %s, want %s", origin(target), origin(allowed))
	}
	return nil
}

func origin(u *url.URL) string {
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return fmt.Sprintf("%s://%s:%s", u.Scheme, u.Hostname(), port)
}

// TestReadmeListsEveryScenario keeps the catalogue table in the README from
// drifting. The table is a reference people read instead of calling
// list_scenarios, so a scenario missing from it is a scenario nobody runs.
func TestReadmeListsEveryScenario(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	for _, scenario := range scenarios() {
		if !strings.Contains(string(readme), "`"+scenario.ID+"`") {
			t.Errorf("scenario %s is not in the README table", scenario.ID)
		}
	}
}

// TestCredentialFormRendersRatherThanBeingRefused pins the one scenario whose
// expectation looks backwards. The spec's MUST NOT about passwords and API
// keys in form mode binds the *server*; it gives the client no rule to apply,
// and no client can tell a field named api_key from any other string. Marking
// this non-conformant would assert that a client should refuse on the label --
// guesswork that misses every case where the field is named something else.
func TestCredentialFormRendersRatherThanBeingRefused(t *testing.T) {
	for _, scenario := range scenarios() {
		if scenario.ID != "form/requests_credential" {
			continue
		}
		if !scenario.Conformant {
			t.Error("form/requests_credential must render: the spec constrains the server, not the client")
		}
		if scenario.Echo {
			t.Error("form/requests_credential must not echo: it is about rendering, not result integrity")
		}
		return
	}
	t.Error("form/requests_credential is missing, so nothing covers a server breaking the form-mode rule")
}

// TestEchoingScenariosAreConformantForms keeps the integrity probes usable: a
// client that refuses the schema never reaches the echo, so an echoing
// scenario has to be one it will render.
func TestEchoingScenariosAreConformantForms(t *testing.T) {
	echoing := 0
	for _, scenario := range scenarios() {
		if !scenario.Echo {
			continue
		}
		echoing++
		if !scenario.Conformant || scenario.Kind != KindForm {
			t.Errorf("scenario %s echoes but is %s/conformant=%v", scenario.ID, scenario.Kind, scenario.Conformant)
		}
	}
	if echoing == 0 {
		t.Error("no scenario echoes submitted values, so nothing probes result integrity")
	}
}
