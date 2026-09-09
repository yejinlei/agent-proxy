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
	if got := blocks[1].ImageURL; got != "data:image/png;base64,"+imgData {
		t.Errorf("image_url: got %q, want data:image/png;base64,<imgData>", got)
	}
	if blocks[1].Source != nil {
		t.Errorf("image block must not carry Anthropic-style source: %+v", blocks[1].Source)
	}
}

// TestInbound_ImageURL_CodexFormat 验证 Codex CLI 的 input_image 形状能被解析，
// 而不是静默丢图。image_url 在块顶层（非 source 嵌套），data URL 带 media type，
// 另带 detail 字段。Codex 线格式见 codex-rs/protocol/src/models.rs ContentItem::InputImage。
// 用原始 JSON 而非构造 ContentBlock，因为线上入站本来就是 map[string]interface{}。
func TestInbound_ImageURL_CodexFormat(t *testing.T) {
	tr := NewResponsesTranslator()
	raw := []byte(`{"model":"gpt-4o","input":[{"type":"message","role":"user","content":[` +
		`{"type":"input_text","text":"describe this"},` +
		`{"type":"input_image","image_url":"data:image/png;base64,` + imgData + `","detail":"high"}` +
		`]}]}`)

	req, err := tr.TranslateRequest(context.Background(), raw)
	if err != nil {
		t.Fatalf("TranslateRequest: %v", err)
	}
	msg := req.Messages[0]
	if len(msg.ContentBlocks) != 2 {
		t.Fatalf("ContentBlocks: got %d, want 2", len(msg.ContentBlocks))
	}
	ib := msg.ContentBlocks[1]
	if ib.Type != "image" || ib.Data != imgData || ib.MediaType != "image/png" {
		t.Errorf("image block: %+v", ib)
	}
}

// TestInbound_ImageURL_CodexPlainURL 验证 Codex 顶层 image_url 是普通 URL 时也走 URL 分支
func TestInbound_ImageURL_CodexPlainURL(t *testing.T) {
	tr := NewResponsesTranslator()
	raw := []byte(`{"model":"gpt-4o","input":[{"type":"message","role":"user","content":[` +
		`{"type":"input_image","image_url":"https://example.com/photo.jpg"}` + `]}]}`)

	req, err := tr.TranslateRequest(context.Background(), raw)
	if err != nil {
		t.Fatalf("TranslateRequest: %v", err)
	}
	ib := req.Messages[0].ContentBlocks[0]
	if ib.Type != "image" || ib.URL != "https://example.com/photo.jpg" || ib.Data != "" {
		t.Errorf("image block: %+v", ib)
	}
}

// TestInbound_ImageNoData_Skipped 验证既无 source 又无 image_url 的空 input_image 被丢弃
// 而不是写成空 image 块（空块出站无信息量，还会让上游收到一个必然失败的图引用）。
func TestInbound_ImageNoData_Skipped(t *testing.T) {
	tr := NewResponsesTranslator()
	raw := []byte(`{"model":"gpt-4o","input":[{"type":"message","role":"user","content":[` +
		`{"type":"input_text","text":"hi"},` + `{"type":"input_image"}` + `]}]}`)

	req, err := tr.TranslateRequest(context.Background(), raw)
	if err != nil {
		t.Fatalf("TranslateRequest: %v", err)
	}
	msg := req.Messages[0]
	if len(msg.ContentBlocks) != 1 || msg.ContentBlocks[0].Type != "text" {
		t.Errorf("empty image block must be dropped, got %+v", msg.ContentBlocks)
	}
}

// TestInbound_ImageSourceAndImageURL_Both 两种形状同时存在时 image_url 优先
// （Codex 分支后于 source 分支执行，故意保留这个顺序让顶层字段覆盖嵌套字段）
func TestInbound_ImageSourceAndImageURL_Both(t *testing.T) {
	tr := NewResponsesTranslator()
	raw := []byte(`{"model":"gpt-4o","input":[{"type":"message","role":"user","content":[` +
		`{"type":"input_image","source":{"type":"base64","data":"AAA","media_type":"image/jpeg"},` +
		`"image_url":"data:image/png;base64,` + imgData + `"}]}]}`)

	req, err := tr.TranslateRequest(context.Background(), raw)
	if err != nil {
		t.Fatalf("TranslateRequest: %v", err)
	}
	ib := req.Messages[0].ContentBlocks[0]
	if ib.Data != imgData || ib.MediaType != "image/png" {
		t.Errorf("image_url should win over source: %+v", ib)
	}
}
