package gemini

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

// ── WithGeminiModel / GeminiModelFromContext ──

func TestWithGeminiModel(t *testing.T) {
	ctx := WithGeminiModel(context.Background(), "gemini-1.5-pro")

	m, ok := GeminiModelFromContext(ctx)
	if !ok {
		t.Fatal("GeminiModelFromContext returned false")
	}
	if m != "gemini-1.5-pro" {
		t.Errorf("model: got %q, want %q", m, "gemini-1.5-pro")
	}
}

func TestGeminiModelFromContext_Absent(t *testing.T) {
	_, ok := GeminiModelFromContext(context.Background())
	if ok {
		t.Error("expected absent, got ok")
	}
}

// ── TranslateRequest ──

func TestTranslateRequest_Basic(t *testing.T) {
	t0 := NewGeminiTranslator()

	raw := json.RawMessage(`{
		"model": "gemini-1.5-flash",
		"stream": true,
		"contents": [
			{"role": "user", "parts": [{"text": "hello"}]},
			{"role": "model", "parts": [{"text": "hi there"}]}
		],
		"systemInstruction": {"parts": [{"text": "be nice"}]}
	}`)

	req, err := t0.TranslateRequest(context.Background(), raw)
	if err != nil {
		t.Fatalf("TranslateRequest error: %v", err)
	}

	if req.Model != "gemini-1.5-flash" {
		t.Errorf("model: got %q, want %q", req.Model, "gemini-1.5-flash")
	}
	if req.Stream != true {
		t.Errorf("stream: got %v", req.Stream)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("messages count: got %d, want 2", len(req.Messages))
	}
	if req.Messages[0].Role != schema.RoleUser {
		t.Errorf("msg0 role: got %q", req.Messages[0].Role)
	}
	if req.Messages[1].Role != schema.RoleAssistant {
		t.Errorf("msg1 role (model→assistant): got %q", req.Messages[1].Role)
	}
	if req.Protocol != "gemini" {
		t.Errorf("protocol: got %q", req.Protocol)
	}
	if req.SystemPrompt == nil {
		t.Error("systemPrompt should be populated")
	}
}

func TestTranslateRequest_ModelFromContext(t *testing.T) {
	t0 := NewGeminiTranslator()
	raw := json.RawMessage(`{"contents": [{"role": "user", "parts": [{"text": "hi"}]}]}`)

	ctx := WithGeminiModel(context.Background(), "gemini-1.5-pro")
	req, err := t0.TranslateRequest(ctx, raw)
	if err != nil {
		t.Fatalf("TranslateRequest error: %v", err)
	}
	if req.Model != "gemini-1.5-pro" {
		t.Errorf("model (from context): got %q, want %q", req.Model, "gemini-1.5-pro")
	}
}

func TestTranslateRequest_BodyModelWinsOverContext(t *testing.T) {
	t0 := NewGeminiTranslator()
	raw := json.RawMessage(`{
		"model": "gemini-1.0-pro",
		"contents": [{"role": "user", "parts": [{"text": "hi"}]}]
	}`)

	ctx := WithGeminiModel(context.Background(), "gemini-1.5-pro")
	req, err := t0.TranslateRequest(ctx, raw)
	if err != nil {
		t.Fatalf("TranslateRequest error: %v", err)
	}
	if req.Model != "gemini-1.0-pro" {
		t.Errorf("model (body wins): got %q, want %q", req.Model, "gemini-1.0-pro")
	}
}

// ── TranslateToProvider ──

func TestTranslateToProvider_Basic(t *testing.T) {
	t0 := NewGeminiTranslator()
	iReq := &schema.InternalRequest{
		Model: "gemini-1.5-flash",
		Messages: []schema.InternalMessage{
			{Role: schema.RoleUser, Content: json.RawMessage(`"hello"`)},
		},
		Stream:      true,
		Temperature: floatPtr(0.7),
		MaxTokens:   500,
	}

	genReq, err := t0.TranslateToProvider(iReq)
	if err != nil {
		t.Fatalf("TranslateToProvider error: %v", err)
	}

	// Model and Stream are NOT set by TranslateToProvider — they are carried by the
	// downstream URL (info.Name → /v1/models/{model}:...) and the Accept header,
	// respectively. Verify they are absent in the generated request.
	if genReq.Model != "" {
		t.Errorf("Model should be empty (carried by URL): got %q", genReq.Model)
	}
	if genReq.Stream != false {
		t.Errorf("Stream should be false (carried by Accept header): got %v", genReq.Stream)
	}
	if genReq.GenerationConfig.MaxOutputTokens != 500 {
		t.Errorf("maxTokens: got %d", genReq.GenerationConfig.MaxOutputTokens)
	}
	if genReq.GenerationConfig.Temperature == nil || *genReq.GenerationConfig.Temperature != 0.7 {
		t.Errorf("temperature mismatch")
	}
	if len(genReq.Contents) != 1 {
		t.Fatalf("contents count: got %d", len(genReq.Contents))
	}
	if genReq.Contents[0].Role != "user" {
		t.Errorf("content role: got %q", genReq.Contents[0].Role)
	}
}

// ── TranslateFromProvider / TranslateResponse (round-trip) ──

func TestTranslateResponse_Basic(t *testing.T) {
	t0 := NewGeminiTranslator()

	raw := json.RawMessage(`{
		"candidates": [{
			"content": {"parts": [{"text": "hello world"}]},
			"finishReason": "STOP"
		}],
		"usageMetadata": {
			"promptTokenCount": 10,
			"candidatesTokenCount": 20,
			"totalTokenCount": 30
		}
	}`)

	resp, err := t0.TranslateFromProvider(raw)
	if err != nil {
		t.Fatalf("TranslateFromProvider error: %v", err)
	}

	if len(resp.Choices) != 1 {
		t.Fatalf("choices count: got %d", len(resp.Choices))
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Errorf("finishReason: got %q", resp.Choices[0].FinishReason)
	}
	if resp.Usage.TotalTokens != 30 {
		t.Errorf("totalTokens: got %d", resp.Usage.TotalTokens)
	}

	out, err := t0.TranslateResponse(resp)
	if err != nil {
		t.Fatalf("TranslateResponse error: %v", err)
	}
	var outGen map[string]interface{}
	if err := json.Unmarshal(out, &outGen); err != nil {
		t.Fatalf("outgoing not JSON: %v", err)
	}
	candidates, ok := outGen["candidates"].([]interface{})
	if !ok || len(candidates) == 0 {
		t.Errorf("outgoing candidates empty or wrong type")
	}
}

// ── TranslateStreamEvent (basic) ──

func TestTranslateStreamEvent_Unknown(t *testing.T) {
	t0 := NewGeminiTranslator()
	ev := t0.TranslateStreamEvent(json.RawMessage(`{"event":"test.unknown"}`))
	if ev != nil {
		t.Errorf("expected nil for unknown event, got %+v", ev)
	}
}

func floatPtr(f float64) *float64 {
	return &f
}
