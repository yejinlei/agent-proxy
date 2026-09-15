package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// @AI_GUARD: NONSTREAM_A2S_RESPONSES_EVENTS - writeNonStreamAsSSE 的 OpenAI Responses 分支
// 必须发带 type 字段的完整事件序列，禁止发裸 chunk。
//
// 回归历史（v0.2.146/147）：本分支原先发 {"object":"response.text_delta","response":{...},
// "output":[...]} —— 无 type 字段、无 created_at、无响应级 object。Codex 的 WS 路径对整帧做
// serde_json::from_str::<ResponsesStreamEvent> 且靠 type 分发事件，全部解析失败被静默丢弃，
// 客户端报 stream closed before response.completed / os error 10054。
// 本测试锁住「每条帧都有 type、type 与 event 名一致、终态是 response.completed」。
func TestWriteNonStreamAsSSE_ResponsesEvents(t *testing.T) {
	respBody := `{
		"id":"resp_upstream_1","model":"m","output":[
			{"id":"msg_1","type":"message","role":"assistant","status":"completed",
			 "content":[{"type":"output_text","text":"hi"}]},
			{"id":"call_1","type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"ls\"}","call_id":"call_1"}
		],
		"usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120}
	}`
	rec := httptest.NewRecorder()
	writeNonStreamAsSSE(rec, rec, []byte(respBody), "glm-5.2")

	out := rec.Body.String()
	if out == "" {
		t.Fatal("writeNonStreamAsSSE 没有写出任何字节")
	}

	// 拆帧
	var events []struct {
		event string
		data  string
	}
	for _, block := range strings.Split(out, "\n\n") {
		var ev, data string
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "event: ") {
				ev = strings.TrimSpace(strings.TrimPrefix(line, "event: "))
			} else if strings.HasPrefix(line, "data: ") {
				data += strings.TrimPrefix(line, "data: ")
			}
		}
		if data != "" {
			events = append(events, struct {
				event string
				data  string
			}{ev, data})
		}
	}

	// 1. 每一帧都必须有 event 前缀 + data 里的 type 字段，且两者一致。
	//    缺 type 是本次事故的直接根因。[DONE] 是唯一例外：它不是事件，是流终止标记。
	for i, e := range events {
		if e.data == "[DONE]" {
			continue
		}
		if e.event == "" {
			t.Errorf("第 %d 帧缺 event: 前缀（data=%.80q）", i+1, e.data)
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(e.data), &payload); err != nil {
			t.Errorf("第 %d 帧不是合法 JSON: %v", i+1, err)
			continue
		}
		if typ, _ := payload["type"].(string); typ != e.event {
			t.Errorf("第 %d 帧 event=%q 但 data.type=%q（必须一致）", i+1, e.event, typ)
		}
	}

	// 2. 整条流里禁止出现裸 chunk 形态（v0.2.146/147 的实际字节）
	if strings.Contains(out, `"object":"response.text_delta"`) {
		t.Error("仍发出 object=response.text_delta 的裸 chunk（无 type 字段），Codex 会解析失败")
	}
	if strings.Contains(out, "created_at") {
		t.Error("响应里出现了 created_at（非流式响应体没有该字段，可能是拼错了）")
	}
	// 响应级 object 字段也不发 —— responses/translator.go 的 sendCompleted/sendCreated
	// 都不带它。载荷里出现 object 说明形状偏离了参考实现，值得单独报出来。
	if strings.Contains(out, `"object":"response"`) {
		t.Error("response 载荷里带了 object 字段 —— 与 responses/translator.go 的形状不一致")
	}

	// 3. 禁止出现会让 Codex 直接 Err 中止的事件 kind
	for _, bad := range []string{"response.failed", "response.incomplete"} {
		if strings.Contains(out, `"type":"`+bad+`"`) {
			t.Errorf("发出了 %s —— 该 kind 在 Codex 里直接 return Err 中止事件循环", bad)
		}
	}

	// 4. 事件序列必须完整收尾
	got := []string{}
	for _, e := range events {
		got = append(got, e.event)
	}
	idxOf := func(name string) int {
		for i, s := range got {
			if s == name {
				return i
			}
		}
		return -1
	}
	if i := idxOf("response.created"); i < 0 {
		t.Error("缺 response.created（客户端靠它确认连接活着并拿到 response.id）")
	}
	if i := idxOf("response.completed"); i < 0 {
		t.Error("缺 response.completed —— 客户端报 stream closed before response.completed")
	} else {
		for _, ev := range []string{"response.created", "response.output_item.added", "response.output_item.done"} {
			if j := idxOf(ev); j > i {
				t.Errorf("%s（位置 %d）出现在 response.completed（位置 %d）之后", ev, j, i)
			}
		}
	}
	// added 必须早于 done，且两个数量相等（每个 item 一对）
	nAdded, nDone := countEvent(events, "response.output_item.added"), countEvent(events, "response.output_item.done")
	if nAdded != nDone {
		t.Errorf("output_item.added=%d 但 output_item.done=%d，数量必须相等", nAdded, nDone)
	}
	if i, j := idxOf("response.output_item.added"), idxOf("response.output_item.done"); i >= 0 && j >= 0 && i > j {
		t.Error("response.output_item.done 出现在 added 之前")
	}
	// [DONE] 必须是最后一帧
	if events[len(events)-1].data != "[DONE]" {
		t.Errorf("最后一帧 = %q，want [DONE]", events[len(events)-1].data)
	}
	// added/done 各 2 个（请求体有 2 个 output item）
	if nAdded != 2 {
		t.Errorf("output_item.added 数量 = %d，want 2（output 里有 2 个 item）", nAdded)
	}

	// 5. output_index 必须逐 item 递增（0,1,2…），禁止写常量
	//    —— v0.2.148 第一版误用 len(output)-1 硬编码，两个 item 都拿到 index=1。
	//    本路径当前无调用方（翻译路径已撤回降级），但形状必须与 responses/translator.go 一致，
	//    否则哪天又被接回去又得返工。
	var idxs []float64
	for _, e := range events {
		if e.event != "response.output_item.added" {
			continue
		}
		var p map[string]any
		json.Unmarshal([]byte(e.data), &p)
		if v, ok := p["output_index"].(float64); ok {
			idxs = append(idxs, v)
		} else {
			t.Errorf("added 帧缺 output_index（data=%.120q）", e.data)
		}
	}
	for i, v := range idxs {
		if v != float64(i) {
			t.Errorf("第 %d 个 added 的 output_index = %v，want %d", i+1, v, i)
		}
	}

	// 6. response.completed 的载荷形状：id/status/usage 三键齐全、finish_reason 动态
	var completed map[string]any
	for _, e := range events {
		if e.event == "response.completed" {
			json.Unmarshal([]byte(e.data), &completed)
			break
		}
	}
	if completed == nil {
		return // 上面已报错
	}
	r := completed["response"].(map[string]any)
	if r["id"] == nil || r["id"] == "" {
		t.Error("response.completed.response.id 缺失或为空")
	}
	if r["status"] != "completed" {
		t.Errorf("response.completed.response.status = %v，want completed", r["status"])
	}
	u, ok := r["usage"].(map[string]any)
	if !ok {
		t.Error("response.completed.response.usage 缺失（Codex 要求三键齐全且为数字）")
	} else {
		for _, k := range []string{"input_tokens", "output_tokens", "total_tokens"} {
			if v, ok := u[k].(float64); !ok || v < 0 {
				t.Errorf("usage.%s = %v（必须是数字）", k, u[k])
			}
		}
	}
	if r["finish_reason"] != "function_call" {
		t.Errorf("finish_reason = %v，want function_call（output 里有 function_call item）", r["finish_reason"])
	}
	outArr, ok := r["output"].([]any)
	if !ok || len(outArr) == 0 {
		t.Error("response.completed.response.output 为空或非数组")
	}
	// 7. 单个 output item 的必填字段
	for _, itAny := range outArr {
		it, ok := itAny.(map[string]any)
		if !ok {
			continue
		}
		switch it["type"] {
		case "message":
			if it["role"] == "" {
				t.Error("message item 缺 role（Codex 必填）")
			}
			c, ok := it["content"].([]any)
			if !ok {
				t.Error("message item 的 content 不是数组（必须 [] 而非 null/缺失）")
			} else if len(c) == 0 {
				t.Error("message item 的 content 为空数组")
			}
		case "function_call":
			for _, k := range []string{"name", "arguments", "call_id"} {
				if v := it[k]; v == nil || v == "" {
					t.Errorf("function_call item 缺 %s（Codex 必填）", k)
				}
			}
		}
	}
}

func countEvent(events []struct {
	event string
	data  string
}, name string) int {
	n := 0
	for _, e := range events {
		if e.event == name {
			n++
		}
	}
	return n
}
