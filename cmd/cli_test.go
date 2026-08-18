package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agent-proxy/agent-proxy/internal/db"
)

// ─── Test helpers ───────────────────────────────────────────────────

func initTestDB(t *testing.T) (*string, func()) {
	t.Helper()
	f, err := os.CreateTemp("", "ap-test-*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	path := f.Name()
	old := dbPathOverride
	dbPathOverride = &path
	cleanup := func() {
		dbPathOverride = old
		os.Remove(path)
	}
	return &path, cleanup
}

func makeStub(result *SniffResult) func() {
	old := sniffAllFn
	sniffAllFn = func(url, key string) *SniffResult { return result }
	return func() { sniffAllFn = old }
}

func mkResult(caps []string, modelsMap map[string][]string) *SniffResult {
	if modelsMap == nil {
		modelsMap = make(map[string][]string)
	}
	return &SniffResult{Capabilities: caps, ModelsMap: modelsMap}
}

func runWithStdin(t *testing.T, stdinStr string, fn func() error) (string, error) {
	t.Helper()
	oldOut := os.Stdout
	oldIn := os.Stdin
	defer func() { os.Stdout = oldOut; os.Stdin = oldIn }()

	// stdin pipe
	rIn, wIn, _ := os.Pipe()
	os.Stdin = rIn
	go func() {
		wIn.WriteString(stdinStr)
		wIn.Close()
	}()

	// stdout pipe: capture output while also passing through
	rOut, wOut, _ := os.Pipe()
	os.Stdout = wOut
	var outBuf bytes.Buffer
	done := make(chan struct{})
	go func() {
		io.Copy(&outBuf, rOut)
		close(done)
	}()

	err := fn()
	wOut.Close() // signal the goroutine
	<-done
	os.Stdout = oldOut
	return outBuf.String(), err
}

func now() time.Time {
	return time.Now().Truncate(time.Second)
}

// ─── Mock HTTP server ─────────────────────────────────────────────

func startMockServer(t *testing.T, openAIModels []string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		data := make([]map[string]string, len(openAIModels))
		for i, m := range openAIModels {
			data[i] = map[string]string{"id": m}
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"data": data})
	})

	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{"output": []string{}})
	})

	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"model": "claude-3-opus"})
	})

	mux.HandleFunc("/v1/models/gemini-pro:generateContent", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"candidates": "[]"})
	})

	return httptest.NewServer(mux)
}

// =================================================================
// RunDBUpdate tests
// =================================================================

func TestRunDBUpdate_SingleID(t *testing.T) {
	_, cleanup := initTestDB(t)
	defer cleanup()
	defer makeStub(mkResult(
		[]string{"openai", "anthropic"},
		map[string][]string{"openai": {"gpt-4o"}, "anthropic": {"claude-3-opus"}}),
	)()

	store, _ := openDB()
	store.Init()
	store.Add(&db.ProxyRecord{
		Name:             "test-up",
		URL:              "https://example.com/v1",
		ModelCount:       1,
		CapabilitiesJSON: `["openai"]`,
		ModelsMapJSON:    `{"openai":["gpt-3"]}`,
		CreatedAt:        now(),
	})
	store.Close()

	out, err := runWithStdin(t, "", func() error {
		return RunDBUpdate(1)
	})
	if err != nil {
		t.Fatalf("RunDBUpdate(1) error: %v", err)
	}
	if !strings.Contains(out, "更新完成") {
		t.Fatalf("expected 更新完成, got: %s", out)
	}
	if !strings.Contains(out, "1 条成功") {
		t.Fatalf("expected '1 条成功', got: %s", out)
	}
}

func TestRunDBUpdate_All(t *testing.T) {
	_, cleanup := initTestDB(t)
	defer cleanup()
	defer makeStub(mkResult(
		[]string{"openai", "anthropic", "gemini"},
		map[string][]string{
			"openai":    {"gpt-4o"},
			"anthropic": {"claude-3-opus"},
			"gemini":    {},
		}),
	)()

	store, _ := openDB()
	store.Init()
	store.Add(&db.ProxyRecord{Name: "up1", URL: "https://a.com", CapabilitiesJSON: `["openai"]`, ModelsMapJSON: `{"openai":["m1"]}`, ModelCount: 1, CreatedAt: now()})
	store.Add(&db.ProxyRecord{Name: "up2", URL: "https://b.com", CapabilitiesJSON: `["openai"]`, ModelsMapJSON: `{"openai":["m2"]}`, ModelCount: 1, CreatedAt: now()})
	store.Close()

	out, err := runWithStdin(t, "", func() error {
		return RunDBUpdate(0)
	})
	if err != nil {
		t.Fatalf("RunDBUpdate(0) error: %v", err)
	}
	if !strings.Contains(out, "2 条成功") {
		t.Fatalf("expected '2 条成功', got: %s", out)
	}
}

func TestRunDBUpdate_NonExistentID(t *testing.T) {
	_, cleanup := initTestDB(t)
	defer cleanup()

	_, err := runWithStdin(t, "", func() error {
		return RunDBUpdate(999)
	})
	if err == nil {
		t.Fatal("expected error for non-existent ID")
	}
	if !strings.Contains(err.Error(), "未找到") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestRunDBUpdate_EmptyDB(t *testing.T) {
	_, cleanup := initTestDB(t)
	defer cleanup()

	out, err := runWithStdin(t, "", func() error {
		return RunDBUpdate(0)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "无需更新") {
		t.Fatalf("expected 无需更新, got: %s", out)
	}
}

func TestRunDBUpdate_InvalidUpstream(t *testing.T) {
	_, cleanup := initTestDB(t)
	defer cleanup()
	defer makeStub(mkResult([]string{}, map[string][]string{}))()

	store, _ := openDB()
	store.Init()
	store.Add(&db.ProxyRecord{Name: "dead", URL: "https://dead.com", CapabilitiesJSON: `["openai"]`, ModelsMapJSON: `{"openai":["m1"]}`, ModelCount: 1, CreatedAt: now()})
	store.Close()

	out, err := runWithStdin(t, "", func() error {
		return RunDBUpdate(1)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "1 条无效") {
		t.Fatalf("expected '1 条无效', got: %s", out)
	}
	if !strings.Contains(out, "0 条成功") {
		t.Fatalf("expected '0 条成功', got: %s", out)
	}
}

func TestRunDBUpdate_DBWritePersists(t *testing.T) {
	_, cleanup := initTestDB(t)
	defer cleanup()
	defer makeStub(mkResult(
		[]string{"openai", "anthropic"},
		map[string][]string{"openai": {"gpt-4o", "gpt-4"}, "anthropic": {"claude-3"}}),
	)()

	store, _ := openDB()
	store.Init()
	store.Add(&db.ProxyRecord{Name: "persist", URL: "https://p.com", CapabilitiesJSON: `["openai"]`, ModelsMapJSON: `{"openai":["m1"]}`, ModelCount: 1, CreatedAt: now()})
	store.Close()

	_ = RunDBUpdate(1)

	store2, _ := openDB()
	r, _ := store2.GetByID(1)
	store2.Close()
	if r == nil {
		t.Fatal("record not found after update")
	}
	if r.ModelCount != 3 {
		t.Fatalf("expected model_count=3, got %d", r.ModelCount)
	}
	caps := r.Capabilities()
	if len(caps) != 2 {
		t.Fatalf("expected 2 capabilities, got %d: %v", len(caps), caps)
	}
	if !r.HasCapability("anthropic") {
		t.Fatal("expected anthropic capability after update")
	}
}

// =================================================================
// RunDBCheck tests
// =================================================================

func TestRunDBCheck_AllNoChange(t *testing.T) {
	_, cleanup := initTestDB(t)
	defer cleanup()
	result := mkResult(
		[]string{"openai"},
		map[string][]string{"openai": {"gpt-4o"}})
	defer makeStub(result)()

	store, _ := openDB()
	store.Init()
	capsJSON, _ := json.Marshal(result.Capabilities)
	modelsMapJSON, _ := json.Marshal(result.ModelsMap)
	store.Add(&db.ProxyRecord{
		Name:             "unchanged",
		URL:              "https://ok.com",
		CapabilitiesJSON: string(capsJSON),
		ModelsMapJSON:    string(modelsMapJSON),
		ModelCount:       1,
		CreatedAt:        now(),
	})
	store.Close()

	out, err := runWithStdin(t, "", func() error {
		return RunDBCheck(nil)
	})
	if err != nil {
		t.Fatalf("RunDBCheck(nil) error: %v", err)
	}
	if !strings.Contains(out, "无需操作") {
		t.Fatalf("expected 无需操作, got: %s", out)
	}
}

func TestRunDBCheck_SingleID(t *testing.T) {
	_, cleanup := initTestDB(t)
	defer cleanup()
	defer makeStub(mkResult(
		[]string{"openai"},
		map[string][]string{"openai": {"gpt-4o"}}),
	)()

	store, _ := openDB()
	store.Init()
	store.Add(&db.ProxyRecord{Name: "up1", URL: "https://a.com", CapabilitiesJSON: `["openai"]`, ModelsMapJSON: `{"openai":["m1"]}`, ModelCount: 1, CreatedAt: now()})
	store.Add(&db.ProxyRecord{Name: "up2", URL: "https://b.com", CapabilitiesJSON: `["openai"]`, ModelsMapJSON: `{"openai":["m2"]}`, ModelCount: 1, CreatedAt: now()})
	store.Close()

	id := 2
	out, err := runWithStdin(t, "", func() error {
		return RunDBCheck(&id)
	})
	if err != nil {
		t.Fatalf("RunDBCheck(&2) error: %v", err)
	}
	if !strings.Contains(out, "正在核对 1 条") {
		t.Fatalf("expected 正在核对 1 条, got: %s", out)
	}
	if strings.Contains(out, "正在核对 2 条") {
		t.Fatalf("should check only 1 record: %s", out)
	}
}

func TestRunDBCheck_NonExistentID(t *testing.T) {
	_, cleanup := initTestDB(t)
	defer cleanup()
	defer makeStub(mkResult([]string{"openai"}, map[string][]string{"openai": {"gpt-4o"}}))()

	store, _ := openDB()
	store.Init()
	store.Add(&db.ProxyRecord{Name: "up1", URL: "https://a.com", CapabilitiesJSON: `["openai"]`, ModelsMapJSON: `{"openai":["m1"]}`, ModelCount: 1, CreatedAt: now()})
	store.Close()

	id := 999
	_, err := runWithStdin(t, "", func() error {
		return RunDBCheck(&id)
	})
	if err == nil {
		t.Fatal("expected error for non-existent ID")
	}
	if !strings.Contains(err.Error(), "未找到") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestRunDBCheck_EmptyDB(t *testing.T) {
	_, cleanup := initTestDB(t)
	defer cleanup()

	out, err := runWithStdin(t, "", func() error {
		return RunDBCheck(nil)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "无需核对") {
		t.Fatalf("expected 无需核对, got: %s", out)
	}
}

func TestRunDBCheck_ChangedModels_PromptYes_Updates(t *testing.T) {
	_, cleanup := initTestDB(t)
	defer cleanup()

	before := mkResult([]string{"openai"}, map[string][]string{"openai": {"gpt-3"}})
	after := mkResult([]string{"openai", "anthropic"}, map[string][]string{"openai": {"gpt-4o", "gpt-4"}, "anthropic": {"claude-3"}})
	defer makeStub(after)()

	store, _ := openDB()
	store.Init()
	capsJSON, _ := json.Marshal(before.Capabilities)
	modelsMapJSON, _ := json.Marshal(before.ModelsMap)
	store.Add(&db.ProxyRecord{
		Name:             "changed",
		URL:              "https://ch.com",
		ModelCount:       1,
		CapabilitiesJSON: string(capsJSON),
		ModelsMapJSON:    string(modelsMapJSON),
		CreatedAt:        now(),
	})
	store.Close()

	out, err := runWithStdin(t, "yes\n", func() error {
		return RunDBCheck(nil)
	})
	if err != nil {
		t.Fatalf("RunDBCheck error: %v", err)
	}
	if !strings.Contains(out, "已更新") {
		t.Fatalf("expected 已更新, got: %s", out)
	}
	if !strings.Contains(out, "是否更新") {
		t.Fatalf("expected 是否更新 prompt, got: %s", out)
	}

	store2, _ := openDB()
	r, _ := store2.GetByID(1)
	store2.Close()
	if r == nil {
		t.Fatal("record not found")
	}
	if r.ModelCount != 3 {
		t.Fatalf("expected model_count=3 after update, got %d", r.ModelCount)
	}
}

func TestRunDBCheck_ChangedModels_PromptNo_Skips(t *testing.T) {
	_, cleanup := initTestDB(t)
	defer cleanup()

	before := mkResult([]string{"openai"}, map[string][]string{"openai": {"gpt-3"}})
	after := mkResult([]string{"openai", "anthropic"}, map[string][]string{"openai": {"gpt-4o", "gpt-4"}, "anthropic": {"claude-3"}})
	defer makeStub(after)()

	store, _ := openDB()
	store.Init()
	capsJSON, _ := json.Marshal(before.Capabilities)
	modelsMapJSON, _ := json.Marshal(before.ModelsMap)
	store.Add(&db.ProxyRecord{
		Name:             "changed",
		URL:              "https://ch.com",
		ModelCount:       1,
		CapabilitiesJSON: string(capsJSON),
		ModelsMapJSON:    string(modelsMapJSON),
		CreatedAt:        now(),
	})
	store.Close()

	_, err := runWithStdin(t, "no\n", func() error {
		return RunDBCheck(nil)
	})
	if err != nil {
		t.Fatalf("RunDBCheck error: %v", err)
	}

	store2, _ := openDB()
	r, _ := store2.GetByID(1)
	store2.Close()
	if r == nil {
		t.Fatal("record not found")
	}
	if r.ModelCount != 1 {
		t.Fatalf("expected model_count to stay 1 (no update), got %d", r.ModelCount)
	}
}

func TestRunDBCheck_InvalidUpstream_PromptYes_Deletes(t *testing.T) {
	_, cleanup := initTestDB(t)
	defer cleanup()
	defer makeStub(mkResult([]string{}, map[string][]string{}))()

	store, _ := openDB()
	store.Init()
	store.Add(&db.ProxyRecord{Name: "dead", URL: "https://dead.com", CapabilitiesJSON: `["openai"]`, ModelsMapJSON: `{"openai":["m1"]}`, ModelCount: 1, CreatedAt: now()})
	store.Close()

	out, err := runWithStdin(t, "yes\n", func() error {
		return RunDBCheck(nil)
	})
	if err != nil {
		t.Fatalf("RunDBCheck error: %v", err)
	}
	if !strings.Contains(out, "已删除") {
		t.Fatalf("expected 已删除, got: %s", out)
	}

	store2, _ := openDB()
	count := store2.Count()
	store2.Close()
	if count != 0 {
		t.Fatalf("expected 0 records after delete, got %d", count)
	}
}

func TestRunDBCheck_InvalidUpstream_PromptNo_Keeps(t *testing.T) {
	_, cleanup := initTestDB(t)
	defer cleanup()
	defer makeStub(mkResult([]string{}, map[string][]string{}))()

	store, _ := openDB()
	store.Init()
	store.Add(&db.ProxyRecord{Name: "dead", URL: "https://dead.com", CapabilitiesJSON: `["openai"]`, ModelsMapJSON: `{"openai":["m1"]}`, ModelCount: 1, CreatedAt: now()})
	store.Close()

	_, err := runWithStdin(t, "no\n", func() error {
		return RunDBCheck(nil)
	})
	if err != nil {
		t.Fatalf("RunDBCheck error: %v", err)
	}

	store2, _ := openDB()
	count := store2.Count()
	store2.Close()
	if count != 1 {
		t.Fatalf("expected 1 record (not deleted), got %d", count)
	}
}

func TestRunDBCheck_ChangeTableShowsDiff(t *testing.T) {
	_, cleanup := initTestDB(t)
	defer cleanup()

	before := mkResult([]string{"openai"}, map[string][]string{"openai": {"gpt-3", "gpt-3.5"}})
	after := mkResult([]string{"openai", "anthropic"}, map[string][]string{"openai": {"gpt-4o", "gpt-4"}, "anthropic": {"claude-3"}})
	defer makeStub(after)()

	store, _ := openDB()
	store.Init()
	capsJSON, _ := json.Marshal(before.Capabilities)
	modelsMapJSON, _ := json.Marshal(before.ModelsMap)
	store.Add(&db.ProxyRecord{
		Name:             "ch",
		URL:              "https://ch.com",
		ModelCount:       2,
		CapabilitiesJSON: string(capsJSON),
		ModelsMapJSON:    string(modelsMapJSON),
		CreatedAt:        now(),
	})
	store.Close()

	out, err := runWithStdin(t, "no\n", func() error {
		return RunDBCheck(nil)
	})
	if err != nil {
		t.Fatalf("RunDBCheck error: %v", err)
	}
	if !strings.Contains(out, "2→3") {
		t.Fatalf("expected change indicator 2→3 in output, got: %s", out)
	}
}

// =================================================================
// readYesNo tests
// =================================================================

func TestReadYesNo(t *testing.T) {
	cases := []struct {
		input    string
		expected bool
	}{
		{"yes\n", true},
		{"y\n", true},
		{"no\n", false},
		{"n\n", false},
		{"YES\n", true},
		{"  yes  \n", true},
		{"maybe\n", false},
	}
	for _, tc := range cases {
		f, err := os.CreateTemp("", "yn-")
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString(tc.input)
		f.Seek(0, 0)
		got := readYesNo(f)
		f.Close()
		os.Remove(f.Name())
		if got != tc.expected {
			t.Errorf("readYesNo(%q) = %v, want %v", tc.input, got, tc.expected)
		}
	}
}

// =================================================================
// sniffAll + real HTTP test
// =================================================================

func TestSniffAll_WithMockServer(t *testing.T) {
	srv := startMockServer(t, []string{"gpt-4o", "gpt-3.5-turbo"})
	defer srv.Close()

	result := sniffAll(srv.URL, "test-key")

	if !contains(result.Capabilities, "openai") {
		t.Error("expected openai capability")
	}
	if !contains(result.Capabilities, "anthropic") {
		t.Error("expected anthropic capability")
	}
	if !contains(result.Capabilities, "gemini") {
		t.Error("expected gemini capability")
	}
	if !contains(result.Capabilities, "responses") {
		t.Error("expected responses capability")
	}

	openAIModels := result.ModelsMap["openai"]
	if len(openAIModels) != 2 {
		t.Fatalf("expected 2 openai models, got %d: %v", len(openAIModels), openAIModels)
	}
	if openAIModels[0] != "gpt-4o" || openAIModels[1] != "gpt-3.5-turbo" {
		t.Fatalf("unexpected models: %v", openAIModels)
	}
}

func contains(s []string, target string) bool {
	for _, v := range s {
		if v == target {
			return true
		}
	}
	return false
}

// =================================================================
// parseOpenAIModels tests
// =================================================================

func TestParseOpenAIModels(t *testing.T) {
	body := `{"data":[{"id":"gpt-4o"},{"id":"gpt-3.5-turbo"},{"id":"claude-3"}]}`
	reader := strings.NewReader(body)
	models := parseOpenAIModels(reader)
	if len(models) != 3 {
		t.Fatalf("expected 3 models, got %d: %v", len(models), models)
	}
	if models[0] != "gpt-4o" || models[2] != "claude-3" {
		t.Fatalf("unexpected models: %v", models)
	}
}

func TestParseOpenAIModels_InvalidJSON(t *testing.T) {
	reader := strings.NewReader(`not json`)
	models := parseOpenAIModels(reader)
	if models != nil {
		t.Fatalf("expected nil, got %v", models)
	}
}

func TestParseOpenAIModels_NoDataField(t *testing.T) {
	reader := strings.NewReader(`{"models":["gpt-4o"]}`)
	models := parseOpenAIModels(reader)
	if models != nil {
		t.Fatalf("expected nil, got %v", models)
	}
}

// =================================================================
// DB Update method tests
// =================================================================

func TestDB_Update(t *testing.T) {
	_, cleanup := initTestDB(t)
	defer cleanup()
	store, _ := openDB()
	store.Init()

	store.Add(&db.ProxyRecord{
		Name:             "original",
		URL:              "https://orig.com",
		ModelCount:       2,
		ModelsJSON:       `["m1","m2"]`,
		CapabilitiesJSON: `["openai"]`,
		ModelsMapJSON:    `{"openai":["m1","m2"]}`,
		UpstreamType:     "openai-compat",
		CreatedAt:        now(),
	})

	updated := &db.ProxyRecord{
		ID:               1,
		ModelCount:       5,
		ModelsJSON:       `["m1","m2","m3","m4","m5"]`,
		CapabilitiesJSON: `["openai","anthropic"]`,
		ModelsMapJSON:    `{"openai":["m1","m2","m3","m4","m5"],"anthropic":["claude-3"]}`,
		UpstreamType:     "anthropic-compat",
	}
	if err := store.Update(updated); err != nil {
		t.Fatal(err)
	}

	r, err := store.GetByID(1)
	if err != nil {
		t.Fatal(err)
	}
	if r.ModelCount != 5 {
		t.Fatalf("expected ModelCount=5, got %d", r.ModelCount)
	}
	if r.UpstreamType != "anthropic-compat" {
		t.Fatalf("expected upstream_type=anthropic-compat, got %s", r.UpstreamType)
	}
	caps := r.Capabilities()
	if len(caps) != 2 {
		t.Fatalf("expected 2 capabilities, got %v", caps)
	}
	if !r.HasCapability("anthropic") {
		t.Fatal("expected anthropic capability")
	}
	if r.Name != "original" {
		t.Fatalf("expected Name=original, got %s", r.Name)
	}
	if r.URL != "https://orig.com" {
		t.Fatalf("expected URL=https://orig.com, got %s", r.URL)
	}
}

func TestDB_Update_NonExistent(t *testing.T) {
	_, cleanup := initTestDB(t)
	defer cleanup()
	store, _ := openDB()
	store.Init()

	err := store.Update(&db.ProxyRecord{ID: 999, ModelCount: 1})
	if err != nil {
		t.Fatalf("Update should not error for non-existent ID, got: %v", err)
	}
}

// =================================================================
// ProxyRecord tests
// =================================================================

func TestProxyRecord_Capabilities(t *testing.T) {
	rec := &db.ProxyRecord{CapabilitiesJSON: `["openai","anthropic"]`}
	caps := rec.Capabilities()
	if len(caps) != 2 {
		t.Fatalf("expected 2, got %v", caps)
	}
}

func TestProxyRecord_Capabilities_Empty(t *testing.T) {
	rec := &db.ProxyRecord{ProviderType: "openai"}
	caps := rec.Capabilities()
	if len(caps) != 1 || caps[0] != "openai" {
		t.Fatalf("expected [openai], got %v", caps)
	}
}

func TestProxyRecord_ModelsMap(t *testing.T) {
	rec := &db.ProxyRecord{ModelsMapJSON: `{"openai":["gpt-4"],"anthropic":["claude-3"]}`}
	mm := rec.ModelsMap()
	if len(mm) != 2 {
		t.Fatalf("expected 2 protocol groups, got %d", len(mm))
	}
	if len(mm["openai"]) != 1 || mm["openai"][0] != "gpt-4" {
		t.Fatalf("unexpected openai models: %v", mm["openai"])
	}
}

func TestProxyRecord_TotalModelCount(t *testing.T) {
	rec := &db.ProxyRecord{ModelsMapJSON: `{"openai":["gpt-4","gpt-3"],"anthropic":["claude-3"]}`}
	if rec.TotalModelCount() != 3 {
		t.Fatalf("expected 3, got %d", rec.TotalModelCount())
	}
}

func TestProxyRecord_TotalModelCount_Legacy(t *testing.T) {
	rec := &db.ProxyRecord{ModelsJSON: `["m1","m2"]`}
	if rec.TotalModelCount() != 2 {
		t.Fatalf("expected 2, got %d", rec.TotalModelCount())
	}
}

func TestProxyRecord_ModelsForProtocol(t *testing.T) {
	rec := &db.ProxyRecord{ModelsMapJSON: `{"openai":["gpt-4"],"anthropic":["claude-3"]}`}
	models := rec.ModelsForProtocol("anthropic")
	if len(models) != 1 || models[0] != "claude-3" {
		t.Fatalf("unexpected: %v", models)
	}
}

func TestProxyRecord_HasCapability(t *testing.T) {
	rec := &db.ProxyRecord{CapabilitiesJSON: `["openai","anthropic"]`}
	if !rec.HasCapability("openai") {
		t.Fatal("expected HasCapability(openai)=true")
	}
	if rec.HasCapability("gemini") {
		t.Fatal("expected HasCapability(gemini)=false")
	}
}

// =================================================================
// SniffResult tests
// =================================================================

func TestSniffResult_HasProto(t *testing.T) {
	sr := &SniffResult{Capabilities: []string{"openai", "anthropic"}}
	if !sr.hasProto("openai") {
		t.Fatal("expected hasProto(openai)=true")
	}
	if sr.hasProto("gemini") {
		t.Fatal("expected hasProto(gemini)=false")
	}
}

// =================================================================
// Command dispatch reachability tests
// =================================================================

func TestDispatch_UpdateCommand(t *testing.T) {
	_, cleanup := initTestDB(t)
	defer cleanup()
	defer makeStub(mkResult([]string{"openai"}, map[string][]string{"openai": {"gpt-4o"}}))()
	_ = RunDBUpdate(0)
}

func TestDispatch_CheckCommand(t *testing.T) {
	_, cleanup := initTestDB(t)
	defer cleanup()
	defer makeStub(mkResult([]string{"openai"}, map[string][]string{"openai": {"gpt-4o"}}))()
	_ = RunDBCheck(nil)
}
