package mcpserver

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Exchange is one round: what the server asked for and, once the client comes
// back, what it answered.
//
// The log is the read-back a client's own transcript cannot give. A client
// that redacts submitted values from its transcript is behaving correctly, and
// leaves nobody able to confirm the values arrived at all; reading the log in
// a second tool call answers that without putting the values back in front of
// the model.
type Exchange struct {
	At        time.Time
	Tool      string
	Scenario  string
	Round     int
	Delivery  string
	Requested []RequestRecord
	Answered  []loggedResponse
	Note      string
}

// RequestRecord is what went out, in the shape the client saw it.
type RequestRecord struct {
	RequestID string
	Method    string
	Mode      string
	Message   string
	URL       string
	Schema    json.RawMessage
}

type loggedResponse struct {
	RequestID string
	Method    string
	Action    string
	Values    map[string]json.RawMessage
}

// exchangeLog is a fixed-size ring. Nothing here is durable: restarting the
// server is how you clear it, and reset_log is how you clear it without.
type exchangeLog struct {
	mu      sync.Mutex
	size    int
	entries []Exchange
}

func newExchangeLog(size int) *exchangeLog {
	return &exchangeLog{size: size}
}

func (l *exchangeLog) append(entry Exchange) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry.At = time.Now().UTC()
	l.entries = append(l.entries, entry)
	if len(l.entries) > l.size {
		l.entries = l.entries[len(l.entries)-l.size:]
	}
}

// recent returns up to limit entries, newest first.
func (l *exchangeLog) recent(limit int) []Exchange {
	l.mu.Lock()
	defer l.mu.Unlock()
	if limit <= 0 || limit > len(l.entries) {
		limit = len(l.entries)
	}
	out := make([]Exchange, 0, limit)
	for i := len(l.entries) - 1; i >= len(l.entries)-limit; i-- {
		out = append(out, l.entries[i])
	}
	return out
}

func (l *exchangeLog) reset() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := len(l.entries)
	l.entries = nil
	return n
}

// describeRequests records what the server put on the wire, so a mismatch
// between what was sent and what a client rendered is visible here.
func describeRequests(reqs mcp.InputRequestMap) []RequestRecord {
	out := make([]RequestRecord, 0, len(reqs))
	for _, id := range sortedKeys(reqs) {
		record := RequestRecord{RequestID: id}
		switch params := reqs[id].(type) {
		case *mcp.ElicitParams:
			record.Method = "elicitation/create"
			record.Mode = params.Mode
			record.Message = params.Message
			record.URL = params.URL
			if raw, ok := params.RequestedSchema.(json.RawMessage); ok {
				record.Schema = raw
			} else if params.RequestedSchema != nil {
				if encoded, err := json.Marshal(params.RequestedSchema); err == nil {
					record.Schema = encoded
				}
			}
		case *mcp.CreateMessageParams, *mcp.CreateMessageWithToolsParams:
			record.Method = "sampling/createMessage"
		case *mcp.ListRootsParams:
			record.Method = "roots/list"
		default:
			record.Method = fmt.Sprintf("unknown (%T)", params)
		}
		out = append(out, record)
	}
	return out
}

// describeResponses reduces the client's answers to what the log keeps: the
// action, the field names, and each value's JSON encoding.
func describeResponses(responses mcp.InputResponseMap) []loggedResponse {
	out := make([]loggedResponse, 0, len(responses))
	for _, id := range sortedResponseKeys(responses) {
		entry := loggedResponse{RequestID: id}
		switch response := responses[id].(type) {
		case *mcp.ElicitResult:
			entry.Method = "elicitation/create"
			entry.Action = response.Action
			entry.Values = make(map[string]json.RawMessage, len(response.Content))
			for name, value := range response.Content {
				encoded, err := json.Marshal(value)
				if err != nil {
					encoded = json.RawMessage(`"<unencodable>"`)
				}
				entry.Values[name] = encoded
			}
		default:
			entry.Method = fmt.Sprintf("unknown (%T)", response)
		}
		out = append(out, entry)
	}
	return out
}

// describeErrorElicitations reads back what a -32042 error carried, so the log
// shows the URL that went out even though it travelled as error data rather
// than as an input request. A payload that does not decode records nothing,
// which is itself what the malformed scenario is for.
func describeErrorElicitations(data json.RawMessage) []RequestRecord {
	var payload struct {
		Elicitations []*mcp.ElicitParams `json:"elicitations"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return nil
	}
	reqs := make(mcp.InputRequestMap, len(payload.Elicitations))
	for i, item := range payload.Elicitations {
		reqs[fmt.Sprintf("e%d", i+1)] = item
	}
	return describeRequests(reqs)
}
