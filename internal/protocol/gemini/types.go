package gemini

import "encoding/json"

// ═══════════════════════════════════════════════════════════════════════════════
//  Google Gemini API 协议类型
//
//  兼容性关键差异（见 schema/internal.go 注释）：
//  - 端点格式: /v1/models/{model}:generateContent
//  - System prompt 在顶层 systemInstruction 字段
//  - Role 映射：user ↔ user, assistant ↔ model, system ↔ systemInstruction
//  - Content 用 parts 数组而非 content blocks
//  - Tool 定义为 functionDeclarations 数组（嵌套一层）
//  - Tool 调用用 functionCall part，function 返回用 functionResponse part
//  - 没有 tool_call_id → 网关需生成 ID
//  - Usage 字段名：prompt_token_count / candidates_token_count / total_token_count
//  - 流式：每行 data 是标准对象，无 delta 字段
// ═══════════════════════════════════════════════════════════════════════════════

// GenerateContentRequest Gemini 请求
type GenerateContentRequest struct {
	Model             string            `json:"model,omitempty"`
	Contents          []Content         `json:"contents"`
	Tools             *ToolConfig       `json:"tools,omitempty"`
	SystemInstruction *Content          `json:"systemInstruction,omitempty"`
	GenerationConfig  *GenerationConfig `json:"generationConfig,omitempty"`
	SafetySettings    []SafetySetting   `json:"safetySettings,omitempty"`
	Stream            bool              `json:"stream,omitempty"`
}

type Content struct {
	Role  string `json:"role"` // "user" | "model" | "system"
	Parts []Part `json:"parts"`
}

type Part struct {
	Text             string            `json:"text,omitempty"`
	InlineData       *InlineData       `json:"inline_data,omitempty"`
	FunctionCall     *FunctionCall     `json:"function_call,omitempty"`
	FunctionResponse *FunctionResponse `json:"function_response,omitempty"`
}

type InlineData struct {
	MimeType string `json:"mime_type"`
	Data     string `json:"data"`
}

type ToolConfig struct {
	FunctionDeclarations []FunctionDeclaration `json:"functionDeclarations"`
}

type FunctionDeclaration struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
}

type FunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"` // JSON 对象！不是字符串
}

type FunctionResponse struct {
	Name     string                 `json:"name"`
	Response map[string]interface{} `json:"response"` // {result: "..."} 或嵌套对象
}

type GenerationConfig struct {
	CandidateCount   int             `json:"candidateCount,omitempty"`
	MaxOutputTokens  int             `json:"maxOutputTokens,omitempty"`
	StopSequences    []string        `json:"stopSequences,omitempty"`
	Temperature      *float64        `json:"temperature,omitempty"`
	TopP             *float64        `json:"topP,omitempty"`
	TopK             *int            `json:"topK,omitempty"`
	ResponseMimeType string          `json:"responseMimeType,omitempty"` // "text/plain" | "application/json"
	ResponseSchema   *ResponseSchema `json:"responseSchema,omitempty"`
}

type ResponseSchema struct {
	Type  string                 `json:"type"` // "OBJECT" | "STRING"
	Props map[string]interface{} `json:"properties,omitempty"`
	Keys  []string               `json:"required,omitempty"`
}

type SafetySetting struct {
	Category  string `json:"category"`
	Threshold string `json:"threshold"`
}

// GenerateContentResponse Gemini 响应
type GenerateContentResponse struct {
	Candidates    []Candidate    `json:"candidates"`
	UsageMetadata *UsageMetadata `json:"usageMetadata,omitempty"`
	ModelVersion  string         `json:"modelVersion,omitempty"`
}

type Candidate struct {
	Content       *Content       `json:"content"`
	FinishReason  string         `json:"finishReason"` // "STOP" | "MAX_TOKENS" | "SAFETY" | "RECITATION"
	Index         int            `json:"index"`
	SafetyRatings []SafetyRating `json:"safetyRatings,omitempty"`
	FinishMessage string         `json:"finishMessage,omitempty"`
}

type SafetyRating struct {
	Category    string `json:"category"`
	Probability string `json:"probability"`
	Blocked     bool   `json:"blocked,omitempty"`
}

type UsageMetadata struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
}

// StreamChunk Gemini 流式块（每行一个 JSON）
type StreamChunk struct {
	Candidates    []Candidate    `json:"candidates"`
	UsageMetadata *UsageMetadata `json:"usageMetadata"`
	ModelVersion  string         `json:"modelVersion"`
}

// GeminiError
type GeminiError struct {
	Error *ErrorDetail `json:"error"`
}

type ErrorDetail struct {
	Code    int           `json:"code"`
	Message string        `json:"message"`
	Status  string        `json:"status"`
	Details []interface{} `json:"details,omitempty"`
}
