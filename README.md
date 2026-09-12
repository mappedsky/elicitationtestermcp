# elicitationtestermcp

An MCP server that exists to be answered badly.

It serves a catalogue of elicitation shapes — forms, URLs, groups of both,
input requests that are not elicitations at all, and the `-32042` error — and
puts them in front of a client on demand. Roughly a quarter of them are shapes
a conforming client should render. The rest are shapes it should refuse, and
for those **a clean refusal is the pass condition**: an error the model can
read, no crash, no half-rendered form, no retry loop.

The server asks for input it does not want. Nothing a client submits is kept
beyond its length and a short digest; the values themselves are readable only
by asking `exchange_log` for them explicitly.

## Why there are two endpoints

Elicitation is delivered two different ways depending on the protocol revision,
and no single endpoint can serve both.

| | `/mcp` | `/mcp/legacy` |
|---|---|---|
| Session | stateless | stateful |
| Protocol | up to **2026-07-28** | capped at **2025-11-25** |
| Delivery | `inputRequests` in the tool result, answered on a later call | `elicitation/create` requests sent during the call |

The transport only permits 2026-07-28 when it is stateless, and 2026-07-28 is
the revision that *forbids* server-initiated elicitation requests: from there
on they must be embedded in an input-required result. So the stateless endpoint
cannot send an `elicitation/create`, and the stateful one cannot speak the
revision that replaced it. Point the client at whichever path matches the path
you mean to test; `session_info` reports which one you actually got.

`run_scenario` takes a `delivery` argument to force the issue. `auto`, the
default, follows the negotiated revision, which is what a server that was not a
test fixture would do.

## Running it

```
make up         # build and start on http://localhost:8090
make smoke      # start it, then drive both endpoints end to end with curl
make test       # go vet + go test -race
make down
```

Or directly:

```
go run ./cmd/elicitationtestermcp \
  -http-address 127.0.0.1:8090 \
  -allowed-url http://localhost:8090/authorize \
  -state-secret dev
```

### The two settings that matter

**`-allowed-url`** is the URL the client under test is configured to trust,
which for an OAuth-style gateway is its reauthorize URL. Every URL scenario is
expressed as a mutation of this one value — a different port, a different
scheme, userinfo spliced in, a fragment appended — so re-pointing it re-bases
the whole set. If it does not match what the client trusts, every URL scenario
collapses into "refused", including the ones that were supposed to pass.

It defaults to this server's own stand-in authorization page at
`/authorize`. That page grants nothing and completes nothing. It exists so the
link a client rendered can actually be opened and compared with the one the
server sent, which is the only way to see what happened to a URL between
arriving and being displayed.

**`-state-secret`** fixes the key that signs request states. A client may park
an elicitation for an hour and answer it on a different connection, so the
state has to survive a restart. Left empty, the key is random per process and
every parked elicitation dies with the container.

### Flags

| Flag | Environment | Default | Meaning |
|---|---|---|---|
| `-transport` | `ELICIT_MCP_TRANSPORT` | `http` | `http` or `stdio` |
| `-http-address` | `ELICIT_MCP_HTTP_ADDRESS` | `127.0.0.1:8080` | listen address |
| `-http-path` | `ELICIT_MCP_HTTP_PATH` | `/mcp` | stateless endpoint |
| `-legacy-http-path` | `ELICIT_MCP_LEGACY_HTTP_PATH` | `/mcp/legacy` | stateful endpoint; empty disables it |
| `-authorize-path` | `ELICIT_MCP_AUTHORIZE_PATH` | `/authorize` | stand-in authorization page; empty disables it |
| `-allowed-url` | `ELICIT_MCP_ALLOWED_URL` | `http://localhost:8080/authorize` | the URL the client trusts |
| `-max-form-fields` | `ELICIT_MCP_MAX_FORM_FIELDS` | `32` | size of `form/max_fields`; one more is `form/too_many_fields` |
| `-state-ttl` | `ELICIT_MCP_STATE_TTL` | `2h` | how long a parked elicitation stays answerable |
| `-state-secret` | `ELICIT_MCP_STATE_SECRET` | random | request-state signing key |
| `-log-size` | `ELICIT_MCP_LOG_SIZE` | `50` | exchanges retained |

## Tools

| Tool | Use |
|---|---|
| `list_scenarios` | The catalogue, filterable by `kind` and by `conformant`. Start here. |
| `run_scenario` | Run one scenario by id. `delivery` forces `deferred` or `direct`. |
| `session_info` | What the client negotiated: protocol version, elicitation capabilities, the delivery mode `auto` will pick. Start here when a scenario behaves oddly. |
| `custom_form` | A form carrying a schema you supply, forwarded byte for byte. `repeat` asks the same form N times. |
| `custom_url` | A URL elicitation, or a `-32042` error, for any URL. |
| `exchange_log` | What the server sent and what came back. `include_values` returns the submitted values. |
| `reset_log` | Discard the log. |

`run_scenario` reports each submitted value as a type, a length and a digest,
never the value. That is deliberate: a client that redacts submitted values
from its own transcript is behaving correctly, and this server should not undo
that by echoing them back into the conversation. `exchange_log` with
`include_values` is the explicit way to confirm a value arrived intact.

## The catalogue

`list_scenarios` is the live version of this. **Renders?** is whether a
conforming client should put it in front of the user; `no` means a refusal is
the expected outcome.


| `form` scenario | Renders? | What it sends | What should happen |
|---|---|---|---|
| `form/minimal` | yes | One required string with length bounds. | Renders a single text input; submitting returns action=accept with that one field. |
| `form/types` | yes | String, integer, number and boolean in one form. | Renders four inputs of the right widget for each type; integer rejects 1.5. |
| `form/enum` | yes | String enum with enumNames labels and a default. | Renders a select showing the labels, not the values, preselected on staging. |
| `form/enum_integer` | yes | Integer enum with enumNames labels. | Allowed by the elicitation subset. Note that the Go SDK's own post-accept schema check rejects a non-string enum, so a direct-delivery accept fails there even though the form rendered. |
| `form/defaults` | yes | A default on every primitive type. | Every input is prefilled; accepting without edits returns the defaults. |
| `form/bounds` | yes | minLength/maxLength and minimum/maximum, including a negative minimum. | Out-of-range values are refused before submission rather than sent. |
| `form/optional` | yes | No required fields. | The submit control is enabled with every field empty. |
| `form/no_fields` | yes | An object schema with an empty properties map. | Renders as a bare acknowledgement; the only answer carried is the action. |
| `form/max_fields` | yes | Exactly as many fields as the configured field ceiling. | Renders every field. This is the largest form the client should accept. |
| `redaction/free_text` | yes | A free-text secret, echoed back beside the canary sentence. | The submitted value may be absent from the result; hiding it is a reasonable thing for a client to do. The canary must still hash to canary_digest. If it does not, the client removed the value by rewriting the whole result and damaged unrelated text doing it. |
| `redaction/short_values` | yes | An integer, a boolean and a three-letter enum, echoed back beside the canary sentence. | The canary must still hash to canary_digest. None of these values can be hidden by rewriting the result: 1 and 10 are in its numbers, true is in its text, and dev and prod sit inside developer and production. A client that tries destroys the result it is protecting. |
| `form/nested_object` | **no** | A property whose type is object. | Refused: elicitation schemas are flat. |
| `form/multi_select` | **no** | An array-of-enum multi-select property. | Refused by a client whose subset is primitives only. The MCP spec and the Go SDK do allow this shape, so a refusal here is a deliberate narrowing, not a bug. |
| `form/titled_enum` | **no** | A oneOf const/title enum, the successor to enumNames. | Refused by an enumNames-only client. Same deliberate narrowing as form/multi_select. |
| `form/too_many_fields` | **no** | One field more than the configured ceiling. | Refused whole. A client must not render a truncated form. |
| `form/prototype_field_names` | **no** | Fields named __proto__ and constructor. | Refused. Rendering these into a JavaScript object is prototype pollution. |
| `form/unknown_keyword` | **no** | A string property carrying pattern and format. | Refused by a keyword-allowlisting client, which must not silently drop constraints it will not enforce. |
| `form/extra_object_keyword` | **no** | additionalProperties and patternProperties at the root. | Refused for the same reason as form/unknown_keyword. |
| `form/inverted_bounds` | **no** | minLength above maxLength and minimum above maximum. | Refused. Rendering it produces a form no input can satisfy. |
| `form/enum_names_mismatch` | **no** | Three enum values, one enumNames label. | Refused. Pairing by index would mislabel two options. |
| `form/default_wrong_type` | **no** | A string default on an integer property. | Refused. The default must satisfy the field it prefills. |
| `form/required_unknown_field` | **no** | required names a property that does not exist. | Refused. No submission could ever satisfy it. |
| `form/not_an_object` | **no** | A root schema of type string. | Refused. The elicitation root is always an object. |
| `form/oversized_text` | **no** | A field description of 1001 characters. | Refused, or bounded. It must not reach a prompt or a page unbounded. |
| `form/oversized_message` | **no** | An elicitation message of 1001 characters. | Refused, or bounded, on the same reasoning as form/oversized_text. |
| `form/injection_prose` | yes | A well-formed schema whose message and labels carry instructions aimed at the model. | Rendered as untrusted text. The instructions must not change what the agent does. |
| `form/oversized_schema` | **no** | A legal schema whose JSON runs past 32 KiB. | Refused. Size is bounded independently of field count, because prose is where the bytes go. |
| `form/enum_too_large` | **no** | An enum of 65 values. | Refused, or bounded. A select of unbounded length is a rendering problem, not a choice. |
| `form/enum_empty` | **no** | An enum with no values. | Refused. There is nothing to choose. |
| `form/long_field_name` | **no** | A property name of 129 characters. | Refused, or bounded before the name reaches a label. |

| `url` scenario | Renders? | What it sends | What should happen |
|---|---|---|---|
| `url/allowed` | yes | A URL on exactly the origin the client is configured to trust. | Surfaced as a link the owner can open. This is the only URL scenario a client should show. |
| `url/allowed_long_path` | yes | The trusted origin with a long path and query string. | Surfaced. Only the origin is matched; path and query are free. |
| `url/other_origin` | **no** | A URL on an origin the client was never told to trust. | Refused. An untrusted origin in a link the owner is being told to open is a phishing primitive. |
| `url/other_port` | **no** | The trusted host and scheme, a different port. | Refused. Origin includes the port. |
| `url/other_scheme` | **no** | The trusted host, the other of http and https. | Refused. Origin includes the scheme. |
| `url/subdomain` | **no** | A subdomain of the trusted host. | Refused. Origin matching is exact, not suffix-based. |
| `url/credentials` | **no** | Userinfo embedded in an otherwise trusted URL. | Refused. Userinfo is how a link is made to read as one host and resolve as another. |
| `url/fragment` | **no** | A trusted URL carrying a fragment. | Refused. A fragment is where an implicit-flow token would be smuggled back out. |
| `url/javascript_scheme` | **no** | A javascript: URL. | Refused. Only http and https are openable. |
| `url/data_scheme` | **no** | A data: URL carrying HTML. | Refused, for the same reason as url/javascript_scheme. |
| `url/control_characters` | **no** | A trusted URL with an embedded newline and tab. | Refused. Control characters split a URL differently in different parsers. |
| `url/backslash` | **no** | A backslash where a URL parser may read a path separator. | Refused. Browsers and libraries disagree on which host this names. |
| `url/no_elicitation_id` | **no** | A trusted URL with an empty elicitationId. | Refused, or given a client-side id. Without one, completion cannot be correlated. |
| `url/oversized_message` | **no** | A trusted URL with a 1001-character message. | Refused, or bounded before the message reaches a page. |
| `url/oversized_url` | **no** | A trusted origin with a URL 5000 characters long. | Refused, or bounded. The origin is right; the length is not. |

| `mixed` scenario | Renders? | What it sends | What should happen |
|---|---|---|---|
| `multi/form_and_url` | yes | One form and one trusted URL in a single input-required result. | Both surfaced as one group. The call resumes only once both are answered. |
| `multi/at_ceiling` | yes | Eight forms in one result, the usual per-group ceiling. | All eight surfaced as one group. |
| `multi/over_ceiling` | **no** | Nine forms in one result. | Refused whole. A client must not park a prefix and drop the rest. |
| `multi/empty` | **no** | An input-required result carrying no requests at all. | Treated as the load-shedding signal it is: retry later or give up. It must not become an empty form or a tight retry loop. |
| `multi/one_valid_one_not` | **no** | A well-formed form beside a nested-object one. | The whole group is refused. Answering the valid half would resume a call whose other half was never asked. |
| `multi/duplicate_url` | **no** | The same trusted URL and elicitationId twice in one group. | Surfaced once, or twice, but answered without deadlock. |
| `multi/long_request_id` | **no** | One form whose server-assigned request id is 300 characters. | Refused, or bounded. The id is echoed back and stored, so its length is the client's problem. |

| `form` scenario | Renders? | What it sends | What should happen |
|---|---|---|---|
| `rounds/two` | yes | A form, then a second form after it is answered. | The second round is parked like the first. This is what proves the request state survives whatever the client does between rounds, including a reconnect. |

| `mixed` scenario | Renders? | What it sends | What should happen |
|---|---|---|---|
| `rounds/three_mixed` | yes | Form, then URL, then form. | Three parked groups in sequence for one tool call. |

| `form` scenario | Renders? | What it sends | What should happen |
|---|---|---|---|
| `rounds/reprompt` | yes | A form that is asked again whatever the client answers, ten times over. | The client stops of its own accord. A server can re-elicit forever, so the bound has to be the client's. |

| `unsupported` scenario | Renders? | What it sends | What should happen |
|---|---|---|---|
| `unsupported/sampling` | **no** | A sampling request where an elicitation would go. | Refused. A client that does not offer sampling must say so rather than ignore the request. |
| `unsupported/roots` | **no** | A roots listing request where an elicitation would go. | Refused. Roots is deprecated as of 2026-07-28 and a server-side agent has no filesystem to offer. |
| `unsupported/mixed_with_form` | **no** | A valid form beside a sampling request. | The whole group is refused, on the same reasoning as multi/one_valid_one_not. |

| `protocol_error` scenario | Renders? | What it sends | What should happen |
|---|---|---|---|
| `error/url_required` | **no** | A -32042 URL-elicitation-required error naming the trusted origin. | Surfaced as a recovery link, not as a tool failure. This is the pre-2026-07-28 way a server demands authorization, and it can arrive from any request, not only a tool call. |
| `error/url_required_untrusted` | **no** | A -32042 error naming an origin the client does not trust. | The link is dropped. The error may still be reported; the URL must not become a link. |
| `error/url_required_many` | **no** | A -32042 error carrying nine elicitations. | Bounded or refused, never nine links. |
| `error/url_required_empty` | **no** | A -32042 error with an empty elicitations array. | Reported as a plain failure. There is nothing to recover through. |
| `error/url_required_malformed` | **no** | A -32042 error whose data is not the documented shape. | Reported as a plain failure, without a parse error escaping to the user. |
| `error/url_required_no_id` | **no** | A -32042 error whose elicitation has no elicitationId. | Refused or given a client-side id, as in url/no_elicitation_id. |

### The redaction probes

`redaction/free_text` and `redaction/short_values` are the only scenarios whose
result repeats what you submitted. They exist because a client that hides
submitted values from its transcript is doing something reasonable, and the
usual way to do it — replacing the value wherever it appears in the result —
quietly destroys the rest.

Each result carries a fixed `canary` sentence and its `canary_digest`.
Re-derive the digest from the sentence you received. A mismatch means the
client rewrote the result, and whatever it damaged in the canary it damaged in
everything else the tool returned. The sentence is chosen so a substring
redaction cannot miss it: it contains `1`, `10`, `true`, and the words
`developer` and `production`, which carry the `dev` and `prod` enum values the
probe offers.

### Two of these are deliberate narrowings, not bugs

`form/multi_select` and `form/titled_enum` are legal MCP: the spec and the Go
SDK both accept an array-of-enum property and a `oneOf` const/title enum. They
are listed as refusals because a client whose renderer implements primitives
and `enumNames` only *should* refuse a shape it cannot lay out, rather than
drop the constraint and render something else. If the client under test
supports them, expect these two to pass instead — that is a finding about the
client, not a failure of the scenario.

`form/enum_integer` cuts the other way. An integer enum is inside the
elicitation subset, but the Go SDK validates the submitted content against the
schema after the client accepts and rejects a non-string enum there. So it
renders, and then a *direct*-delivery accept fails inside the SDK. Deferred
delivery is unaffected.

## What a run looks like

Ask the agent under test to work through the catalogue:

> List the scenarios from the elicitation tester and run every `url/` one in
> turn. For each, tell me whether you were shown a link, and what the link was.

Then read `exchange_log` to compare what the client displayed against what the
server sent. The interesting failures are the quiet ones: a URL that was
normalized on the way through, a schema constraint that was dropped rather than
enforced, a group of eight that arrived as seven.

## Wiring it into a client

The tester is an ordinary streamable-HTTP MCP server, so anything that can
reach a URL can drive it. For a client that reaches MCP servers through a
configured proxy list, the two things to get right are the endpoint path and
the trusted URL, and they have to agree:

- point the client at `http://<host>:8090/mcp` for the deferred path, or
  `http://<host>:8090/mcp/legacy` for `elicitation/create`;
- set the client's trusted or reauthorize URL to the same value passed to
  `-allowed-url`.

If the client runs in a container network, that URL has to be reachable from
both sides — the client resolves the origin, and a person opens the link — so a
published port on `localhost` is usually the workable choice for both.

## Security

The server holds no credentials, reaches nothing, and stores nothing durable.
It does deliberately emit hostile content: URLs on origins you do not control,
prompt-injection prose in form labels, prototype-polluting field names,
megabyte-scale schemas. That is the product. Run it against a client you are
testing, not one doing real work, and do not point `-allowed-url` at an origin
that means anything.
