package responses

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

const imgData = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAAC0lEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// TestInbound_ImageBase64 验证 Responses 入站 (input_image + base64) 解析为 ContentBlocks
func TestInbound_ImageBase64(t *testing.T) {
	tr := NewResponsesTranslator()
	items := []ContentBlock{
		{Type: "input_text", Text: "describe"},
		{
			Type: "input_image",
			Source: map[string]interface{}{
				"type":       "base64",
				"data":       imgData,
				"media_type": "image/png",
			},
		},
	}
	input := []InputItem{{
		Type:    "message",
		Role:    "user",
		Content: items,
	}}
	inputBytes, _ := json.Marshal(input)
	raw := append([]byte(`{"model":"gpt-4o","input":`), inputBytes...)
	raw = append(raw, '}')

	req, err := tr.TranslateRequest(context.Background(), raw)
	if err != nil {
		t.Fatalf("TranslateRequest: %v", err)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("messages: got %d, want 1", len(req.Messages))
	}
	msg := req.Messages[0]
	if len(msg.ContentBlocks) != 2 {
		t.Fatalf("ContentBlocks: got %d, want 2", len(msg.ContentBlocks))
	}
	tb := msg.ContentBlocks[0]
	if tb.Type != "text" || tb.Text != "describe" {
		t.Errorf("text block: %+v", tb)
	}
	ib := msg.ContentBlocks[1]
	if ib.Type != "image" || ib.Data != imgData || ib.MediaType != "image/png" {
		t.Errorf("image block: %+v", ib)
	}
}

// TestInbound_ImageURL 验证 Responses 入站 (input_image + url)
func TestInbound_ImageURL(t *testing.T) {
	tr := NewResponsesTranslator()
	items := []ContentBlock{
		{
			Type: "input_image",
			Source: map[string]interface{}{
				"type": "url",
				"url":  "https://example.com/photo.jpg",
			},
		},
	}
	input := []InputItem{{Type: "message", Role: "user", Content: items}}
	inputBytes, _ := json.Marshal(input)
	raw := append([]byte(`{"model":"gpt-4o","input":`), inputBytes...)
	raw = append(raw, '}')

	req, err := tr.TranslateRequest(context.Background(), raw)
	if err != nil {
		t.Fatalf("TranslateRequest: %v", err)
	}
	ib := req.Messages[0].ContentBlocks[0]
	if ib.Type != "image" || ib.URL != "https://example.com/photo.jpg" {
		t.Errorf("image block: %+v", ib)
	}
}

// TestOutbound_BuildInputArray_WithImage 验证 Responses 出站从 ContentBlocks 生成 input_image
func TestOutbound_BuildInputArray_WithImage(t *testing.T) {
	tr := NewResponsesTranslator()
	req := &schema.InternalRequest{
		Model: "gpt-4o",
		Messages: []schema.InternalMessage{
			{
				Role: schema.RoleUser,
				ContentBlocks: []schema.InternalContentBlock{
					{Type: "text", Text: "describe"},
					{Type: "image", Data: imgData, MediaType: "image/png"},
				},
			},
		},
	}

	respReq, err := tr.TranslateToProvider(req)
	if err != nil {
		t.Fatalf("TranslateToProvider: %v", err)
	}
	inputItems := InputToItems(respReq.Input)
	if len(inputItems) != 1 {
		t.Fatalf("input items: got %d, want 1", len(inputItems))
	}
	inputItem := inputItems[0]
	blocks := inputItem.Content.([]ContentBlock)
	if len(blocks) != 2 {
		t.Fatalf("content blocks: got %d, want 2", len(blocks))
	}
	if blocks[0].Type != "input_text" || blocks[0].Text != "describe" {
		t.Errorf("text block: %+v", blocks[0])
	}
	if blocks[1].Type != "input_image" {
		t.Fatalf("image block type: got %q", blocks[1].Type)
	}
	if blocks[1].Source["type"] != "base64" || blocks[1].Source["data"] != imgData {
		t.Errorf("source: %+v", blocks[1].Source)
	}
}