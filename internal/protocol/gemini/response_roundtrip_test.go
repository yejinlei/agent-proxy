package gemini

import (
	"encoding/json"
	"testing"

	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

const gemRespImgData = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAAC0lEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// TestTranslateResponse_WithImageContentBlocks 验证 Gemini 出站响应含 InlineData
func TestTranslateResponse_WithImageContentBlocks(t *testing.T) {
	tr := NewGeminiTranslator()

	resp := &schema.InternalResponse{
		ID:    "gem-1",
		Model: "gemini-1.5-pro",
		Choices: []schema.InternalChoice{
			{
				Index: 0,
				Message: schema.InternalMessage{
					Role: schema.RoleAssistant,
					ContentBlocks: []schema.InternalContentBlock{
						{Type: "text", Text: "see image"},
						{Type: "image", Data: gemRespImgData, MediaType: "image/png"},
					},
				},
				FinishReason: "stop",
			},
		},
	}

	out, err := tr.TranslateResponse(resp)
	if err != nil {
		t.Fatalf("TranslateResponse: %v", err)
	}

	// 直接解析 JSON 验证结构
	var envelope struct {
		Candidates []struct {
			Content struct {
				Role  string `json:"role"`
				Parts []struct {
					Text       string `json:"text,omitempty"`
					InlineData *struct {
						MimeType string `json:"mime_type"`
						Data     string `json:"data"`
					} `json:"inline_data,omitempty"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(out, &envelope); err != nil {
		t.Fatalf("unmarshal: %v raw=%s", err, out)
	}
	if len(envelope.Candidates) != 1 {
		t.Fatalf("candidates: got %d", len(envelope.Candidates))
	}
	c := envelope.Candidates[0]
	if c.Content.Role != "model" {
		t.Errorf("role: got %q, want model", c.Content.Role)
	}
	if len(c.Content.Parts) != 2 {
		t.Fatalf("parts: got %d, want 2", len(c.Content.Parts))
	}
	if c.Content.Parts[0].Text != "see image" {
		t.Errorf("part[0].Text: got %q", c.Content.Parts[0].Text)
	}
	if c.Content.Parts[1].InlineData == nil {
		t.Fatal("part[1].InlineData is nil")
	}
	if c.Content.Parts[1].InlineData.Data != gemRespImgData || c.Content.Parts[1].InlineData.MimeType != "image/png" {
		t.Errorf("inline_data: %+v", c.Content.Parts[1].InlineData)
	}
}

// TestTranslateFromProvider_WithInlineData 验证 Gemini Provider 响应 InlineData 被保留
func TestTranslateFromProvider_WithInlineData(t *testing.T) {
	tr := NewGeminiTranslator()

	raw := json.RawMessage(`{
		"candidates": [{
			"content": {
				"role": "model",
				"parts": [
					{"text": "generated image"},
					{"inline_data": {"mime_type": "image/png", "data": "` + gemRespImgData + `"}}
				]
			},
			"finishReason": "STOP",
			"index": 0
		}],
		"usageMetadata": {
			"promptTokenCount": 10,
			"candidatesTokenCount": 20,
			"totalTokenCount": 30
		}
	}`)

	resp, err := tr.TranslateFromProvider(raw)
	if err != nil {
		t.Fatalf("TranslateFromProvider: %v", err)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices: got %d", len(resp.Choices))
	}
	msg := resp.Choices[0].Message
	if len(msg.ContentBlocks) != 2 {
		t.Fatalf("ContentBlocks: got %d, want 2", len(msg.ContentBlocks))
	}
	if msg.ContentBlocks[0].Type != "text" || msg.ContentBlocks[0].Text != "generated image" {
		t.Errorf("text: %+v", msg.ContentBlocks[0])
	}
	ib := msg.ContentBlocks[1]
	if ib.Type != "image" || ib.Data != gemRespImgData || ib.MediaType != "image/png" {
		t.Errorf("image block: %+v", ib)
	}
}

// TestTranslateFromProviderToTranslateResponse_WithImage 端到端 round-trip
func TestTranslateFromProviderToTranslateResponse_WithImage(t *testing.T) {
	tr := NewGeminiTranslator()

	raw := json.RawMessage(`{
		"candidates": [{
			"content": {"role": "model", "parts": [
				{"text": "hello"},
				{"inline_data": {"mime_type": "image/png", "data": "` + gemRespImgData + `"}}
			]},
			"finishReason": "STOP"
		}]
	}`)

	internalResp, err := tr.TranslateFromProvider(raw)
	if err != nil {
		t.Fatalf("TranslateFromProvider: %v", err)
	}
	if len(internalResp.Choices[0].Message.ContentBlocks) != 2 {
		t.Fatalf("internal ContentBlocks: got %d", len(internalResp.Choices[0].Message.ContentBlocks))
	}

	out, err := tr.TranslateResponse(internalResp)
	if err != nil {
		t.Fatalf("TranslateResponse: %v", err)
	}

	var envelope struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					InlineData *struct {
						MimeType string `json:"mime_type"`
						Data     string `json:"data"`
					} `json:"inline_data,omitempty"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(out, &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(envelope.Candidates[0].Content.Parts) != 2 {
		t.Fatalf("outgoing parts: got %d", len(envelope.Candidates[0].Content.Parts))
	}
	if envelope.Candidates[0].Content.Parts[1].InlineData == nil {
		t.Fatal("image not preserved")
	}
	if envelope.Candidates[0].Content.Parts[1].InlineData.Data != gemRespImgData {
		t.Errorf("image data: got %q", envelope.Candidates[0].Content.Parts[1].InlineData.Data)
	}
}