package server

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"testing"
)

// 本文件守的是 2026-09-14 那次 /v1/responses 非流式 Content-Length: 527 只传 319 字节的事故。
//
// 根因：「透传下游响应头」的循环把上游 Content-Length 一起转发了。上游 CL 是按上游响应体
// 算的，而代理写出的体是别名回显 / 协议翻译后的短体；Go 在 WriteHeader(200) 之后按真实体
// 重算 CL，两个长度同时进 Header map 就是错的，非流式客户端按 CL 读会报 IncompleteRead。
//
// 过滤规则早就有了（quick.go isConnectionManagementHeader），但 8 个转发点里只有 3 个用了。
// 这里两个测试：一个锁住规则本身，一个用 AST 锁住「每个转发点都必须过滤」——后者是防
// 回归的关键，因为这个项目的失败模式历来是「修 A 坏 B」。

// TestIsConnectionManagementHeader 锁住过滤集合。少任何一个键，上游那个按上游体算的长度
// 就会漏回下游。
func TestIsConnectionManagementHeader(t *testing.T) {
	t.Parallel()

	drop := []string{
		"content-length",    // 本次事故的直接原因
		"transfer-encoding", // 与 CL 互斥
		"connection",
		"keep-alive",
		"upgrade",
		"proxy-connection",
		"trailer",
	}
	for _, h := range drop {
		if !isConnectionManagementHeader(h) {
			t.Errorf("isConnectionManagementHeader(%q) = false, want true", h)
		}
	}

	// 大小写不敏感（HTTP/1.1 header 名不区分大小写）
	for _, h := range []string{"CONTENT-LENGTH", "Transfer-Encoding", "Content-Length", "KEEP-ALIVE"} {
		if !isConnectionManagementHeader(h) {
			t.Errorf("isConnectionManagementHeader(%q) = false, want true（大小写）", h)
		}
	}

	// 这些必须放行：Content-Type 是客户端需要的，请求追踪头同理。
	// 误杀会把有用的透传头一起删掉，属于另一种回归。
	keep := []string{
		"content-type",
		"x-request-id",
		"access-control-allow-origin",
		"access-control-expose-headers",
		"x-upstream-extra",
		"x-ratelimit-remaining",
	}
	for _, h := range keep {
		if isConnectionManagementHeader(h) {
			t.Errorf("isConnectionManagementHeader(%q) = true, want false", h)
		}
	}
}

// TestNoUnfilteredHeaderForwarding 用 AST 扫 quick.go / gateway.go，
// 要求每个遍历响应头的循环体内都出现 isConnectionManagementHeader 调用。
//
// 覆盖两种写法：`for k, v := range headers`（provider.Call 返回的拷贝）和
// `for k, v := range resp.Header`（net/http Response 直接遍历，handleModels 用）。
// 只扫 headers 时 handleModels 的别名注入分支漏网——那次写的是重编码后的体，上游 CL 同样错位。
//
// 为什么用 AST 而不是字符串匹配：这些循环只有 6 行，靠行号锚点会随任何无关改动失效；
// AST 匹配的是语义（遍历响应头的 RangeStmt），跟格式无关。
//
// 新增转发点时如果忘了加过滤，这里会直接失败并打印文件:行号。
func TestNoUnfilteredHeaderForwarding(t *testing.T) {
	t.Parallel()

	for _, src := range []string{"quick.go", "gateway.go"} {
		src := src
		t.Run(src, func(t *testing.T) {
			t.Parallel()
			guard(t, src)
		})
	}
}

// headerRangeVars 是「遍历上游响应头」的 RangeStmt.X 可接受形态。
var headerRangeVars = map[string]bool{
	"headers": true,
}

func isHeaderRangeX(x ast.Node) bool {
	if id, ok := x.(*ast.Ident); ok {
		return headerRangeVars[id.Name]
	}
	if sel, ok := x.(*ast.SelectorExpr); ok {
		if sel.Sel.Name == "Header" {
			return true
		}
	}
	return false
}

func guard(t *testing.T, rel string) {
	t.Helper()

	dir := pkgDir(t)
	path := filepath.Join(dir, rel)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("解析 %s 失败: %v", rel, err)
	}

	var found int
	ast.Inspect(f, func(n ast.Node) bool {
		rs, ok := n.(*ast.RangeStmt)
		if !ok {
			return true
		}
		if !isHeaderRangeX(rs.X) {
			return true
		}
		found++
		if hasConnFilter(rs.Body) {
			return true
		}
		line := fset.Position(rs.Pos()).Line
		t.Errorf("%s:%d: %q 缺 isConnectionManagementHeader 过滤 —— "+
			"上游 Content-Length 会透传成错误值（见本文件头部说明）",
			rel, line, fmt.Sprintf("for k, v := range %s", astString(fset, rs.X)))
		return true
	})

	if found == 0 {
		t.Fatalf("%s 里没找到任何响应头转发循环——测试锚点可能已过时", rel)
	}
	t.Logf("%s: %d 个响应头转发点全部带 hop-by-hop 过滤", rel, found)
}

// hasConnFilter 判断循环体内是否出现 isConnectionManagementHeader(...) 调用。
// 同包无接收者，所以 AST 里是 ast.Ident 而不是 SelectorExpr。
func hasConnFilter(body ast.Node) bool {
	got := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok {
			return true
		}
		if id.Name == "isConnectionManagementHeader" {
			got = true
		}
		return true
	})
	return got
}

// pkgDir 返回本测试所在目录（即包目录），避免写死绝对路径。
func pkgDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller 失败")
	}
	return filepath.Dir(file)
}

// astString 把 RangeStmt.X 还原成源码形态（headers / resp.Header），只用于报错信息。
func astString(fset *token.FileSet, n ast.Node) string {
	if id, ok := n.(*ast.Ident); ok {
		return id.Name
	}
	if sel, ok := n.(*ast.SelectorExpr); ok {
		if id, ok := sel.X.(*ast.Ident); ok {
			return id.Name + "." + sel.Sel.Name
		}
	}
	return "<expr>"
}
