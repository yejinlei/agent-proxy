#!/usr/bin/env python3
"""Generate agent-proxy architecture diagram (canvas-style PNG)."""

from PIL import Image, ImageDraw, ImageFont
import math

# ── Theme (matching agent-nexus dark style) ──
BG = (18, 22, 33)          # #121621
CARD_BG = (32, 38, 56)     # #202638
EDGE = (80, 90, 130)       # muted border
ACCENT_1 = (120, 200, 255) # blue — CC entry
ACCENT_2 = (180, 140, 255) # purple — schema
ACCENT_3 = (100, 220, 160) # green — translators
ACCENT_4 = (255, 180, 100) # orange — providers
ACCENT_5 = (255, 120, 140) # red — Web UI
TEXT = (230, 235, 245)
TEXT_DIM = (150, 160, 180)
TEXT_BRIGHT = (255, 255, 255)

W, H = 1440, 1000

def get_font(size, bold=False):
    # Try Microsoft YaHei first (supports CJK), then Consolas, then default
    for fname in (
        "C:/Windows/Fonts/msyhbd.ttc" if bold else "C:/Windows/Fonts/msyh.ttc",
        "C:/Windows/Fonts/simhei.ttf",
        "C:/Windows/Fonts/Consolas.ttf",
        "C:/Windows/Fonts/dejavusans.ttf",
    ):
        try:
            return ImageFont.truetype(fname, size)
        except Exception:
            continue
    return ImageFont.load_default()

FONT_T = get_font(18, bold=False)
FONT_B = get_font(16, bold=True)
FONT_S = get_font(14)
FONT_SM = get_font(12)
FONT_TINY = get_font(10)
FONT_TITLE = get_font(22, bold=True)
FONT_SUBTITLE = get_font(13)


def round_rect(draw, box, radius, fill, outline=None, width=1):
    draw.rounded_rectangle(box, radius=radius, fill=fill, outline=outline, width=width)


def draw_arrow(draw, x0, y0, x1, y1, color=ACCENT_1, dashed=False):
    # Simple line with arrowhead
    draw.line([(x0, y0), (x1, y1)], fill=color, width=2)
    # Arrowhead
    angle = math.atan2(y1 - y0, x1 - x0)
    size = 10
    for a in [angle - 2.6, angle + 2.6]:
        ex = int(x1 - size * math.cos(a))
        ey = int(y1 - size * math.sin(a))
        draw.line([(x1, y1), (ex, ey)], fill=color, width=2)


def draw_double_arrow(draw, x0, y0, x1, y1, color=ACCENT_2):
    draw.line([(x0, y0), (x1, y1)], fill=color, width=2)
    # Both ends
    for sx, sy in [(x0, y0), (x1, y1)]:
        if sx == x0:
            angle = math.atan2(y1 - y0, x1 - x0) + math.pi
        else:
            angle = math.atan2(y1 - y0, x1 - x0)
        size = 9
        for a in [angle - 2.6, angle + 2.6]:
            ex = int(sx - size * math.cos(a))
            ey = int(sy - size * math.sin(a))
            draw.line([(sx, sy), (ex, ey)], fill=color, width=2)


def label(draw, x, y, text, color=TEXT, font=FONT_S, anchor="mm"):
    draw.text((x, y), text, fill=color, font=font, anchor=anchor)


def measure(draw, text, font):
    return draw.textbbox((0, 0), text, font=font)[2]


def draw_box_centered(cx, cy, w, h, title, subtitle=None, fill=CARD_BG, outline=EDGE, items=None):
    """Draw a centered card with title, optional subtitle and bullet items."""
    x0, y0 = cx - w // 2, cy - h // 2
    x1, y1 = x0 + w, y0 + h
    round_rect(draw, [x0, y0, x1, y1], 8, fill, outline, 2)

    # Title
    draw.text((cx, y0 + 14), title, fill=TEXT_BRIGHT, font=FONT_B, anchor="mt")

    if subtitle:
        draw.text((cx, y0 + 36), subtitle, fill=TEXT_DIM, font=FONT_SM, anchor="mt")

    if items:
        start_y = y0 + 50
        if subtitle:
            start_y = y0 + 56
        for i, item in enumerate(items):
            bullet = "●"
            draw.text((x0 + 20, start_y + i * 22), bullet, fill=ACCENT_3, font=FONT_S)
            draw.text((x0 + 40, start_y + i * 22), item, fill=TEXT, font=FONT_S)


def draw_protocol_card(draw, cx, cy, name, color, tag=""):
    w, h = 140, 64
    round_rect(draw, [cx - w//2, cy - h//2, cx + w//2, cy + h//2], 8, CARD_BG, color, 2)
    draw.text((cx, cy - 10), name, fill=color, font=FONT_B, anchor="mm")
    if tag:
        draw.text((cx, cy + 12), tag, fill=TEXT_DIM, font=FONT_SM, anchor="mm")


img = Image.new("RGB", (W, H), BG)
draw = ImageDraw.Draw(img)

# ── Title ──
label(draw, W // 2, 30, "agent-proxy  ·  AI 消息协议网关", TEXT_BRIGHT, FONT_TITLE)
label(draw, W // 2, 58, "Central Schema 架构  ·  统一翻译 · 多协议互通", TEXT_DIM, FONT_S)
# divider line
draw.line([(160, 76), (W - 160, 76)], fill=EDGE, width=1)

# ── Layer 1: User / Client ──
y1 = 130
label(draw, W // 2 - 160, y1 - 18, "① 上游客户端（均使用标准 OpenAI Chat Completions 格式）", TEXT_DIM, FONT_S)
draw.line([(120, y1), (900, y1 + 30)], fill=EDGE, width=1)
# Three sample clients (left side only — UI panel on right)
clients = [("Claude Code", ACCENT_5), ("Cursor", ACCENT_1), ("Kimi", ACCENT_3)]
cw = 180
gap = 50
start_x = 260
for i, (name, color) in enumerate(clients):
    cx = start_x + i * (cw + gap)
    round_rect(draw, [cx - cw//2, y1 + 8, cx + cw//2, y1 + 30], 6, (24, 28, 42), color, 1)
    draw.text((cx, y1 + 19), name, fill=TEXT, font=FONT_SM, anchor="mm")

# ── Arrow to Gateway ──
y_gateway_top = 220
draw_arrow(draw, 440, y1 + 32, 720, y_gateway_top, ACCENT_1)

# ── Layer 2: Gateway Router (big box) ──
gw_y = 280
gw_w, gw_h = 900, 100
x_gw0 = W // 2 - gw_w // 2
round_rect(draw, [x_gw0, gw_y, x_gw0 + gw_w, gw_y + gw_h], 10, (26, 32, 50), EDGE, 2)
draw.text((W // 2, gw_y + 12), "🔗  agent-proxy  Gateway", fill=TEXT_BRIGHT, font=FONT_B, anchor="mt")
draw.text((W // 2, gw_y + 36), "POST /v1/chat/completions  ·  SSE 流式", fill=ACCENT_1, font=FONT_S, anchor="mt")
draw.text((W // 2, gw_y + 62), "模型路由  →  请求翻译  →  Provider 调用  →  响应翻译", fill=TEXT_DIM, font=FONT_SM, anchor="mt")
draw.text((W // 2, gw_y + 84), "限流（令牌桶）  ·  CORS  ·  请求日志", fill=TEXT_DIM, font=FONT_SM, anchor="mt")

# ── Layer 3: Core (3 parallel modules) ──
y3 = 470
# Arrow from Gateway to Core
draw_arrow(draw, W // 2, gw_y + gw_h + 4, W // 2, y3 - 60, ACCENT_1)

# Module A: ChatCompletion Translator
mx_a = 260
draw_box_centered(mx_a, y3, 260, 140,
                  "ChatCompletion 翻译器", "入口协议翻译器", outline=ACCENT_1,
                  items=["ChatCompletionRequest →", "InternalRequest (中枢)", "响应 + 流式翻译"])

# Module B: Internal Schema (center)
mx_b = W // 2
draw_box_centered(mx_b, y3, 280, 140,
                  "Central Schema 中枢模型", "与所有外部协议无关", fill=(30, 40, 50), outline=ACCENT_2,
                  items=["InternalRequest / Response", "InternalMessage", "InternalToolCall"])

# Module C: Protocol Translators
mx_c = W - 320
draw_box_centered(mx_c, y3, 260, 160,
                  "协议翻译器（输出）", "多协议双向翻译", outline=ACCENT_3,
                  items=["→ Anthropic Messages", "→ Gemini GenerateContent", "→ OpenAI Responses", "→ OpenAI Compatible (透传)"])

# Double arrows: A ↔ B ↔ C
draw_double_arrow(draw, mx_a + 130, y3, mx_b - 140, y3, ACCENT_2)
draw_double_arrow(draw, mx_b + 140, y3, mx_c - 130, y3, ACCENT_2)
label(draw, (mx_b + mx_c) // 2, y3 - 20, "双向翻译", TEXT_DIM, FONT_TINY)
label(draw, (mx_b + mx_c) // 2, y3 - 20, "双向翻译", TEXT_DIM, FONT_TINY)

# ── Layer 4: Provider Clients ──
y4 = 680
draw_arrow(draw, mx_b, y3 + 70, mx_b, y4 - 30, ACCENT_2)

label(draw, W // 2, y4 - 12, "③ Provider 客户端", TEXT_DIM, FONT_S)

providers = [
    ("OpenAIClient", "OpenAI Compatible", ACCENT_1),
    ("AnthropicClient", "Messages API", ACCENT_3),
    ("GeminiClient", "GenerateContent", ACCENT_4),
]
pw = 250
pgap = 30
p_start = W // 2 - (len(providers) * pw + (len(providers) - 1) * pgap) // 2
for i, (name, tag, color) in enumerate(providers):
    px = p_start + pw // 2 + i * (pw + pgap)
    round_rect(draw, [px - pw//2, y4 + 2, px + pw//2, y4 + 64], 8, CARD_BG, color, 2)
    draw.text((px, y4 + 20), name, fill=color, font=FONT_B, anchor="mm")
    draw.text((px, y4 + 42), tag, fill=TEXT_DIM, font=FONT_SM, anchor="mm")

# ── Layer 5: Upstream LLMs ──
y5 = 810
draw_arrow(draw, mx_b, y4 + 66, mx_b, y5 - 20, ACCENT_3)

label(draw, W // 2, y5 - 8, "④ 上游 LLM 端点", TEXT_DIM, FONT_S)

llms = [("Sensenova", "sensenova-6.7-flash-lite", ACCENT_1),
        ("Anthropic", "claude-3-5-sonnet", ACCENT_3),
        ("Gemini", "gemini-2.0-flash", ACCENT_4),
        ("OpenAI", "gpt-4o", TEXT_DIM)]

lw = 250
lgap = 30
l_start = W // 2 - (len(llms) * lw + (len(llms) - 1) * lgap) // 2
for i, (name, model, color) in enumerate(llms):
    lx = l_start + lw // 2 + i * (lw + lgap)
    round_rect(draw, [lx - lw//2, y5 + 4, lx + lw//2, y5 + 58], 8, (22, 26, 40), color, 1)
    draw.text((lx, y5 + 18), name, fill=TEXT_BRIGHT, font=FONT_B, anchor="mm")
    draw.text((lx, y5 + 38), model, fill=TEXT_DIM, font=FONT_SM, anchor="mm")

# ── Right side: Web UI panel ──
ui_x = W - 110
ui_y = 120  # move to top-right area next to client cards
round_rect(draw, [ui_x - 100, ui_y, ui_x + 100, ui_y + 220], 10, (26, 32, 50), ACCENT_5, 2)
draw.text((ui_x, ui_y + 14), "Web UI", fill=ACCENT_5, font=FONT_B, anchor="mm")
draw.text((ui_x, ui_y + 34), "/ui", fill=TEXT_DIM, font=FONT_SM, anchor="mm")
ui_items = ["QPS / 延迟", "Provider 状态", "实时日志", "60s 趋势图"]
for j, item in enumerate(ui_items):
    draw.text((ui_x, ui_y + 56 + j * 34), "▸ " + item, fill=TEXT, font=FONT_SM, anchor="mm")

# Dashed connection Gateway → UI (from gateway top-right to UI bottom-left)
x1, y1 = x_gw0 + gw_w - 30, gw_y
x2, y2 = ui_x - 100, ui_y + 110
# Draw with dashes
dash_len = 8
gap_len = 6
dx, dy = x2 - x1, y2 - y1
total = math.hypot(dx, dy)
t = 0
on = True
while t < total:
    seg = dash_len if on else gap_len
    if t + seg > total:
        seg = total - t
    if on:
        x_a = x1 + dx * t / total
        y_a = y1 + dy * t / total
        x_b = x1 + dx * (t + seg) / total
        y_b = y1 + dy * (t + seg) / total
        draw.line([(x_a, y_a), (x_b, y_b)], fill=ACCENT_5, width=1)
    on = not on
    t += seg
draw.text(((x1 + x2) // 2, (y1 + y2) // 2 + 14), "embed.FS", fill=ACCENT_5, font=FONT_TINY)

# ── Bottom: compatibility note ──
note_y = H - 30
draw.line([(120, note_y + 5), (W - 120, note_y + 5)], fill=EDGE, width=1)
label(draw, 160, note_y, "8 大协议差异自动处理", ACCENT_3, FONT_B)
compat_items = [
    "System prompt 位置",
    "Tool 字段名",
    "Tool call 位置",
    "JSON string vs object",
    "Role 映射",
    "Usage 字段",
    "Stop reason",
    "SSE 格式"
]
label(draw, W // 2, note_y, "·  ".join(compat_items), TEXT_DIM, FONT_SM)

# ── Save ──
output = "d:/src/agent-proxy/docs/architecture.png"
img.save(output, "PNG", optimize=True)
print(f"Saved: {output} ({img.width}x{img.height})")
