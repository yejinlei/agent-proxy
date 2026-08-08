package chatcompletion

import (
	"encoding/json"
	"testing"

	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

const sampleBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAAC0lEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

func TestMessageToInternal_ImageDataURL(t *testing.T) {
	raw := []byte(`[
		{"type":"text","text":"describe this"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,` + sampleBase64 + `","detail":"low"}}
	]`)
	content := Content{raw: raw}
	msg := Message{
		Role:    "user",
		Content: content,
	}

	im := messageToInternal(msg)
	if im.Role != schema.RoleUser {
		t.Fatalf("role: got %v, want user", im.Role)
	}
	if len(im.ContentBlocks) != 2 {
		t.Fatalf("ContentBlocks: got %d blocks, want 2", len(im.ContentBlocks))
	}

	tb := im.ContentBlocks[0]
	if tb.Type != "text" || tb.Text != "describe this" {
		t.Errorf("text block: %+v", tb)
	}

	ib := im.ContentBlocks[1]
	if ib.Type != "image" {
		t.Fatalf("image block type: got %q", ib.Type)
	}
	if ib.Data != sampleBase64 {
		t.Errorf("image data: got %q, want %q", ib.Data, sampleBase64)
	}
	if ib.MediaType != "image/png" {
		t.Errorf("media_type: got %q", ib.MediaType)
	}
	if ib.URL != "" {
		t.Errorf("url should be empty for data URL: got %q", ib.URL)
	}
}

func TestMessageToInternal_ImageExternalURL(t *testing.T) {
	raw := []byte(`[
		{"type":"text","text":"hello"},
		{"type":"image_url","image_url":{"url":"https://example.com/photo.jpg"}}
	]`)
	msg := Message{Role: "user", Content: Content{raw: raw}}
	im := messageToInternal(msg)

	if len(im.ContentBlocks) != 2 {
		t.Fatalf("ContentBlocks len: got %d, want 2", len(im.ContentBlocks))
	}
	ib := im.ContentBlocks[1]
	if ib.URL != "https://example.com/photo.jpg" {
		t.Errorf("url: got %q", ib.URL)
	}
	if ib.Data != "" {
		t.Errorf("data should be empty for external URL: got %q", ib.Data)
	}
	if ib.MediaType != "" {
		t.Errorf("media_type: got %q, want empty", ib.MediaType)
	}
}

func TestMessageToInternal_PureString(t *testing.T) {
	raw := []byte(`"hello world"`)
	msg := Message{Role: "user", Content: Content{raw: raw}}
	im := messageToInternal(msg)

	if len(im.ContentBlocks) != 0 {
		t.Errorf("ContentBlocks should be nil for plain string content, got %d", len(im.ContentBlocks))
	}
	var text string
	if err := json.Unmarshal(im.Content, &text); err != nil || text != "hello world" {
		t.Errorf("Content fallback: got %q, want hello world", text)
	}
}

func TestParseCCContentBlocks_IgnoreUnknown(t *testing.T) {
	raw := []json.RawMessage{
		json.RawMessage(`{"type":"text","text":"a"}`),
		json.RawMessage(`{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,abc"}}`),
		json.RawMessage(`{"type":"tool_use","id":"t1"}`),
	}
	blocks := ParseCCContentBlocks(raw)
	if len(blocks) != 2 {
		t.Fatalf("got %d blocks, want 2", len(blocks))
	}
	if blocks[0].Type != "text" || blocks[0].Text != "a" {
		t.Errorf("text block: %+v", blocks[0])
	}
	if blocks[1].Type != "image" || blocks[1].Data != "abc" {
		t.Errorf("image block: %+v", blocks[1])
	}
}