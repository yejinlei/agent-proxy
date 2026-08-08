package gemini

import (
	"encoding/json"
	"testing"

	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

const geminiImgData = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAAC0lEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// TestContentsToMessages_InlineData 验证 Gemini 入站 inline_data 解析为 ContentBlocks
func TestContentsToMessages_InlineData(t *testing.T) {
	contents := []Content{
		{
			Role: "user",
			Parts: []Part{
				{Text: "describe"},
				{InlineData: &InlineData{MimeType: "image/png", Data: geminiImgData}},
			},
		},
	}

	msgs := contentsToMessages(contents)
	if len(msgs) != 1 {
		t.Fatalf("msgs: got %d, want 1", len(msgs))
	}
	msg := msgs[0]
	if len(msg.ContentBlocks) != 2 {
		t.Fatalf("ContentBlocks: got %d, want 2", len(msg.ContentBlocks))
	}

	tb := msg.ContentBlocks[0]
	if tb.Type != "text" || tb.Text != "describe" {
		t.Errorf("text block: %+v", tb)
	}
	ib := msg.ContentBlocks[1]
	if ib.Type != "image" || ib.Data != geminiImgData || ib.MediaType != "image/png" {
		t.Errorf("image block: %+v", ib)
	}
}

// TestMessagesToGemini_UserImage 验证 Gemini 出站从 ContentBlocks 生成 InlineData
func TestMessagesToGemini_UserImage(t *testing.T) {
	msgs := []schema.InternalMessage{
		{
			Role: schema.RoleUser,
			ContentBlocks: []schema.InternalContentBlock{
				{Type: "text", Text: "describe"},
				{Type: "image", Data: geminiImgData, MediaType: "image/png"},
			},
		},
	}

	contents, err := messagesToGemini(msgs)
	if err != nil {
		t.Fatalf("messagesToGemini: %v", err)
	}
	if len(contents) != 1 {
		t.Fatalf("contents: got %d, want 1", len(contents))
	}
	c := contents[0]
	if c.Role != "user" {
		t.Errorf("role: got %q, want user", c.Role)
	}
	if len(c.Parts) != 2 {
		t.Fatalf("parts: got %d, want 2", len(c.Parts))
	}
	if c.Parts[0].Text != "describe" {
		t.Errorf("part[0].Text: got %q", c.Parts[0].Text)
	}
	if c.Parts[1].InlineData == nil {
		t.Fatal("part[1].InlineData is nil")
	}
	if c.Parts[1].InlineData.Data != geminiImgData || c.Parts[1].InlineData.MimeType != "image/png" {
		t.Errorf("inline_data: %+v", c.Parts[1].InlineData)
	}
}

// TestMessagesToGemini_FallbackText 验证回退到 Content 字符串
func TestMessagesToGemini_FallbackText(t *testing.T) {
	content, _ := json.Marshal("hello")
	msgs := []schema.InternalMessage{
		{Role: schema.RoleUser, Content: content},
	}

	contents, err := messagesToGemini(msgs)
	if err != nil {
		t.Fatalf("messagesToGemini: %v", err)
	}
	if len(contents) != 1 || len(contents[0].Parts) != 1 {
		t.Fatalf("contents: %+v", contents)
	}
	if contents[0].Parts[0].Text != "hello" {
		t.Errorf("text: got %q", contents[0].Parts[0].Text)
	}
}