package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"
)

// @AI_GUARD: LARGE_BODY_SKIP_STREAM_TRANSLATION - 翻译路径**不降级**，始终走原生流式
//
// v0.2.146 曾把大请求降级到 handleNonStreamResponseAsSSE，v0.2.148 撤回：降级产生的
// Responses 出站事件没有 type 字段（只有 object/response/output），Codex 的 WS 路径按
// serde_json::from_str 整帧解析、靠 type 分发事件，全部解析失败被静默丢弃 →
// stream closed before response.completed / os error 10054。
//
// 降级分支（if translationStreamRoute(...)）已整体删除——不留恒真判断这种死代码。

// TestLargeBodyThreshold_Value 锁住阈值本身，防止透传路径的判定被悄悄改宽或改窄。
func TestLargeBodyThreshold_Value(t *testing.T) {
	if largeBodyThreshold != 100*1024 {
		t.Errorf("largeBodyThreshold = %d, want %d", largeBodyThreshold, 100*1024)
	}
}

// TestTranslationRoute_AlwaysStreams 用 AST 锁住 quick.go 的翻译路径流式分支：
// 恰好调用一次 handleStreamRequest，且绝不出现 handleNonStreamResponseAsSSE /
// translationStreamRoute。
//
// 为什么用 AST：该回归方式是「把分支改回 if len(downstreamReq) > threshold 再降级」——
// 编译期、go vet 都零提示，唯一的后果是下一次大请求 Codex 丢响应。测的是
// 「这个块里出现了哪些调用」，正是唯一会回归的那点。与 header_forward_test.go 同一思路。
//
// @AI_GUARD: LARGE_BODY_SKIP_STREAM_TRANSLATION - 与 quick.go 路由分支同 guard
func TestTranslationRoute_AlwaysStreams(t *testing.T) {
	raw, err := os.ReadFile("quick.go")
	if err != nil {
		t.Fatalf("read quick.go: %v", err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "quick.go", string(raw), parser.ParseComments)
	if err != nil {
		t.Fatalf("parse quick.go: %v", err)
	}

	var handleRequestBody *ast.BlockStmt
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 || fn.Name.Name != "handleRequest" {
			continue
		}
		if star, ok := fn.Recv.List[0].Type.(*ast.StarExpr); ok {
			if ident, ok := star.X.(*ast.Ident); ok && ident.Name == "QuickGateway" {
				handleRequestBody = fn.Body
			}
		}
	}
	if handleRequestBody == nil {
		t.Fatalf("未找到 QuickGateway.handleRequest")
	}

	// 定位 translation 路径的 `if stream { ... } else { ... }` 块
	var streamBlock, elseBlock *ast.BlockStmt
	ast.Inspect(handleRequestBody, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok || ifs.Else == nil {
			return true
		}
		if cond, ok := ifs.Cond.(*ast.Ident); ok && cond.Name == "stream" {
			streamBlock = ifs.Body
			elseBlock = ifs.Else.(*ast.BlockStmt)
			return false
		}
		return true
	})
	if streamBlock == nil {
		t.Fatalf("handleRequest 里未找到 if stream { ... } else { ... }（路由形状变了，本测试需同步更新）")
	}

	methodsIn := func(block ast.Node) (streamCalls, downgradeCalls, probeRefs int) {
		ast.Inspect(block, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
					if x, ok := sel.X.(*ast.Ident); ok && x.Name == "q" {
						switch sel.Sel.Name {
						case "handleStreamRequest":
							streamCalls++
						case "handleNonStreamResponseAsSSE", "handlePassthroughNonStreamAsSSE":
							downgradeCalls++
						}
					}
				}
			}
			if ident, ok := n.(*ast.Ident); ok && ident.Name == "translationStreamRoute" {
				probeRefs++
			}
			return true
		})
		return
	}

	t.Run("translation 流式分支", func(t *testing.T) {
		streamCalls, downgradeCalls, probeRefs := methodsIn(streamBlock)
		if streamCalls != 1 {
			t.Errorf("q.handleStreamRequest 调用次数 = %d，want 1（翻译路径必须始终走原生流式）", streamCalls)
		}
		if downgradeCalls != 0 {
			t.Errorf("translation 流式分支出现了 %d 次非流式→SSE 调用 —— 大请求会被降级，"+
				"产生无 type 字段的 Responses 事件，Codex 必然丢响应（v0.2.146/147 回归）", downgradeCalls)
		}
		if probeRefs != 0 {
			t.Errorf("translation 流式分支引用了 translationStreamRoute —— 降级判断被接回了")
		}
	})

	t.Run("translation 非流式分支", func(t *testing.T) {
		streamCalls, downgradeCalls, _ := methodsIn(elseBlock)
		if streamCalls != 0 {
			t.Errorf("translation 非流式分支出现了 %d 次 handleStreamRequest 调用", streamCalls)
		}
		if downgradeCalls != 0 {
			t.Errorf("translation 非流式分支不应走非流式→SSE 包装（%d 次）", downgradeCalls)
		}
	})
}
