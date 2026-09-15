package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"testing"
	"time"
)

// TestNonStreamCallTimeout_Bounded 锁住 NONSTREAM_CALL_TIMEOUT 的取值不变量。
//
// 为什么必须锁：这条链路的回归方式是「把 ctx 超时改回 time.Duration(q.timeout)」——编译期零提示、
// go vet 零提示，唯一的后果是下一次上游挂死时代理静默 5 分钟。本测试读常量并断言上界，
// 改回 q.timeout 语义时立刻失败。
//
// @AI_GUARD: NONSTREAM_CALL_TIMEOUT - 与 quick.go 常量定义同 guard
func TestNonStreamCallTimeout_Bounded(t *testing.T) {
	// 上界：必须与流式路径的静默判据同值。两者都是「上游沉默多久算判死」，不一致会让
	// 同一个代理在流式路径 60s 收尾、非流式路径 300s 收尾——正是 v0.2.146 的故障形状。
	if nonStreamCallTimeout != upstreamStallTimeout {
		t.Errorf("nonStreamCallTimeout = %v，want %v（与 UPSTREAM_STALL_DETECT 同值）",
			nonStreamCallTimeout, upstreamStallTimeout)
	}

	// 硬上界：绝不允许退化回 q.timeout 的量级（默认 300s）。
	if nonStreamCallTimeout > 120*time.Second {
		t.Errorf("nonStreamCallTimeout = %v，超过 120s 上界（q.timeout 默认 300s，退回即回归）",
			nonStreamCallTimeout)
	}

	// 下界：太小会误杀正常长推理。实测感诺 118KB 非流式 ~7.6s、799KB 流式 first_byte 27s。
	if nonStreamCallTimeout < 30*time.Second {
		t.Errorf("nonStreamCallTimeout = %v，小于 30s 下界（会误杀长推理）", nonStreamCallTimeout)
	}

	// 类型必须是 time.Duration，防止有人把常量改成 float64 秒或 int 毫秒后静默失效。
	if reflect.TypeOf(nonStreamCallTimeout).Kind() != reflect.Int64 {
		t.Errorf("nonStreamCallTimeout 类型 = %v，want time.Duration", reflect.TypeOf(nonStreamCallTimeout))
	}
}

// TestNonStreamAsSSE_UsesCallTimeout 用 AST 锁住两个非流式→SSE 包装函数确实引用
// nonStreamCallTimeout，而不是绕过它。
//
// 为什么用 AST 而不是单测调用：这两个函数要挂 http.ResponseWriter + provider.Provider 才能真正
// 触发，构造成本高且测的是实现细节。AST 检查的是「这一行写的是哪个标识符」，这正是唯一会回归的
// 那点（改回 time.Duration(q.timeout)）。与 header_forward_test.go 按函数分组扫描同一思路。
//
// @AI_GUARD: NONSTREAM_CALL_TIMEOUT - 与 quick.go 两个 handler 同 guard
func TestNonStreamAsSSE_UsesCallTimeout(t *testing.T) {
	raw, err := os.ReadFile("quick.go")
	if err != nil {
		t.Fatalf("read quick.go: %v", err)
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "quick.go", string(raw), parser.ParseComments)
	if err != nil {
		t.Fatalf("parse quick.go: %v", err)
	}

	// 只跟踪两个「非流式 p.Call 再包 SSE」的函数：它们的调用方带 stream:true，
	// 客户端会一直等，所以超时必须远小于 300s。
	want := map[string]bool{
		"handleNonStreamResponseAsSSE":    false, // 翻译路径
		"handlePassthroughNonStreamAsSSE": false, // 透传路径
	}

	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 {
			continue
		}
		if _, tracked := want[fn.Name.Name]; !tracked {
			continue
		}

		found := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "WithTimeout" {
				return true
			}
			x, ok := sel.X.(*ast.Ident)
			if !ok || x.Name != "context" {
				return true
			}
			if len(call.Args) < 2 {
				return true
			}
			if arg, ok := call.Args[1].(*ast.Ident); ok && arg.Name == "nonStreamCallTimeout" {
				found = true
			}
			return true
		})
		want[fn.Name.Name] = found
	}

	for name, ok := range want {
		if !ok {
			t.Errorf("QuickGateway.%s 未使用 context.WithTimeout(ctx, nonStreamCallTimeout)——非流式 p.Call 会退回 300s 静默", name)
		}
	}
}
