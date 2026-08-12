#!/usr/bin/env python3
"""Generate message transformation field-mapping diagrams for agent-proxy.
Left/right panels with connecting lines — geek & canvas terminal aesthetic.
"""

import os

CANVAS_W = 1140
BG = "#ffffff"
FG = "#24292f"
BORDER = "#d0d7de"
LINK = "#656d76"
GLOW = "#1f6feb22"

# Field colors by transform type
C_ALIAS = "#0969da"      # 别名替换
C_SAME = "#1a7f37"       # 直接透传
C_MERGE = "#bc4c00"      # 合并/拆分
C_INJECT = "#9a6700"     # 注入
C_REMOVE = "#cf222e"     # 移除
C_PASSTHROUGH = "#656d76" # 透传

FONT = "JetBrains Mono, Consolas, monospace"
FONT_SIZE = 11
TITLE_SIZE = 14
FIELD_SIZE = 9         # field row font size

ROW_H = 24             # row height
ROW_GAP = 10           # gap between rows
PANEL_HEADER_H = 28    # panel header height
HEADER_GAP = 8         # after header before fields
LP = 14                # left panel x
RP = 590               # right panel x
PW = 510               # panel width
LABEL_W = 160          # label column width in panel
TITLE_H = 64           # title area height
LEGEND_H = 36          # legend height


def calc_canvas_h(n_req, n_resp=0, has_func_box=False):
    """Calculate canvas height based on field counts."""
    h = TITLE_H
    # Request section
    h += PANEL_HEADER_H + HEADER_GAP
    if has_func_box:
        h += 28  # func box height
    h += n_req * (ROW_H + ROW_GAP) + 20
    # Response section
    if n_resp > 0:
        h += PANEL_HEADER_H + HEADER_GAP
        h += n_resp * (ROW_H + ROW_GAP) + 20
    h += LEGEND_H + 20
    return max(h, 400)


def svg_header(name, w, h):
    return f'''<?xml version="1.0" encoding="UTF-8"?>
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {w} {h}"
     width="{w}" height="{h}">
  <defs>
    <filter id="glow"><feGaussianBlur stdDeviation="2" result="blur"/>
      <feMerge><feMergeNode in="blur"/><feMergeNode in="SourceGraphic"/></feMerge>
    </filter>
  </defs>
  <rect width="{w}" height="{h}" fill="{BG}"/>
'''


def svg_footer():
    return '</svg>\n'


def draw_panel_header(x, y, w, text, color):
    return f'''<rect x="{x}" y="{y}" width="{w}" height="{PANEL_HEADER_H}" rx="4" fill="{color}" opacity="0.15"/>
<rect x="{x}" y="{y}" width="4" height="{PANEL_HEADER_H}" rx="2" fill="{color}"/>
<text x="{x+12}" y="{y+19}" fill="{color}" font-family="{FONT}" font-size="10" font-weight="bold">{text}</text>\n'''


def draw_field_row(x, y, w, label, value, accent, hl=False):
    """Draw a field row: label then value in one text element."""
    bg = accent if hl else "transparent"
    op = "0.08" if hl else "1"
    svg = f'<rect x="{x}" y="{y}" width="{w}" height="{ROW_H}" rx="2" fill="{bg}" opacity="{op}"/>\n'
    svg += f'<text x="{x+8}" y="{y+16}" fill="{accent}" font-family="{FONT}" font-size="{FIELD_SIZE}" font-weight="bold">{label}</text>\n'
    svg += f'<text x="{x+LABEL_W+8}" y="{y+16}" fill="{FG}" font-family="{FONT}" font-size="{FIELD_SIZE}" opacity="0.75">{value}</text>\n'
    return svg


def draw_connector(x1, y1, x2, y2, color, label=""):
    """Draw a line connecting left field to right field, with optional label."""
    svg = f'<line x1="{x1}" y1="{y1}" x2="{x2}" y2="{y2}" stroke="{color}" stroke-width="1.2" opacity="0.5"/>\n'
    if label:
        mx = (x1 + x2) / 2
        my = (y1 + y2) / 2
        svg += f'<rect x="{mx-28}" y="{my-9}" width="56" height="16" rx="3" fill="{BG}" stroke="{color}" stroke-width="0.8"/>\n'
        svg += f'<text x="{mx}" y="{my+3}" text-anchor="middle" fill="{color}" font-family="{FONT}" font-size="8">{label}</text>\n'
    return svg


def draw_func_box(x, y, w, text, color):
    """Draw a function call box."""
    h = 24
    svg = f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="4" fill="{color}" opacity="0.12" stroke="{color}" stroke-width="1"/>\n'
    svg += f'<text x="{x+w/2}" y="{y+16}" text-anchor="middle" fill="{color}" font-family="{FONT}" font-size="10">{text}</text>\n'
    return svg


def draw_label(text, x, y, color=FG, size=11, anchor="middle", bold=False):
    w = "bold" if bold else "normal"
    return f'<text x="{x}" y="{y}" text-anchor="{anchor}" fill="{color}" font-family="{FONT}" font-size="{size}" font-weight="{w}">{text}</text>\n'


# ─── FIELD MAPPING DATA (verified against translator.go code) ───
#
# stop_reason 映射表 (Anthropic ↔ OpenAI):
#   end_turn    → stop        | stop         → end_turn
#   max_tokens  → length      | length       → max_tokens
#   stop_sequence → stop      | tool_calls   → tool_use
#   tool_use    → tool_calls  | (default)    → end_turn
#
# content 结构差异:
#   Anthropic: [{type:"text", text:"..."}, {type:"tool_use", id, name, input:{...}}]
#   OpenAI:    "plain string" + tool_calls: [{id, type:"function", function:{name, arguments:"{json}"}}]
#   Responses: [{type:"output_text", text:"..."}, {type:"tool_call", id, name, input:{...}}]
#
# usage 字段差异:
#   Anthropic:  {input_tokens, output_tokens}
#   OpenAI:     {prompt_tokens, completion_tokens, total_tokens}
#   Responses:  {input_tokens, output_tokens, total_tokens}
#   Internal:   {prompt_tokens, completion_tokens, total_tokens}
#
# ═══ 规范符合性标记 (all fixed) ═══
# A1 ~FIXED~ Anthropic SSE → CC SSE: message_delta.usage 丢失
#     translator.go:769-780 — StreamEvent 新增 Usage 字段, TranslateStreamEvent 提取 usage
# A2 ~FIXED~ Responses completed: 硬编码 finish_reason="stop"
#     translator.go:768+ — EventData 新增 Status/StopReason, 按 mapResponsesStatus 映射
# A3 ~FIXED~ OpenAI SSE: stream_options.include_usage 未注入
#     gateway.go:buildCCRequest — 新增 StreamOptions{IncludeUsage: true}
# A4 ~FIXED~ Anthropic message_start: role 字段未传递
#     translator.go:794-815 — message_start 提取 Message.Role 到 InternalStreamChunk
# B1 ~FIXED~ 缺少 thinking 内容块类型支持
#     types.go:ContentBlock/Delta + internal.go:InternalContentBlock — 新增 thinking/signature 字段
# B2 ~FIXED~ 缺少 output_config 请求参数
#     types.go:MessageRequest — 新增 OutputConfig{Effort} 字段


def note_issue(issue_id):
    """Return a tag suffix for non-compliant fields."""
    return f" ⚠{issue_id}"


def anthropic_to_openai_request_fields():
    """Anthropic Messages → OpenAI ChatCompletion 请求映射 (translator.go:29-99, 432-459)."""
    return [
        ("model",       '"claude-sonnet-5"',        "model",       '"deepseek-v4-flash"',      C_ALIAS, "别名替换"),
        ("max_tokens",  "1024",                     "max_tokens",  "1024",                      C_SAME,  "透传"),
        ("stream",      "true",                     "stream",      "true",                      C_SAME,  "透传"),
        ("temperature", "0.7",                      "temperature", "0.7",                       C_SAME,  "透传"),
        ("top_p",       "0.9",                      "top_p",       "0.9",                       C_SAME,  "透传"),
        ("stop_sequences", '["\\n\\nHuman:"]',      "stop",        '["\\n\\nHuman:"]',          C_SAME,  "字段名变化"),
        ("system",      '"You are helpful"',        "messages[0]", '{role:"system",...}',       C_MERGE, "顶层→消息数组"),
        ("messages[0].role", '"user"',              "messages[1].role", '"user"',               C_SAME,  "透传"),
        ("messages[0].content", '[{type:"text",...}]', "messages[1].content", '"hello"',        C_MERGE, "blocks→字符串"),
        ("tools[0].name", '"get_weather"',          "tools[0].function.name", '"get_weather"',  C_SAME,  "路径变化"),
        ("tools[0].input_schema", '{type:"object",...}', "tools[0].function.parameters", '{...}', C_SAME, "路径变化"),
        ("metadata.user_id", '"u-123"',             "user",       '"u-123"',                    C_SAME,  "字段名变化"),
    ]


def openai_to_anthropic_response_fields():
    """OpenAI ChatCompletion → Anthropic Messages 响应映射 (translator.go:239-314, 694-706)."""
    return [
        ("id",         '"chatcmpl-xxx"',            "id",          '"msg_xxx"',                 C_SAME,  "透传"),
        ("object",     '"chat.completion"',         "type",        '"message"',                 C_MERGE, "固定值"),
        ("choices[0].message.role", '"assistant"',  "role",        '"assistant"',               C_SAME,  "透传"),
        ("choices[0].message.content", '"Hello!"',  "content[0].type", '"text"',               C_MERGE, "字符串→blocks"),
        ("choices[0].message.content", '"Hello!"',  "content[0].text", '"Hello!"',             C_MERGE, "字符串→blocks"),
        ("choices[0].finish_reason", '"stop"',      "stop_reason", '"end_turn"',               C_MERGE, "stop→end_turn"),
        ("choices[0].finish_reason", '"length"',    "stop_reason", '"max_tokens"',             C_MERGE, "length→max_tokens"),
        ("choices[0].finish_reason", '"tool_calls"',"stop_reason", '"tool_use"',               C_MERGE, "tool_calls→tool_use"),
        ("usage.prompt_tokens", "128",              "usage.input_tokens", "128",               C_MERGE, "prompt→input"),
        ("usage.completion_tokens", "512",          "usage.output_tokens", "512",              C_MERGE, "completion→output"),
        ("usage.total_tokens", "640",               "usage.total_tokens", "640",               C_SAME,  "一致"),
    ]


def anthropic_sse_to_openai_sse_fields():
    """Anthropic SSE 事件 → OpenAI SSE chunk 映射 (translator.go:736-827)."""
    return [
        ("message_start", '{type:"message_start",message:{id,model,role}}',
         "data:{id,object,choices:[{delta:", '{role:"assistant"}}]}',    C_MERGE, "start→首个delta"),
        ("content_block_start", '{type:"content_block_start",index,...}',
         "—",            "—",                                            C_PASSTHROUGH, "无对应"),
        ("content_block_delta", '{type:"content_block_delta",delta:{type:"text_delta",text:"He"}}',
         "data:{choices:[{delta:", '{content:"He"}}]}',                   C_MERGE, "delta.text→delta.content"),
        ("content_block_delta", '{delta:{type:"input_json_delta",partial_json:"{...}"}}',
         "data:{choices:[{delta:", '{tool_calls:[{function:{arguments:"{...}"}}]}}]}', C_MERGE, "json→tool_calls"),
        ("content_block_stop", '{type:"content_block_stop",index}',
         "—",            "—",                                            C_PASSTHROUGH, "无对应"),
        ("message_delta", '{type:"message_delta",delta:{stop_reason:"end_turn"},usage:{output_tokens:512}}',
         "data:{choices:[{", 'finish_reason:"stop"}]}',                  C_MERGE, "stop_reason→finish_reason"),
        ("message_stop", '{type:"message_stop"}',
         "data: [DONE]",  "",                                            C_SAME,  "流结束"),
        ("—",            "—",
         "data:{...}",     'usage:{prompt_tokens,completion_tokens}',     C_MERGE, "usage 字段映射"),
    ]


def passthrough_anthropic_fields():
    """Anthropic → Anthropic 透传字段 (仅 model 别名替换)."""
    return [
        ("model",       '"claude-sonnet-5"',   "model",       '"deepseek-v4-flash"', C_ALIAS, "别名替换"),
        ("max_tokens",  "1024",                "max_tokens",  "1024",                C_SAME,  "透传"),
        ("stream",      "N/A (无此字段)",        "stream",      "true (注入)",          C_INJECT, "注入"),
        ("messages",    '[{"role":"user","content":"hello"}]',
                         "messages",    '[{"role":"user","content":"hello"}]', C_SAME, "透传"),
        ("system",      '"You are helpful"',   "system",      '"You are helpful"',  C_SAME,  "透传"),
        ("temperature", "0.7",                 "temperature", "0.7",                C_SAME,  "透传"),
        ("tools",       '[{"name":"get_weather","input_schema":{...}}]',
                         "tools",       '[{"name":"get_weather","input_schema":{...}}]', C_SAME, "透传"),
    ]


def passthrough_openai_fields():
    """OpenAI → OpenAI 透传字段 (仅 model 别名替换)."""
    return [
        ("model",       '"gpt-5"',             "model",       '"deepseek-v4-flash"', C_ALIAS, "别名替换"),
        ("messages",    '[{"role":"user","content":"hello"}]',
                         "messages",    '[{"role":"user","content":"hello"}]', C_SAME, "透传"),
        ("stream",      "true (入站)",          "stream",      "false (移除)",        C_REMOVE, "移除"),
        ("max_tokens",  "1024",                "max_tokens",  "1024",                C_SAME,  "透传"),
        ("temperature", "0.7",                 "temperature", "0.7",                C_SAME,  "透传"),
    ]


def responses_to_openai_request_fields():
    """Responses → OpenAI ChatCompletion 请求翻译 (translator.go:72-121)."""
    return [
        ("model",           '"claude-sonnet-5"',    "model",       '"deepseek-v4-flash"', C_ALIAS, "别名替换"),
        ("instructions",    '"You are helpful"',    "messages[0]", '{role:"system",...}',  C_MERGE, "instructions→system"),
        ("input[0].type",   '"message"',            "messages[1].role", '"user"',         C_MERGE, "input→messages"),
        ("input[0].role",   '"user"',               "messages[1].role", '"user"',         C_SAME,  "透传"),
        ("input[0].content", '[{type:"input_text",text:"hello"}]',
                             "messages[1].content", '"hello"',                            C_MERGE, "blocks→字符串"),
        ("max_output_tokens", "1024",               "max_tokens",  "1024",                C_SAME,  "字段名变化"),
        ("stream",          "true",                 "stream",      "true",                C_SAME,  "透传"),
        ("tools[0].name",   '"get_weather"',        "tools[0].function.name", '"get_weather"', C_SAME, "路径变化"),
        ("tools[0].parameters", '{type:"object",...}', "tools[0].function.parameters", '{...}', C_SAME, "路径变化"),
    ]


def openai_to_responses_response_fields():
    """OpenAI → Responses 响应翻译 (translator.go:216-297)."""
    return [
        ("id",              '"chatcmpl-xxx"',       "id",          '"resp_xxx"',          C_SAME,  "透传"),
        ("object",          '"chat.completion"',    "object",      '"response"',           C_MERGE, "固定值"),
        ("choices[0].message.content", '"Hello!"',  "output[0].content[0].type", '"output_text"', C_MERGE, "choices→output"),
        ("choices[0].message.content", '"Hello!"',  "output[0].content[0].text", '"Hello!"', C_MERGE, "choices→output"),
        ("choices[0].finish_reason", '"stop"',      "stop_reason", '"stop"',              C_SAME,  "一致"),
        ("choices[0].finish_reason", '"length"',    "stop_reason", '"max_output_tokens"', C_MERGE, "length→max_output_tokens"),
        ("usage.prompt_tokens", "128",              "usage.input_tokens", "128",          C_MERGE, "prompt→input"),
        ("usage.completion_tokens", "512",          "usage.output_tokens", "512",         C_MERGE, "completion→output"),
    ]


def responses_sse_fields():
    """Responses SSE 事件 → Internal 映射 (translator.go:704-773)."""
    return [
        ("response.created", 'event:response.created\ndata:{type:"response.created",...}',
         "start",           '{ID, Model}',                                     C_SAME,  "透传"),
        ("response.output_delta", 'event:response.output_delta\ndata:{output_delta:{content:[{type:"delta",text:"He"}]}}',
         "delta",           '{content:"He"}',                                  C_MERGE, "output_delta→delta"),
        ("response.content_block_delta", 'event:response.content_block_delta\ndata:{delta:{text:"He"}}',
         "delta",           '{content:"He"}',                                  C_MERGE, "冗余路径"),
        ("response.completed", 'event:response.completed\ndata:{status:"completed",usage:{input_tokens,output_tokens}}',
         "done",            '{finish_reason:mapResponsesStatus(status)}',      C_MERGE, "status→finish_reason"),
        ("response.failed", 'event:response.failed\ndata:{error:{...}}',
         "error",           '{message,code}',                                  C_SAME,  "透传"),
    ]


def anthropic_sse_nonstream_wrap_fields():
    """NonStream 响应 → Anthropic SSE 拆解 (handlePassthroughNonStreamAsSSE → writeNonStreamAsSSE)."""
    return [
        ('respMap["id"]',       '"msg_xxx"',    "message_start.message.id",     '"msg_xxx"',           C_SAME,  "透传"),
        ('respMap["model"]',    '"deepseek-v4"',"message_start.message.model",  '"deepseek-v4"',       C_SAME,  "回显"),
        ('respMap["content"]',  '[{type:"text",text:"Hello"}]',
                               "content_block_start", '{type:"content_block_start",index:0}', C_MERGE, "拆解"),
        ('content[0].text',     '"Hello"',      "content_block_delta.delta.text", '"Hello"',          C_SAME,  "透传"),
        ('content_block_stop',  '—',            "content_block_stop",           '{type:"content_block_stop",index:0}', C_SAME, "生成"),
        ('respMap["stop_reason"]', '"end_turn"', "message_delta.delta.stop_reason", '"end_turn"',     C_SAME,  "透传"),
        ('respMap["usage"]',    '{input_tokens:128,output_tokens:512}',
                               "message_stop.usage", '{input_tokens:128,output_tokens:512}',          C_SAME,  "透传"),
    ]


def openai_sse_nonstream_wrap_fields():
    """NonStream 响应 → OpenAI SSE 拆解 (handlePassthroughNonStreamAsSSE → writeNonStreamAsSSE)."""
    return [
        ('respMap["id"]',       '"chatcmpl-xxx"', "data.id",          '"chatcmpl-xxx"',      C_SAME,  "透传"),
        ('respMap["object"]',   '"chat.completion"', "data.object",   '"chat.completion.chunk"', C_MERGE, "固定值"),
        ('respMap["choices"][0].message.content', '"Hello!"',
                               "data.choices[0].delta.content",       '"Hello!"',             C_MERGE, "拆解"),
        ('respMap["choices"][0].finish_reason', '"stop"',
                               "data.choices[0].finish_reason",       '"stop"',               C_SAME,  "透传"),
        ('respMap["usage"]',    '{prompt_tokens:128,...}',
                               "data.usage",      '{prompt_tokens:128,...}',                  C_SAME,  "透传"),
        ("—",                   "—",              "data: [DONE]",     "",                     C_SAME,  "流结束"),
    ]


# ─── SCENARIO BUILDERS ───

def draw_issue_notes(x, y, issues):
    """Draw compliance issue annotations at the bottom of a diagram."""
    svg = ""
    if not issues:
        return svg
    for i, (id, desc) in enumerate(issues):
        yi = y + i * 18
        svg += f'<text x="{x}" y="{yi}" fill="#cf222e" font-family="{FONT}" font-size="9" font-weight="bold">⚠{id}</text>\n'
        svg += f'<text x="{x+24}" y="{yi}" fill="#656d76" font-family="{FONT}" font-size="9">{desc}</text>\n'
    return svg


def build_translation_scenario(num, title, subtitle, req_fields, resp_fields, func_name, stream_mode="", issues=None):
    """Build a translation-path scenario diagram (left/right field mapping)."""
    n_resp = len(resp_fields) if resp_fields else 0
    has_fb = bool(stream_mode)
    ch = calc_canvas_h(len(req_fields), n_resp, has_fb)
    if issues:
        ch += len(issues) * 18 + 16
    svg = svg_header(f"{title}", CANVAS_W, ch)
    svg += draw_label(f"Scenario {num}: {title}", CANVAS_W/2, 28, C_ALIAS, TITLE_SIZE, "middle", True)
    svg += draw_label(subtitle, CANVAS_W/2, 48, LINK, 10)

    # ─── Request section ───
    ry = TITLE_H
    svg += draw_panel_header(LP, ry, PW, "REQUEST: 入站协议 → 上游协议", C_ALIAS)
    ry += PANEL_HEADER_H + HEADER_GAP

    if has_fb:
        svg += draw_func_box(CANVAS_W/2 - 90, ry, 180, func_name, C_ALIAS)
        ry += 34

    for i, (ll, lv, rl, rv, color, tag) in enumerate(req_fields):
        y = ry + i * (ROW_H + ROW_GAP)
        svg += draw_field_row(LP, y, PW-10, ll, lv, color)
        svg += draw_field_row(RP, y, PW-10, rl, rv, color)
        x1 = LP + PW - 10
        x2 = RP
        yc = y + ROW_H/2
        svg += draw_connector(x1, yc, x2, yc, color, tag)

    ry += len(req_fields) * (ROW_H + ROW_GAP) + 20

    # ─── Response section ───
    if resp_fields:
        svg += draw_panel_header(LP, ry, PW, "RESPONSE: 上游协议 → 入站协议", C_SAME)
        ry += PANEL_HEADER_H + HEADER_GAP

        for i, (ll, lv, rl, rv, color, tag) in enumerate(resp_fields):
            y = ry + i * (ROW_H + ROW_GAP)
            svg += draw_field_row(LP, y, PW-10, ll, lv, color)
            svg += draw_field_row(RP, y, PW-10, rl, rv, color)
            x1 = LP + PW - 10
            x2 = RP
            yc = y + ROW_H/2
            svg += draw_connector(x1, yc, x2, yc, color, tag)

    # Issue notes before legend
    if issues:
        iy = ch - LEGEND_H - len(issues) * 18 - 8
        svg += draw_issue_notes(30, iy, issues)

    # Legend at bottom
    ly = ch - LEGEND_H
    svg += draw_label("别名替换", 80,  ly, C_ALIAS, 9, "start")
    svg += draw_label("直接透传", 185, ly, C_SAME, 9, "start")
    svg += draw_label("合并/拆分", 290, ly, C_MERGE, 9, "start")
    svg += draw_label("注入",    395, ly, C_INJECT, 9, "start")
    svg += draw_label("移除",    470, ly, C_REMOVE, 9, "start")
    svg += draw_label("无对应",  545, ly, C_PASSTHROUGH, 9, "start")

    svg += svg_footer()
    return svg


def build_passthrough_scenario(num, title, subtitle, req_fields, func_name, stream_mode, response_note, issues=None):
    """Build a passthrough-path scenario diagram."""
    has_fb = bool(stream_mode)
    ch = calc_canvas_h(len(req_fields), 0, has_fb)
    if response_note:
        ch += 60  # extra for response note
    if issues:
        ch += len(issues) * 18 + 16
    svg = svg_header(f"{title}", CANVAS_W, ch)
    svg += draw_label(f"Scenario {num}: {title}", CANVAS_W/2, 28, C_SAME, TITLE_SIZE, "middle", True)
    svg += draw_label(subtitle, CANVAS_W/2, 48, LINK, 10)

    ry = TITLE_H
    svg += draw_panel_header(LP, ry, PW, "REQUEST: 入站 → 上游 (透传)", C_SAME)
    ry += PANEL_HEADER_H + HEADER_GAP

    if has_fb:
        svg += draw_func_box(CANVAS_W/2 - 100, ry, 200, func_name, C_SAME)
        ry += 34

    for i, (ll, lv, rl, rv, color, tag) in enumerate(req_fields):
        y = ry + i * (ROW_H + ROW_GAP)
        svg += draw_field_row(LP, y, PW-10, ll, lv, color)
        svg += draw_field_row(RP, y, PW-10, rl, rv, color)
        x1 = LP + PW - 10
        x2 = RP
        yc = y + ROW_H/2
        svg += draw_connector(x1, yc, x2, yc, color, tag)

    ry += len(req_fields) * (ROW_H + ROW_GAP) + 20

    # Response section
    if response_note:
        svg += draw_panel_header(LP, ry, PW, "RESPONSE: 上游 SSE → 客户端 SSE", C_SAME)
        ry += PANEL_HEADER_H + HEADER_GAP
        svg += draw_label(response_note, CANVAS_W/2, ry + 12, LINK, 10)

    # Issue notes before legend
    if issues:
        iy = ch - LEGEND_H - len(issues) * 18 - 8
        svg += draw_issue_notes(30, iy, issues)

    # Legend
    ly = ch - LEGEND_H
    svg += draw_label("别名替换", 80,  ly, C_ALIAS, 9, "start")
    svg += draw_label("直接透传", 185, ly, C_SAME, 9, "start")
    svg += draw_label("注入",    290, ly, C_INJECT, 9, "start")
    svg += draw_label("移除",    395, ly, C_REMOVE, 9, "start")

    svg += svg_footer()
    return svg


def build_nonstream_as_sse_scenario(num, title, subtitle, req_fields, func_name, response_note):
    """Build a scenario where upstream non-stream response is wrapped as SSE."""
    if "Anthropic" in title:
        sse_fields = anthropic_sse_nonstream_wrap_fields()
    else:
        sse_fields = openai_sse_nonstream_wrap_fields()

    has_fb = bool(func_name)
    ch = calc_canvas_h(len(req_fields), len(sse_fields), has_fb)
    svg = svg_header(f"{title}", CANVAS_W, ch)
    svg += draw_label(f"Scenario {num}: {title}", CANVAS_W/2, 28, C_SAME, TITLE_SIZE, "middle", True)
    svg += draw_label(subtitle, CANVAS_W/2, 48, LINK, 10)

    ry = TITLE_H
    svg += draw_panel_header(LP, ry, PW, "REQUEST: 入站 → 上游 (透传)", C_SAME)
    ry += PANEL_HEADER_H + HEADER_GAP

    if has_fb:
        svg += draw_func_box(CANVAS_W/2 - 110, ry, 220, func_name, C_SAME)
        ry += 34

    for i, (ll, lv, rl, rv, color, tag) in enumerate(req_fields):
        y = ry + i * (ROW_H + ROW_GAP)
        svg += draw_field_row(LP, y, PW-10, ll, lv, color)
        svg += draw_field_row(RP, y, PW-10, rl, rv, color)
        x1 = LP + PW - 10
        x2 = RP
        yc = y + ROW_H/2
        svg += draw_connector(x1, yc, x2, yc, color, tag)

    ry += len(req_fields) * (ROW_H + ROW_GAP) + 20

    # Response section: NonStream → SSE wrapping
    svg += draw_panel_header(LP, ry, PW, "RESPONSE: 上游非流式 JSON → SSE 拆解 → 客户端", C_SAME)
    ry += PANEL_HEADER_H + HEADER_GAP

    for i, (ll, lv, rl, rv, color, tag) in enumerate(sse_fields):
        y = ry + i * (ROW_H + ROW_GAP)
        svg += draw_field_row(LP, y, PW-10, ll, lv, color)
        svg += draw_field_row(RP, y, PW-10, rl, rv, color)
        x1 = LP + PW - 10
        x2 = RP
        yc = y + ROW_H/2
        svg += draw_connector(x1, yc, x2, yc, color, tag)

    # Legend
    ly = ch - LEGEND_H
    svg += draw_label("别名替换", 80,  ly, C_ALIAS, 9, "start")
    svg += draw_label("直接透传", 185, ly, C_SAME, 9, "start")
    svg += draw_label("合并/拆分", 290, ly, C_MERGE, 9, "start")
    svg += draw_label("注入",    395, ly, C_INJECT, 9, "start")
    svg += draw_label("移除",    470, ly, C_REMOVE, 9, "start")

    svg += svg_footer()
    return svg


def build_overview():
    """Overview: 9-scenario routing matrix with left/right panels."""
    w, h = 1200, 800
    svg = svg_header("Overview", w, h)
    svg += draw_label("Agent-Proxy 消息转换全览", w/2, 32, C_ALIAS, 18, "middle", True)
    svg += draw_label("9 种入站/上游协议 × 流式组合 → 透传/翻译路径 → 函数路由", w/2, 54, LINK, 10)

    # ── Left: 透传路径 ──
    svg += draw_panel_header(30, 80, 560, "透传路径 (协议相同, --stream-mode 控制)", C_SAME)

    scenarios = [
        ("#5", "Anthropic非流 → Anthropic流", "stream", "handlePassthroughStreamWithBody", C_INJECT, "注入 stream:true"),
        ("#6", "Anthropic流 → Anthropic非流", "non-stream", "handlePassthroughNonStreamAsSSE", C_REMOVE, "写 NonStream→SSE"),
        ("#7", "OpenAI流 → OpenAI非流", "non-stream", "handlePassthroughNonStreamAsSSE", C_REMOVE, "写 NonStream→SSE"),
        ("auto", "auto 默认: 无 stream → handlePassthroughNonStream(chunked JSON)", "auto", "handlePassthroughNonStream", FG, "先写头+Flush 防超时"),
    ]

    y = 118
    for num, desc, mode, func, color, note in scenarios:
        svg += draw_field_row(30, y, 560, f"{num}  {desc}", f"--stream-mode {mode}  →  {func}  ({note})", color, True)
        y += ROW_H + 2

    # ── Right: 翻译路径 ──
    svg += draw_panel_header(620, 80, 550, "翻译路径 (协议不同, --stream-mode 新增支持)", C_ALIAS)

    trans = [
        ("#1", "Anthropic流 → OpenAI流", "auto", "handleStreamRequest", C_SAME, "stream 保留"),
        ("#2", "Anthropic非流 → OpenAI非流", "auto", "handleNonStreamResponse", C_SAME, "stream 保留"),
        ("#3", "Anthropic流 → OpenAI非流", "non-stream (NEW)", "handleNonStreamResponseAsSSE", C_REMOVE, "翻译 → SSE包装"),
        ("#4", "Anthropic非流 → OpenAI流", "stream (NEW)", "handleStreamRequestAsNonStream", C_INJECT, "SSE收集 → JSON"),
        ("#8", "Responses非流 → OpenAI流", "stream (NEW)", "handleStreamRequestAsNonStream", C_INJECT, "SSE收集 → JSON"),
        ("#9", "Responses流 → OpenAI非流", "non-stream (NEW)", "handleNonStreamResponseAsSSE", C_REMOVE, "翻译 → SSE包装"),
    ]

    y = 118
    for num, desc, mode, func, color, note in trans:
        svg += draw_field_row(620, y, 550, f"{num}  {desc}", f"--stream-mode {mode}  →  {func}  ({note})", color, True)
        y += ROW_H + 2

    # ── Architecture ──
    svg += draw_panel_header(30, 290, 1140, "架构管道", C_MERGE)
    arch_lines = [
        "翻译管道:  IngressRequest → TranslateRequest → InternalRequest → TranslateToProvider → ProviderRequest → Provider.Call",
        "          ProviderResponse → TranslateFromProvider → InternalResponse → TranslateResponse → IngressResponse",
        "透传管道:  IngressRequest → (model alias replace) → Provider.Call → (model echo) → IngressResponse",
        "--stream-mode:  auto(自适应) | stream(强制流式) | non-stream(强制非流式) | passthrough(直连)",
    ]
    y = 328
    for line in arch_lines:
        svg += draw_label(line, 40, y, LINK, 10, "start")
        y += 20

    svg += svg_footer()
    return svg


def main():
    os.makedirs("docs", exist_ok=True)

    # ── Scenario 1: Anthropic 流 → OpenAI 流 ──
    svg = build_translation_scenario(
        1, "Anthropic 流式 → OpenAI 流式",
        "翻译路径 · stream 保留 · handleStreamRequest",
        anthropic_to_openai_request_fields(),
        anthropic_sse_to_openai_sse_fields(),
        "handleStreamRequest()",
    )
    with open("docs/scenario_01_anthropic_stream_openai_stream.svg", "w", encoding="utf-8") as f:
        f.write(svg)

    # ── Scenario 2: Anthropic 非流 → OpenAI 非流 ──
    svg = build_translation_scenario(
        2, "Anthropic 非流式 → OpenAI 非流式",
        "翻译路径 · stream 保留 · handleNonStreamResponse",
        anthropic_to_openai_request_fields(),
        openai_to_anthropic_response_fields(),
        "handleNonStreamResponse()",
    )
    with open("docs/scenario_02_anthropic_nonstream_openai_nonstream.svg", "w", encoding="utf-8") as f:
        f.write(svg)

    # ── Scenario 3: Anthropic 流 → OpenAI 非流 ──
    nonstream_req = [
        ("model", '"claude-sonnet-5"', "model", '"deepseek-v4-flash"', C_ALIAS, "别名替换"),
        ("stream", "true (入站)", "stream", "false (覆写)", C_REMOVE, "覆写"),
        ("messages", "[{role,content}]", "messages", "[{role,content}]", C_SAME, "透传"),
        ("system", '"You are helpful"', "messages[0].role", '"system"', C_MERGE, "合并"),
        ("max_tokens", "1024", "max_tokens", "1024", C_SAME, "透传"),
    ]
    svg = build_translation_scenario(
        3, "Anthropic 流式 → OpenAI 非流式",
        "翻译 + --stream-mode non-stream · handleNonStreamResponseAsSSE",
        nonstream_req,
        None,  # response handled by writeNonStreamAsSSE
        "handleNonStreamResponseAsSSE()",
        "non-stream",
    )
    with open("docs/scenario_03_anthropic_stream_openai_nonstream.svg", "w", encoding="utf-8") as f:
        f.write(svg)

    # ── Scenario 4: Anthropic 非流 → OpenAI 流 ──
    stream_req = [
        ("model", '"claude-sonnet-5"', "model", '"deepseek-v4-flash"', C_ALIAS, "别名替换"),
        ("stream", "false (入站)", "stream", "true (覆写)", C_INJECT, "覆写"),
        ("messages", "[{role,content}]", "messages", "[{role,content}]", C_SAME, "透传"),
        ("system", '"You are helpful"', "messages[0].role", '"system"', C_MERGE, "合并"),
        ("max_tokens", "1024", "max_tokens", "1024", C_SAME, "透传"),
    ]
    stream_resp = [
        ("SSE delta × N", '{delta:{content:"He"}}...', "累积 content", '"Hello!"', C_MERGE, "收集"),
        ("SSE delta × N", '{delta:{content:"llo"}}', "累积 content", '"!"', C_MERGE, "收集"),
        ("SSE usage", '{prompt_tokens:128,...}', "InternalResponse.usage", "{input_tokens:128,...}", C_MERGE, "组装"),
        ("InternalResponse", "{model,choices,usage}", "TranslateResponse", "→ Anthropic JSON", C_MERGE, "翻译"),
    ]
    svg = build_translation_scenario(
        4, "Anthropic 非流式 → OpenAI 流式",
        "翻译 + --stream-mode stream · handleStreamRequestAsNonStream",
        stream_req,
        stream_resp,
        "handleStreamRequestAsNonStream()",
        "stream",
    )
    with open("docs/scenario_04_anthropic_nonstream_openai_stream.svg", "w", encoding="utf-8") as f:
        f.write(svg)

    # ── Scenario 5: Anthropic非流 → Anthropic流 ──
    svg = build_passthrough_scenario(
        5, "Anthropic 非流式 → Anthropic 流式",
        "透传 + --stream-mode stream · handlePassthroughStreamWithBody",
        passthrough_anthropic_fields(),
        "handlePassthroughStreamWithBody()",
        "stream",
        "SSE 逐行透传 + 500ms 心跳 + 别名回显 → event: done",
    )
    with open("docs/scenario_05_anthropic_nonstream_anthropic_stream.svg", "w", encoding="utf-8") as f:
        f.write(svg)

    # ── Scenario 6: Anthropic流 → Anthropic非流 ──
    svg = build_nonstream_as_sse_scenario(
        6, "Anthropic 流式 → Anthropic 非流式",
        "透传 + --stream-mode non-stream · handlePassthroughNonStreamAsSSE",
        passthrough_anthropic_fields(),
        "handlePassthroughNonStreamAsSSE()",
        "p.Call 非流式 → writeNonStreamAsSSE 拆解为 Anthropic SSE 事件",
    )
    with open("docs/scenario_06_anthropic_stream_anthropic_nonstream.svg", "w", encoding="utf-8") as f:
        f.write(svg)

    # ── Scenario 7: OpenAI流 → OpenAI非流 ──
    svg = build_nonstream_as_sse_scenario(
        7, "OpenAI 流式 → OpenAI 非流式",
        "透传 + --stream-mode non-stream · handlePassthroughNonStreamAsSSE",
        passthrough_openai_fields(),
        "handlePassthroughNonStreamAsSSE()",
        "p.Call 非流式 → writeNonStreamAsSSE 拆解为 OpenAI SSE chunk → [DONE]",
    )
    with open("docs/scenario_07_openai_stream_openai_nonstream.svg", "w", encoding="utf-8") as f:
        f.write(svg)

    # ── Scenario 8: Responses非流 → OpenAI流 ──
    responses_req = [
        ("model", '"claude-sonnet-5"', "model", '"deepseek-v4-flash"', C_ALIAS, "别名替换"),
        ("stream", "false (入站)", "stream", "true (覆写)", C_INJECT, "覆写"),
        ("input", '[{type:message,...}]', "messages", "[{role,content}]", C_MERGE, "翻译"),
        ("instructions", '"You are..."', "messages[0].role", '"system"', C_MERGE, "合并"),
    ]
    svg = build_translation_scenario(
        8, "Responses 非流式 → OpenAI 流式",
        "翻译 + --stream-mode stream · handleStreamRequestAsNonStream",
        responses_req,
        stream_resp,
        "handleStreamRequestAsNonStream()",
        "stream",
    )
    with open("docs/scenario_08_responses_nonstream_openai_stream.svg", "w", encoding="utf-8") as f:
        f.write(svg)

    # ── Scenario 9: Responses流 → OpenAI非流 ──
    responses_req2 = [
        ("model", '"claude-sonnet-5"', "model", '"deepseek-v4-flash"', C_ALIAS, "别名替换"),
        ("stream", "true (入站)", "stream", "false (覆写)", C_REMOVE, "覆写"),
        ("input", '[{type:message,...}]', "messages", "[{role,content}]", C_MERGE, "翻译"),
        ("instructions", '"You are..."', "messages[0].role", '"system"', C_MERGE, "合并"),
    ]
    svg = build_translation_scenario(
        9, "Responses 流式 → OpenAI 非流式",
        "翻译 + --stream-mode non-stream · handleNonStreamResponseAsSSE",
        responses_req2,
        None,
        "handleNonStreamResponseAsSSE()",
        "non-stream",
    )
    with open("docs/scenario_09_responses_stream_openai_nonstream.svg", "w", encoding="utf-8") as f:
        f.write(svg)

    # ── Overview ──
    with open("docs/overview_all_scenarios.svg", "w", encoding="utf-8") as f:
        f.write(build_overview())

    print("Done. 10 SVG files regenerated in docs/")


if __name__ == "__main__":
    main()