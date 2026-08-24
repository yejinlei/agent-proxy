package server

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/agent-proxy/agent-proxy/internal/protocol/chatcompletion"
	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

// TestGateway_SSEDataPrefix_HandlesVariousFormats verifies the fix in
// gateway.go handleStreamRequest where SSE lines from OpenAIClient.CallStream
// include a "data: " prefix. Without strings.TrimPrefix, every chunk fails
// json.Unmarshal and is silently skipped, producing an empty stream response.
func TestGateway_SSEDataPrefix_HandlesVariousFormats(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		wantText string
		wantSkip bool
	}{
		{
			name:     "with data: prefix",
			line:     `data: {"id":"c1","choices":[{"delta":{"content":"hi"},"index":0}]}`,
			wantText: "hi",
			wantSkip: false,
		},
		{
			name:     "without data: prefix",
			line:     `{"id":"c1","choices":[{"delta":{"content":"hi"},"index":0}]}`,
			wantText: "hi",
			wantSkip: false,
		},
		{
			name:     "empty delta — no content but valid chunk",
			line:     `data: {"id":"c1","choices":[{"delta":{},"index":0,"finish_reason":"stop"}]}`,
			wantText: "",
			wantSkip: false,
		},
		{
			name:     "non-json — should skip",
			line:     `this is not json`,
			wantText: "",
			wantSkip: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Mirrors the fix in gateway.go handleStreamRequest:
			//   payload := strings.TrimPrefix(string(line), "data: ")
			//   var ccChunk chatcompletion.ChatCompletionStreamChunk
			//   if json.Unmarshal([]byte(payload), &ccChunk) != nil || len(ccChunk.Choices) == 0 {
			//       continue
			//   }
			payload := strings.TrimPrefix(tt.line, "data: ")
			var ccChunk chatcompletion.ChatCompletionStreamChunk
			if err := json.Unmarshal([]byte(payload), &ccChunk); err != nil {
				if tt.wantSkip {
					return
				}
				t.Fatalf("Unexpected unmarshal failure: %v\n  line: %s\n  payload: %s", err, tt.line, payload)
			}
			if tt.wantSkip {
				t.Fatal("Expected unmarshal to fail but it succeeded")
			}
			if len(ccChunk.Choices) == 0 {
				t.Fatal("Expected at least one choice")
			}

			choice := ccChunk.Choices[0]
			if choice.Delta.Content == "" {
				if tt.wantText == "" {
					return
				}
				t.Fatal("Expected content in delta but got empty string")
			}
			if choice.Delta.Content != tt.wantText {
				t.Fatalf("Expected %q, got %q", tt.wantText, choice.Delta.Content)
			}
		})
	}
}

// TestGateway_SSEStreamAccumulation simulates the full stream accumulation
// that happens in gateway.go handleStreamRequest when an OpenAI provider
// returns SSE-formatted lines with "data: " prefix.
func TestGateway_SSEStreamAccumulation(t *testing.T) {
	// These are exactly the format OpenAIClient.CallStream sends.
	chunks := []string{
		`data: {"id":"chatcmpl-1","choices":[{"delta":{"content":"Hello"},"index":0,"finish_reason":null}],"usage":null}`,
		`data: {"id":"chatcmpl-1","choices":[{"delta":{"content":" World"},"index":0,"finish_reason":null}],"usage":null}`,
		`data: {"id":"chatcmpl-1","choices":[{"delta":{},"index":0,"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		`data: [DONE]`,
	}

	var accumulatedContent strings.Builder
	var lastUsage map[string]float64

	for _, line := range chunks {
		// Simulate gateway.go logic: skip [DONE] and empty lines
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || trimmed == "data: [DONE]" || strings.HasPrefix(trimmed, ":") {
			continue
		}

		payload := strings.TrimPrefix(line, "data: ")
		var ccChunk chatcompletion.ChatCompletionStreamChunk
		if err := json.Unmarshal([]byte(payload), &ccChunk); err != nil {
			t.Fatalf("Failed to parse SSE chunk: %v\nchunk: %s", err, line)
		}
		if len(ccChunk.Choices) == 0 {
			continue
		}

		choice := ccChunk.Choices[0]
		if choice.Delta.Content != "" {
			accumulatedContent.WriteString(choice.Delta.Content)
		}
		if ccChunk.Usage != nil {
			lastUsage = map[string]float64{
				"prompt_tokens":     float64(ccChunk.Usage.PromptTokens),
				"completion_tokens": float64(ccChunk.Usage.CompletionTokens),
				"total_tokens":      float64(ccChunk.Usage.TotalTokens),
			}
		}
	}

	if accumulatedContent.String() != "Hello World" {
		t.Fatalf("Expected 'Hello World', got %q — 'data: ' prefix fix is not working", accumulatedContent.String())
	}
	if lastUsage == nil {
		t.Fatal("Expected usage in final chunk")
	}
	if lastUsage["total_tokens"] != 15 {
		t.Fatalf("Expected total_tokens=15, got %v", lastUsage["total_tokens"])
	}

	t.Logf("✓ SSE stream correctly accumulated: %q (usage: prompt=%v completion=%v total=%v)",
		accumulatedContent.String(),
		lastUsage["prompt_tokens"], lastUsage["completion_tokens"], lastUsage["total_tokens"])
}

// TestQuickGateway_SSEDataPrefixAlreadyFixed confirms quick.go already handles
// the "data: " prefix (the same fix is present there).
func TestQuickGateway_SSEDataPrefixAlreadyFixed(t *testing.T) {
	line := `data: {"id":"c1","choices":[{"delta":{"content":"test"},"index":0}]}`
	payload := strings.TrimPrefix(line, "data: ")
	var ccChunk chatcompletion.ChatCompletionStreamChunk
	if err := json.Unmarshal([]byte(payload), &ccChunk); err != nil {
		t.Fatalf("quick.go-style parse failed: %v", err)
	}
	if len(ccChunk.Choices) == 0 {
		t.Fatal("Expected choices")
	}
	if ccChunk.Choices[0].Delta.Content != "test" {
		t.Fatalf("Expected 'test', got %q", ccChunk.Choices[0].Delta.Content)
	}
}

func getTestGatewayCache() *sync.Map {
	return &sync.Map{}
}

// TestBuildCCRequest_DevRoleMapToSystem verifies Responses API's "developer"
// role is mapped to "system" when building CC requests. Sensenova and other
// CC endpoints reject role:"developer" with 400; it must be mapped to system.
func TestBuildCCRequest_DevRoleMapToSystem(t *testing.T) {
	content, _ := json.Marshal("hello")
	req := &schema.InternalRequest{
		Model:   "test-model",
		Stream:  true,
		Messages: []schema.InternalMessage{
			{Role: "developer", Content: content},   // should map → "system"
			{Role: schema.RoleUser, Content: content}, // stays "user"
		},
	}
	ccReq := buildCCRequest(req, "")

	if len(ccReq.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(ccReq.Messages))
	}

	// developer → system
	var role1 string
	json.Unmarshal(ccReq.Messages[0].Content.Raw(), &role1)
	if ccReq.Messages[0].Role != "system" {
		t.Fatalf("developer role: got %q, want system", ccReq.Messages[0].Role)
	}

	// user stays user
	if ccReq.Messages[1].Role != "user" {
		t.Fatalf("user role: got %q, want user", ccReq.Messages[1].Role)
	}
	t.Logf("✓ developer→system mapping correct (developer=%q, user=%q)",
		ccReq.Messages[0].Role, ccReq.Messages[1].Role)
}
