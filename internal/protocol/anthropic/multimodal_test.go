package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

const sampleBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAAC0lEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

func TestBuildAnthropicUserContent_WithImage(t *testing.T) {
	msg := schema.InternalMessage{
		Role: schema.RoleUser,
		ContentBlocks: []schema.InternalContentBlock{
			{Type: "text", Text: "describe"},
			{Type: "image", Data: sampleBase64, MediaType: "image/png"},
		},
	}

	content := buildAnthropicUserContent(msg)
	var blocks []ContentBlock
	if err := json.Unmarshal(content, &blocks); err != nil {
		t.Fatalf("unmarshal content: %v", err)
	}
	if len(blocks) != 2 {
		t.Fatalf("blocks len: got %d, want 2", len(blocks))
	}
	if blocks[0].Type != "text" || blocks[0].Text != "describe" {
		t.Errorf("text block: %+v", blocks[0])
	}

	ib := blocks[1]
	if ib.Type != "image" {
		t.Fatalf("image type: got %q", ib.Type)
	}
	if ib.Source == nil {
		t.Fatal("source: nil")
	}
	if ib.Source.Type != "base64" || ib.Source.Data != sampleBase64 || ib.Source.MediaType != "image/png" {
		t.Errorf("image source: %+v", ib.Source)
	}
}

func TestBuildAnthropicUserContent_ImageURL(t *testing.T) {
	msg := schema.InternalMessage{
		Role: schema.RoleUser,
		ContentBlocks: []schema.InternalContentBlock{
			{Type: "image", URL: "https://example.com/photo.jpg", MediaType: "image/jpeg"},
		},
	}

	content := buildAnthropicUserContent(msg)
	var blocks []ContentBlock
	if err := json.Unmarshal(content, &blocks); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(blocks) != 1 || blocks[0].Type != "image" {
		t.Fatalf("blocks: %+v", blocks)
	}
	if blocks[0].Source.Type != "url" || blocks[0].Source.URL != "https://example.com/photo.jpg" {
		t.Errorf("image source: %+v", blocks[0].Source)
	}
}

func TestBuildAnthropicUserContent_FallbackText(t *testing.T) {
	content := []byte(`"hello"`)
	msg := schema.InternalMessage{
		Role:    schema.RoleUser,
		Content: content,
	}

	out := buildAnthropicUserContent(msg)
	var text string
	if err := json.Unmarshal(out, &text); err != nil || text != "hello" {
		t.Errorf("fallback: got %s", out)
	}
}