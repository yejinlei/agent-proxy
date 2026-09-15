package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

// TestQuickRemoveStreamFlag_RealWireForm 锁住 NONSTREAM_AS_SSE_STREAM_FLAG 的关键前提：
// buildCCRequest 实际产出的下游体里 stream 标记的确切字节形态，以及 quickRemoveStreamFlag
// 确实能把它改写成 false。
//
// 为什么单独测这条：handleNonStreamResponseAsSSE 靠这个字符串替换把「客户端想要流式」
// 改成「上游按非流式返回」。替换字符串与上游实际 wire form 只要有一字节不符，替换就静默
// 失效，上游收到 stream:true 却走 p.Call 的非流式路径 → 响应体是 SSE 事件流、不是 JSON →
// TranslateFromProvider 反序列化失败。而这条路径没有任何编译期约束（一个 string.Replace），
// 唯一能锁住它的就是拿真实产物体做断言。
//
// @AI_GUARD: NONSTREAM_AS_SSE_STREAM_FLAG - 与 quick.go handleNonStreamResponseAsSSE 同 guard
func TestQuickRemoveStreamFlag_RealWireForm(t *testing.T) {
	raw, _ := json.Marshal("hi")
	ir := &schema.InternalRequest{
		Model:  "glm-5.2",
		Stream: true,
		Messages: []schema.InternalMessage{
			{Role: schema.Role("user"), Content: raw},
		},
	}

	downstreamReq, err := json.Marshal(buildCCRequest(ir, "https://token.sensenova.cn"))
	if err != nil {
		t.Fatalf("marshal buildCCRequest: %v", err)
	}

	// ── 前提 1：buildCCRequest 确实写出 stream:true ──
	// 这条断言是整条链路的起点。stream 字段的 tag 是 `json:"stream,omitempty"`，
	// Stream 是 bool、true 不被 omitempty 省略，Go 默认无空格序列化 → 形态是 "stream":true。
	// 如果哪天改成了带空格的序列化（比如换了 encoder、或加了 SetEscapeHTML/SetIndent），
	// 下面的替换必须跟着改，这个测试会先炸。
	if !strings.Contains(string(downstreamReq), `"stream":true`) {
		t.Fatalf("buildCCRequest 未产出 \"stream\":true，实际体：%s", string(downstreamReq))
	}

	nsBody := quickRemoveStreamFlag(downstreamReq)

	// ── 前提 2：替换后 true 消失、false 出现 ──
	if strings.Contains(string(nsBody), `"stream":true`) {
		t.Errorf("替换后仍含 \"stream\":true，说明替换字符串与 wire form 不匹配：\n%s", string(nsBody))
	}
	if !strings.Contains(string(nsBody), `"stream":false`) {
		t.Errorf("替换后未出现 \"stream\":false：\n%s", string(nsBody))
	}

	// ── 前提 3：替换后仍是合法 JSON（避免替换切坏结构）──
	if err := json.Unmarshal(nsBody, &map[string]any{}); err != nil {
		t.Errorf("替换后 JSON 非法：%v\n%s", err, string(nsBody))
	}

	// ── 前提 4：语义正确 —— 解析出来 stream 确实是 false ──
	var got map[string]any
	if err := json.Unmarshal(nsBody, &got); err != nil {
		t.Fatalf("unmarshal nsBody: %v", err)
	}
	if sv, ok := got["stream"]; !ok || sv != false {
		t.Errorf("替换后 stream = %v (ok=%v)，want false", sv, ok)
	}
}

// TestQuickRemoveStreamFlag_IndentedForm 覆盖带空格的形态。
//
// buildCCRequest 走的是 Go 默认无空格序列化，所以主路径命中的是第一个 pattern。
// 但 quickRemoveStreamFlag 同时服务透传路径，那里的 body 是客户端**原始请求体**——
// 客户端可能发 indented / 手写 JSON，此时形态是 `"stream": true`（带空格）。
// 两个 pattern 缺一不可，这里把带空格的形态也锁住，防止有人精简掉第二个 Replace。
func TestQuickRemoveStreamFlag_IndentedForm(t *testing.T) {
	raw := []byte(`{"model":"glm-5.2","messages":[],"stream": true}`)

	nsBody := quickRemoveStreamFlag(raw)

	if strings.Contains(string(nsBody), `"stream": true`) {
		t.Errorf("带空格形态替换失败：\n%s", string(nsBody))
	}
	var got map[string]any
	if err := json.Unmarshal(nsBody, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if sv, _ := got["stream"]; sv != false {
		t.Errorf("带空格形态替换后 stream = %v，want false", sv)
	}
}
