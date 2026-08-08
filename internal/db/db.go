package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	_ "modernc.org/sqlite"
)

// ProxyRecord 数据库中保存的代理配置
type ProxyRecord struct {
	ID             int       `db:"id"`
	Name           string    `db:"name"`
	URL            string    `db:"url"`
	Key            string    `db:"key"`
	ProviderType   string    `db:"provider_type"`   // "openai" | "anthropic" | "gemini"
	DetectedFormat string    `db:"detected_format"` // 检测到的协议格式
	OpenAICap      bool      `db:"openai_cap"`
	AnthropicCap   bool      `db:"anthropic_cap"`
	ModelCount     int       `db:"model_count"`
	ModelsJSON     string    `db:"models_json"`
	// 多协议支持：存储嗅探出的所有协议能力和每协议模型列表
	CapabilitiesJSON string  `db:"capabilities_json"` // JSON: ["openai","anthropic","gemini","responses"]
	ModelsMapJSON    string  `db:"models_map_json"`   // JSON: {"openai":["gpt-4"],"anthropic":["claude-3"]}
	Weight         int       `db:"weight"`

	CreatedAt      time.Time `db:"created_at"`
}

// Models 解析 models_json 为字符串切片
func (r *ProxyRecord) Models() []string {
	var ids []string
	if r.ModelsJSON == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(r.ModelsJSON), &ids); err != nil {
		return nil
	}
	return ids
}

// Capabilities 解析 capabilities_json 为协议列表
func (r *ProxyRecord) Capabilities() []string {
	var caps []string
	if r.CapabilitiesJSON == "" {
		// 兼容旧记录：从 ProviderType / OpenAICap / AnthropicCap 推断
		if r.ProviderType != "" {
			caps = append(caps, r.ProviderType)
		}
		return caps
	}
	if err := json.Unmarshal([]byte(r.CapabilitiesJSON), &caps); err != nil {
		return nil
	}
	return caps
}

// ModelsForProtocol 返回指定协议下检测到的模型列表
func (r *ProxyRecord) ModelsForProtocol(proto string) []string {
	var m map[string][]string
	if r.ModelsMapJSON == "" {
		// 兼容旧记录：所有模型视为默认协议的
		return r.Models()
	}
	if err := json.Unmarshal([]byte(r.ModelsMapJSON), &m); err != nil {
		return nil
	}
	return m[proto]
}

// HasCapability 检查是否支持指定协议
func (r *ProxyRecord) HasCapability(proto string) bool {
	return slices.Contains(r.Capabilities(), proto)
}

// ModelsMap 解析 models_map_json 为 map[string][]string
func (r *ProxyRecord) ModelsMap() map[string][]string {
	var m map[string][]string
	if r.ModelsMapJSON == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(r.ModelsMapJSON), &m); err != nil {
		return nil
	}
	return m
}
// 返回所有协议下检测到的模型总数
func (r *ProxyRecord) TotalModelCount() int {
	if r.ModelsMapJSON != "" {
		var m map[string][]string
		if err := json.Unmarshal([]byte(r.ModelsMapJSON), &m); err == nil {
			total := 0
			for _, ms := range m {
				total += len(ms)
			}
			return total
		}
	}
	// 兼容旧记录
	return len(r.Models())
}

type DB struct {
	db *sql.DB
}

// New 创建/打开数据库，默认放在 ~/.agent-proxy/proxies.db
func New(dbPath string) (*DB, error) {
	if dbPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("get user home: %w", err)
		}
		dbPath = filepath.Join(home, ".agent-proxy", "proxies.db")
	}
	dir := filepath.Dir(dbPath)
	if dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db directory: %w", err)
		}
	}
	connStr := fmt.Sprintf("file:%s?mode=rwc", dbPath)
	sqlDB, err := sql.Open("sqlite", connStr)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	_, _ = sqlDB.Exec("PRAGMA foreign_keys = ON")
	return &DB{db: sqlDB}, nil
}

func (d *DB) Close() error {
	if d.db != nil {
		return d.db.Close()
	}
	return nil
}

// Init 创建表（幂等）
func (d *DB) Init() error {
	_, err := d.db.Exec(`
		CREATE TABLE IF NOT EXISTS proxies (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			name            TEXT NOT NULL DEFAULT '',
			url             TEXT NOT NULL,
			key             TEXT NOT NULL,
			provider_type   TEXT NOT NULL DEFAULT 'openai',
			detected_format TEXT,
			openai_cap      INTEGER NOT NULL DEFAULT 0,
			anthropic_cap   INTEGER NOT NULL DEFAULT 0,
			model_count     INTEGER NOT NULL DEFAULT 0,
			models_json     TEXT,
			weight          INTEGER NOT NULL DEFAULT 100,
			created_at      TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_proxies_type ON proxies(provider_type);
		CREATE INDEX IF NOT EXISTS idx_proxies_url ON proxies(url);
	`)
	if err != nil {
		return err
	}
	// 迁移：添加多协议列（旧表没有 IF NOT EXISTS 支持）
	if !d.columnExists("proxies", "capabilities_json") {
		_, _ = d.db.Exec("ALTER TABLE proxies ADD COLUMN capabilities_json TEXT")
	}
	if !d.columnExists("proxies", "models_map_json") {
		_, _ = d.db.Exec("ALTER TABLE proxies ADD COLUMN models_map_json TEXT")
	}
	return nil
}

// columnExists 检查表是否已包含指定列
func (d *DB) columnExists(table, column string) bool {
	rows, err := d.db.Query(fmt.Sprintf("PRAGMA table_info('%s')", table))
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dfltVal interface{}
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltVal, &pk); err != nil {
			continue
		}
		if name == column {
			return true
		}
	}
	return false
}

// Add 添加一条代理记录
func (d *DB) Add(record *ProxyRecord) error {
	modelsJSON, err := json.Marshal(record.Models())
	if err != nil {
		return fmt.Errorf("marshal models: %w", err)
	}
	capsJSON, err := json.Marshal(record.Capabilities())
	if err != nil {
		return fmt.Errorf("marshal capabilities: %w", err)
	}
	result, err := d.db.Exec(`
		INSERT INTO proxies (name, url, key, provider_type, detected_format, openai_cap, anthropic_cap, model_count, models_json, capabilities_json, models_map_json, weight, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, record.Name, record.URL, record.Key, record.ProviderType, record.DetectedFormat,
		boolInt(record.OpenAICap), boolInt(record.AnthropicCap), record.ModelCount,
		string(modelsJSON), string(capsJSON), record.ModelsMapJSON,
		record.Weight, record.CreatedAt.Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("insert proxy: %w", err)
	}
	id, _ := result.LastInsertId()
	record.ID = int(id)
	return nil
}

func scanRecord(rows *sql.Rows, r *ProxyRecord) error {
	var oaiCap, antCap int64
	var ts string
	var ns1, ns2, ns3 sql.NullString
	if err := rows.Scan(&r.ID, &r.Name, &r.URL, &r.Key, &r.ProviderType, &r.DetectedFormat, &oaiCap, &antCap, &r.ModelCount, &ns1, &ns2, &ns3, &r.Weight, &ts); err != nil {
		return err
	}
	r.ModelsJSON = ns1.String
	r.CapabilitiesJSON = ns2.String
	r.ModelsMapJSON = ns3.String
	r.OpenAICap = oaiCap != 0
	r.AnthropicCap = antCap != 0
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		r.CreatedAt = t
	}
	return nil
}

// List 列出所有代理记录
func (d *DB) List() ([]ProxyRecord, error) {
	rows, err := d.db.Query(`
		SELECT id, name, url, key, provider_type, detected_format, openai_cap, anthropic_cap, model_count, models_json, capabilities_json, models_map_json, weight, created_at
		FROM proxies
		ORDER BY id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []ProxyRecord
	for rows.Next() {
		var r ProxyRecord
		if err := scanRecord(rows, &r); err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

// GetByID 按 ID 查询一条记录
func (d *DB) GetByID(id int) (*ProxyRecord, error) {
	var r ProxyRecord
	var oaiCap, antCap int64
	var ts string
	var ns1, ns2, ns3 sql.NullString
	err := d.db.QueryRow(`
		SELECT id, name, url, key, provider_type, detected_format, openai_cap, anthropic_cap, model_count, models_json, capabilities_json, models_map_json, weight, created_at
		FROM proxies WHERE id = ?
	`, id).Scan(&r.ID, &r.Name, &r.URL, &r.Key, &r.ProviderType, &r.DetectedFormat, &oaiCap, &antCap, &r.ModelCount, &ns1, &ns2, &ns3, &r.Weight, &ts)
	if err != nil {
		return nil, err
	}
	r.ModelsJSON = ns1.String
	r.CapabilitiesJSON = ns2.String
	r.ModelsMapJSON = ns3.String
	r.OpenAICap = oaiCap != 0
	r.AnthropicCap = antCap != 0
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		r.CreatedAt = t
	}
	return &r, nil
}

// Delete 删除记录
func (d *DB) Delete(id int) error {
	_, err := d.db.Exec("DELETE FROM proxies WHERE id = ?", id)
	return err
}

// Truncate 清空表并重置自增 ID
func (d *DB) Truncate() error {
	_, _ = d.db.Exec("DELETE FROM proxies")
	_, err := d.db.Exec("DELETE FROM sqlite_sequence WHERE name = 'proxies'")
	return err
}

// Count 返回记录数
func (d *DB) Count() int {
	var count int
	_ = d.db.QueryRow("SELECT COUNT(*) FROM proxies").Scan(&count)
	return count
}

// Exists 检查记录是否存在
func (d *DB) Exists(id int) bool {
	var count int
	_ = d.db.QueryRow("SELECT COUNT(*) FROM proxies WHERE id = ?", id).Scan(&count)
	return count > 0
}

// ExistsByURL 检查 URL 是否已存在
func (d *DB) ExistsByURL(url string) bool {
	var count int
	_ = d.db.QueryRow("SELECT COUNT(*) FROM proxies WHERE url = ?", url).Scan(&count)
	return count > 0
}

// Search 按 LIKE 关键词搜索记录（匹配 name / url / capabilities_json / models_map_json）
func (d *DB) Search(query string) ([]ProxyRecord, error) {
	like := "%" + query + "%"
	rows, err := d.db.Query(`
		SELECT id, name, url, key, provider_type, detected_format, openai_cap, anthropic_cap, model_count, models_json, capabilities_json, models_map_json, weight, created_at
		FROM proxies
		WHERE name LIKE ? OR url LIKE ? OR capabilities_json LIKE ? OR models_map_json LIKE ?
		ORDER BY id
	`, like, like, like, like)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []ProxyRecord
	for rows.Next() {
		var r ProxyRecord
		if err := scanRecord(rows, &r); err != nil {
			continue
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

// First 返回第一条记录
// Deprecated: 使用 FirstRecord 替代。
func (d *DB) First() (*ProxyRecord, error) {
	return d.FirstRecord()
}

// FirstRecord 返回 ID 最小的记录
func (d *DB) FirstRecord() (*ProxyRecord, error) {
	rows, err := d.db.Query(`
		SELECT id, name, url, key, provider_type, detected_format, openai_cap, anthropic_cap, model_count, models_json, capabilities_json, models_map_json, weight, created_at
		FROM proxies ORDER BY id LIMIT 1
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var r ProxyRecord
	if !rows.Next() {
		return nil, nil
	}
	if err := scanRecord(rows, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}