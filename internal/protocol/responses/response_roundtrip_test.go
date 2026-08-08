package responses

import (
	"encoding/json"
	"testing"

	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

const respImgData = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAAC0lEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// TestTranslateResponse_WithImageContentBlocks 验证 Responses 出站响应含 output_image
func TestTranslateResponse_WithImageContentBlocks(t *testing.T) {
	tr := NewResponsesTranslator()

	resp := &schema.InternalResponse{
		ID:    "r-1",
		Model: "gpt-4o",
		Choices: []schema.InternalChoice{
			{
				Index: 0,
				Message: schema.InternalMessage{
					Role: schema.RoleAssistant,
					ContentBlocks: []schema.InternalContentBlock{
						{Type: "text", Text: "see image"},
						{Type: "image", Data: respImgData, MediaType: "image/png"},
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

	var envelope struct {
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type   string                 `json:"type"`
				Text   string                 `json:"text,omitempty"`
				Source map[string]interface{} `json:"source,omitempty"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(out, &envelope); err != nil {
		t.Fatalf("unmarshal: %v raw=%s", err, out)
	}
	if len(envelope.Output) != 1 {
		t.Fatalf("output: got %d", len(envelope.Output))
	}
	items := envelope.Output[0]
	if len(items.Content) != 2 {
		t.Fatalf("content: got %d, want 2", len(items.Content))
	}
	if items.Content[0].Type != "output_text" || items.Content[0].Text != "see image" {
		t.Errorf("text block: %+v", items.Content[0])
	}
	if items.Content[1].Type != "output_image" {
		t.Fatalf("image block type: got %q", items.Content[1].Type)
	}
	if items.Content[1].Source["type"] != "base64" || items.Content[1].Source["data"] != respImgData {
		t.Errorf("image source: %+v", items.Content[1].Source)
	}
}

// TestTranslateFromProvider_WithOutputImage 验证 Responses Provider 响应 output_image 被保留
func TestTranslateFromProvider_WithOutputImage(t *testing.T) {
	tr := NewResponsesTranslator()

	raw := json.RawMessage(`{
		"id": "r-1",
		"object": "response",
		"status": "completed",
		"model": "gpt-4o",
		"output": [{
			"type": "message",
			"role": "assistant",
			"content": [
				{"type": "output_text", "text": "here's a pic"},
				{"type": "output_image", "source": {"type": "base64", "data": "` + respImgData + `", "media_type": "image/png"}}
			]
		}],
		"usage": {"input_tokens": 10, "output_tokens": 20, "total_tokens": 30}
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
	if msg.ContentBlocks[0].Type != "text" || msg.ContentBlocks[0].Text != "here's a pic" {
		t.Errorf("text: %+v", msg.ContentBlocks[0])
	}
	ib := msg.ContentBlocks[1]
	if ib.Type != "image" || ib.Data != respImgData || ib.MediaType != "image/png" {
		t.Errorf("image block: %+v", ib)
	}
}

// TestTranslateFromProviderToTranslateResponse_WithImage 端到端 round-trip
func TestTranslateFromProviderToTranslateResponse_WithImage(t *testing.T) {
	tr := NewResponsesTranslator()

	raw := json.RawMessage(`{
		"object": "response",
		"status": "completed",
		"output": [{
			"type": "message",
			"role": "assistant",
			"content": [
				{"type": "output_text", "text": "generated image"},
				{"type": "output_image", "source": {"type": "base64", "data": "` + respImgData + `", "media_type": "image/png"}}
			]
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
		Output []struct {
			Content []struct {
				Type   string                 `json:"type"`
				Source map[string]interface{} `json:"source,omitempty"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(out, &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(envelope.Output[0].Content) != 2 {
		t.Fatalf("outgoing content: got %d", len(envelope.Output[0].Content))
	}
	if envelope.Output[0].Content[1].Type != "output_image" {
		t.Fatalf("image type: got %q", envelope.Output[0].Content[1].Type)
	}
	if envelope.Output[0].Content[1].Source["data"] != respImgData {
		t.Errorf("image data: got %v", envelope.Output[0].Content[1].Source["data"])
	}
}