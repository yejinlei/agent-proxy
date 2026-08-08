package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

const imgBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAAC0lEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// TestTranslateResponse_WithImageContentBlocks 验证 Anthropic 出站响应含图片 ContentBlocks
func TestTranslateResponse_WithImageContentBlocks(t *testing.T) {
	tr := &AnthropicTranslator{APIVersion: "2023-06-01"}

	resp := &schema.InternalResponse{
		ID:    "msg-1",
		Model: "claude-3-opus",
		Choices: []schema.InternalChoice{
			{
				Index: 0,
				Message: schema.InternalMessage{
					Role: schema.RoleAssistant,
					ContentBlocks: []schema.InternalContentBlock{
						{Type: "text", Text: "image below"},
						{Type: "image", Data: imgBase64, MediaType: "image/png"},
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

	var outResp MessageResponse
	if err := json.Unmarshal(out, &outResp); err != nil {
		t.Fatalf("unmarshal: %v raw=%s", err, out)
	}
	if outResp.Role != "assistant" {
		t.Errorf("role: got %q, want assistant", outResp.Role)
	}
	if len(outResp.Content) != 2 {
		t.Fatalf("content blocks: got %d, want 2", len(outResp.Content))
	}
	if outResp.Content[0].Type != "text" || outResp.Content[0].Text != "image below" {
		t.Errorf("text block: %+v", outResp.Content[0])
	}
	ib := outResp.Content[1]
	if ib.Type != "image" || ib.Source == nil {
		t.Fatalf("image block: %+v", ib)
	}
	if ib.Source.Type != "base64" || ib.Source.Data != imgBase64 || ib.Source.MediaType != "image/png" {
		t.Errorf("image source: %+v", ib.Source)
	}
}

// TestTranslateFromProvider_WithImage 验证 Anthropic Provider 响应图片块被保留
func TestTranslateFromProvider_WithImage(t *testing.T) {
	tr := &AnthropicTranslator{APIVersion: "2023-06-01"}

	providerResp := MessageResponse{
		ID:    "msg-1",
		Type:  "message",
		Role:  "assistant",
		Model: "claude-3-opus",
		Content: []ContentBlock{
			{Type: "text", Text: "here's a pic"},
			{
				Type: "image",
				Source: &ImageSource{
					Type:      "base64",
					Data:      imgBase64,
					MediaType: "image/png",
				},
			},
		},
		StopReason: "end_turn",
		Usage:      &Usage{InputTokens: 10, OutputTokens: 20, TotalTokens: 30},
	}
	raw, _ := json.Marshal(providerResp)

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
	if msg.ContentBlocks[0].Type != "text" || msg.ContentBlocks[0].Text != "here's a pic" {
		t.Errorf("text: %+v", msg.ContentBlocks[0])
	}
	ib := msg.ContentBlocks[1]
	if ib.Type != "image" || ib.Data != imgBase64 || ib.MediaType != "image/png" {
		t.Errorf("image block: %+v", ib)
	}
}

// TestTranslateFromProviderToTranslateResponse_WithImage 端到端 round-trip
func TestTranslateFromProviderToTranslateResponse_WithImage(t *testing.T) {
	tr := &AnthropicTranslator{APIVersion: "2023-06-01"}

	providerResp := MessageResponse{
		ID:   "msg-1",
		Type: "message",
		Role: "assistant",
		Content: []ContentBlock{
			{Type: "text", Text: "generated image"},
			{
				Type: "image",
				Source: &ImageSource{
					Type:      "base64",
					Data:      imgBase64,
					MediaType: "image/png",
				},
			},
		},
		StopReason: "end_turn",
	}
	raw, _ := json.Marshal(providerResp)

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

	var outResp MessageResponse
	if err := json.Unmarshal(out, &outResp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(outResp.Content) != 2 {
		t.Fatalf("outgoing Content: got %d", len(outResp.Content))
	}
	if outResp.Content[1].Type != "image" || outResp.Content[1].Source.Data != imgBase64 {
		t.Errorf("image not preserved: %+v", outResp.Content[1])
	}
}