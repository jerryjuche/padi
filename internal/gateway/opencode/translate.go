package opencode

import (
	"encoding/json"
)

// Event is the gateway's normalised event. Unknown OpenCode events are
// dropped by Translate, never allowed to crash the relay.
type Event struct {
	Type      string          `json:"type"`
	SessionID string          `json:"session_id,omitempty"`
	Text      string          `json:"text,omitempty"`
	Tool      string          `json:"tool,omitempty"`
	Message   string          `json:"message,omitempty"`
	Terminal  bool            `json:"terminal,omitempty"`
	Raw       json.RawMessage `json:"raw,omitempty"`
}

// field looks in the event's data and properties objects first, then the
// envelope. OpenCode v2 puts payload fields under "data"; older frames and
// some plugin events use top-level or "properties" fields.
func field(data json.RawMessage, keys ...string) string {
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return ""
	}
	scopes := make([]map[string]any, 0, 3)
	for _, key := range []string{"data", "properties"} {
		if obj, ok := root[key].(map[string]any); ok {
			scopes = append(scopes, obj)
		}
	}
	scopes = append(scopes, root)
	for _, obj := range scopes {
		if value := lookup(obj, keys...); value != "" {
			return value
		}
	}
	return ""
}

func lookup(obj map[string]any, keys ...string) string {
	for _, key := range keys {
		value, ok := obj[key]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case string:
			return typed
		case map[string]any:
			for _, nested := range []string{"name", "tool", "message", "id"} {
				if s, ok := typed[nested].(string); ok {
					return s
				}
			}
		}
	}
	return ""
}

// Translate maps a raw OpenCode v2 event to a gateway event.
func Translate(raw RawEvent) (Event, bool) {
	typ := raw.Event
	if typ == "" {
		var probe struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw.Data, &probe) == nil {
			typ = probe.Type
		}
	}
	sessionID := field(raw.Data, "sessionID", "session_id")
	e := Event{Type: typ, SessionID: sessionID, Raw: raw.Data}
	switch typ {
	case "session.text.delta":
		e.Type = "token"
		e.Text = field(raw.Data, "delta", "text")
	case "session.text.started":
		e.Type = "status"
		e.Message = "Working..."
	case "session.text.ended", "session.idle":
		e.Type = "done"
		e.Terminal = true
	case "session.execution.failed", "session.step.failed":
		e.Type = "error"
		e.Terminal = true
		e.Message = field(raw.Data, "error", "message", "name")
		if e.Message == "" {
			e.Message = "The agent stopped with an error."
		}
	case "session.tool.failed":
		e.Type = "error"
		e.Message = field(raw.Data, "error", "message", "name")
		if e.Message == "" {
			e.Message = "A tool failed."
		}
	case "session.tool.called", "session.tool.input.ended":
		e.Type = "tool_call"
		e.Tool = field(raw.Data, "tool", "name", "toolName")
	case "session.tool.success":
		e.Type = "tool_result"
		e.Tool = field(raw.Data, "tool", "name", "toolName")
	case "filesystem.changed":
		e.Type = "file_change"
		e.Text = field(raw.Data, "path", "file")
	case "session.status":
		e.Type = "status"
		e.Message = field(raw.Data, "status")
	default:
		return Event{}, false
	}
	return e, true
}
