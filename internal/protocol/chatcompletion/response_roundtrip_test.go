package chatcompletion

import (
	"encoding/json"
	"testing"

	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

const respImgData = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAAC0lEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// TestInternalToCCResponse_WithImage 验证 CC 出站响应含图片 content block
func TestInternalToCCResponse_WithImage(t *testing.T) {
	resp := &schema.InternalResponse{
		ID:    "r-1",
		Model: "gpt-4o",
		Choices: []schema.InternalChoice{
			{
				Index: 0,
				Message: schema.InternalMessage{
					Role: schema.RoleAssistant,
					ContentBlocks: []schema.InternalContentBlock{
						{Type: "text", Text: "here's the image"},
						{Type: "image", Data: respImgData, MediaType: "image/png"},
					},
				},
				FinishReason: "stop",
			},
		},
	}

	ccResp := InternalToCCResponse(resp)
	raw, err := json.Marshal(ccResp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var envelope struct {
		Choices []struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(envelope.Choices) != 1 {
		t.Fatalf("choices: got %d", len(envelope.Choices))
	}

	var blocks []map[string]interface{}
	if err := json.Unmarshal(envelope.Choices[0].Message.Content, &blocks); err != nil {
		t.Fatalf("content not a block array: got %s, err=%v", envelope.Choices[0].Message.Content, err)
	}
	if len(blocks) != 2 {
		t.Fatalf("blocks len: got %d, want 2", len(blocks))
	}
	if blocks[0]["type"] != "text" {
		t.Errorf("block[0].type: got %v", blocks[0]["type"])
	}
	if blocks[1]["type"] != "image_url" {
		t.Errorf("block[1].type: got %v", blocks[1]["type"])
	}
	imgURL := blocks[1]["image_url"].(map[string]interface{})["url"].(string)
	if imgURL != "data:image/png;base64,"+respImgData {
		t.Errorf("image url: got %q", imgURL)
	}
}

// TestInternalToCCResponse_PureText 纯文本仍输出字符串
func TestInternalToCCResponse_PureText(t *testing.T) {
	resp := &schema.InternalResponse{
		Model: "gpt-4o",
		Choices: []schema.InternalChoice{
			{
				Message: schema.InternalMessage{
					Role:    schema.RoleAssistant,
					Content: func() json.RawMessage { b, _ := json.Marshal("hello"); return b }(),
				},
				FinishReason: "stop",
			},
		},
	}

	ccResp := InternalToCCResponse(resp)
	var out struct {
		Choices []struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	raw, _ := json.Marshal(ccResp)
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var s string
	if err := json.Unmarshal(out.Choices[0].Message.Content, &s); err != nil {
		t.Fatalf("content not a string: err=%v raw=%s", err, out.Choices[0].Message.Content)
	}
	if s != "hello" {
		t.Errorf("content: got %q, want hello", s)
	}
}