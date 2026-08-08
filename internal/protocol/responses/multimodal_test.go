package responses

import (
	"testing"

	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

const sampleBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAAC0lEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

func TestBuildResponsesContentBlocks_Image(t *testing.T) {
	blocks := []schema.InternalContentBlock{
		{Type: "text", Text: "describe"},
		{Type: "image", Data: sampleBase64, MediaType: "image/png"},
	}
	out := buildResponsesContentBlocks(blocks)
	if len(out) != 2 {
		t.Fatalf("len: got %d, want 2", len(out))
	}
	if out[0].Type != "input_text" || out[0].Text != "describe" {
		t.Errorf("text: %+v", out[0])
	}
	if out[1].Type != "input_image" {
		t.Fatalf("image type: got %q", out[1].Type)
	}
	if out[1].Source["type"] != "base64" || out[1].Source["data"] != sampleBase64 {
		t.Errorf("source: %+v", out[1].Source)
	}
}

func TestBuildResponsesContentBlocks_ImageURL(t *testing.T) {
	blocks := []schema.InternalContentBlock{
		{Type: "image", URL: "https://example.com/photo.jpg"},
	}
	out := buildResponsesContentBlocks(blocks)
	if len(out) != 1 {
		t.Fatalf("len: got %d", len(out))
	}
	if out[0].Type != "input_image" || out[0].Source["type"] != "url" {
		t.Errorf("source: %+v", out[0].Source)
	}
	if out[0].Source["url"] != "https://example.com/photo.jpg" {
		t.Errorf("url: %+v", out[0].Source)
	}
}