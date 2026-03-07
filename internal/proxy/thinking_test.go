package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRewriteModelForCodebuff(t *testing.T) {
	tests := []struct {
		name  string
		model string
		want  string
	}{
		{
			name:  "clean claude model",
			model: "claude-opus-4-6",
			want:  "codebuff/anthropic/claude-opus-4-6",
		},
		{
			name:  "full provider model",
			model: "anthropic/claude-opus-4-6",
			want:  "codebuff/anthropic/claude-opus-4-6",
		},
		{
			name:  "legacy dot alias",
			model: "claude-opus-4.6",
			want:  "codebuff/anthropic/claude-opus-4-6",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(`{"model":"` + tt.model + `"}`)
			got := rewriteModelForCodebuff(body)

			var payload map[string]string
			if err := json.Unmarshal(got, &payload); err != nil {
				t.Fatalf("unmarshal result: %v", err)
			}
			if payload["model"] != tt.want {
				t.Fatalf("model = %q, want %q", payload["model"], tt.want)
			}
		})
	}
}

func TestCodebuffModelEntries_CodebuffSessionUsesCleanClaudeIDs(t *testing.T) {
	entries := codebuffModelEntries(true)
	if len(entries) == 0 {
		t.Fatal("expected claudevibe model entries")
	}

	for _, entry := range entries {
		var payload struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(entry, &payload); err != nil {
			t.Fatalf("unmarshal entry: %v", err)
		}
		if payload.ID == "" {
			t.Fatal("expected non-empty model id")
		}
		if !strings.HasPrefix(payload.ID, "claude-") {
			t.Fatalf("expected clean Claude model id, got %q", payload.ID)
		}
	}
}

func TestAppendUniqueModelEntries_SkipsDuplicateIDs(t *testing.T) {
	existing := []json.RawMessage{
		json.RawMessage(`{"id":"claude-opus-4-6"}`),
	}
	additional := []json.RawMessage{
		json.RawMessage(`{"id":"claude-opus-4-6"}`),
		json.RawMessage(`{"id":"claude-sonnet-4-6"}`),
	}

	merged := appendUniqueModelEntries(existing, additional)
	if len(merged) != 2 {
		t.Fatalf("merged length = %d, want 2", len(merged))
	}
}
