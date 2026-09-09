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
	// Responses 图片走顶层 image_url（OpenAI 官方 + Codex 线格式），不是 Anthropic 的 source
	if out[1].ImageURL != "data:image/png;base64,"+sampleBase64 {
		t.Errorf("image_url: got %q", out[1].ImageURL)
	}
	if out[1].Source != nil {
		t.Errorf("image block must not carry source: %+v", out[1].Source)
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
	if out[0].Type != "input_image" || out[0].ImageURL != "https://example.com/photo.jpg" {
		t.Errorf("image_url: %+v", out[0])
	}
}

// TestBuildResponsesContentBlocks_ImageNoMediaType 缺 media_type 时兜底 image/png，
// 而不是产出 "data:;base64," 这种坏 URL
func TestBuildResponsesContentBlocks_ImageNoMediaType(t *testing.T) {
	out := buildResponsesContentBlocks([]schema.InternalContentBlock{
		{Type: "image", Data: sampleBase64},
	})
	if len(out) != 1 {
		t.Fatalf("len: got %d", len(out))
	}
	if out[0].ImageURL != "data:image/png;base64,"+sampleBase64 {
		t.Errorf("image_url: got %q", out[0].ImageURL)
	}
}

// TestBuildResponsesContentBlocks_ImageEmpty_Dropped 无 Data 无 URL 的空 image 块必须跳过
func TestBuildResponsesContentBlocks_ImageEmpty_Dropped(t *testing.T) {
	out := buildResponsesContentBlocks([]schema.InternalContentBlock{
		{Type: "text", Text: "hi"},
		{Type: "image"},
	})
	if len(out) != 1 {
		t.Fatalf("len: got %d, want 1", len(out))
	}
	if out[0].Type != "input_text" {
		t.Errorf("type: got %q", out[0].Type)
	}
}
