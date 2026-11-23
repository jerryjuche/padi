package opencode

import (
	"encoding/json"
	"testing"
)

func TestTranslate(t *testing.T) {
	cases := []struct {
		name         string
		raw          RawEvent
		wantType     string
		wantText     string
		wantTool     string
		wantTerminal bool
		wantDropped  bool
	}{
		{"token", RawEvent{"session.text.delta", json.RawMessage(`{"data":{"sessionID":"s1","delta":"hello"}}`)}, "token", "hello", "", false, false},
		{"done", RawEvent{"session.idle", json.RawMessage(`{"data":{"sessionID":"s1"}}`)}, "done", "", "", true, false},
		{"execution failed", RawEvent{"session.execution.failed", json.RawMessage(`{"data":{"sessionID":"s1","error":"boom"}}`)}, "error", "", "", true, false},
		{"step failed", RawEvent{"session.step.failed", json.RawMessage(`{"data":{"sessionID":"s1","error":"boom"}}`)}, "error", "", "", true, false},
		{"tool failed", RawEvent{"session.tool.failed", json.RawMessage(`{"data":{"sessionID":"s1","error":"retry"}}`)}, "error", "", "", false, false},
		{"tool call", RawEvent{"session.tool.called", json.RawMessage(`{"data":{"tool":{"name":"shell"}}}`)}, "tool_call", "", "shell", false, false},
		{"file change", RawEvent{"filesystem.changed", json.RawMessage(`{"data":{"path":"README.md"}}`)}, "file_change", "README.md", "", false, false},
		{"unknown dropped", RawEvent{"plugin.updated", json.RawMessage(`{}`)}, "", "", "", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Translate(tc.raw)
			if ok == tc.wantDropped {
				t.Fatalf("ok=%v wantDropped=%v", ok, tc.wantDropped)
			}
			if tc.wantDropped {
				return
			}
			if got.Type != tc.wantType || got.Text != tc.wantText || got.Tool != tc.wantTool || got.Terminal != tc.wantTerminal {
				t.Fatalf("got=%+v want type=%q text=%q tool=%q terminal=%v", got, tc.wantType, tc.wantText, tc.wantTool, tc.wantTerminal)
			}
		})
	}
}

func TestTranslateUsesPayloadTypeWhenFrameTypeEmpty(t *testing.T) {
	ev, ok := Translate(RawEvent{"", json.RawMessage(`{"type":"session.idle","data":{"sessionID":"s9"}}`)})
	if !ok || ev.Type != "done" || ev.SessionID != "s9" {
		t.Fatalf("ev=%+v ok=%v", ev, ok)
	}
}
