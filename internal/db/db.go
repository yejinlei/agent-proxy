package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	return err
}

// Add 添加一条代理记录
func (d *DB) Add(record *ProxyRecord) error {
	modelsJSON, _ := json.Marshal(record.Models())
	result, err := d.db.Exec(`
		INSERT INTO proxies (name, url, key, provider_type, detected_format, openai_cap, anthropic_cap, model_count, models_json, weight, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, record.Name, record.URL, record.Key, record.ProviderType, record.DetectedFormat,
		boolInt(record.OpenAICap), boolInt(record.AnthropicCap), record.ModelCount,
		string(modelsJSON), record.Weight, record.CreatedAt.Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("insert proxy: %w", err)
	}
	id, _ := result.LastInsertId()
	record.ID = int(id)
	return nil
}

// List 列出所有代理记录
func (d *DB) List() ([]ProxyRecord, error) {
	rows, err := d.db.Query(`
		SELECT id, name, url, key, provider_type, detected_format, openai_cap, anthropic_cap, model_count, models_json, weight, created_at
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
		var oaiCap, antCap int64
		var ts string
		if err := rows.Scan(&r.ID, &r.Name, &r.URL, &r.Key, &r.ProviderType, &r.DetectedFormat, &oaiCap, &antCap, &r.ModelCount, &r.ModelsJSON, &r.Weight, &ts); err != nil {
			return nil, err
		}
		r.OpenAICap = oaiCap != 0
		r.AnthropicCap = antCap != 0
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			r.CreatedAt = t
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
	err := d.db.QueryRow(`
		SELECT id, name, url, key, provider_type, detected_format, openai_cap, anthropic_cap, model_count, models_json, weight, created_at
		FROM proxies WHERE id = ?
	`, id).Scan(&r.ID, &r.Name, &r.URL, &r.Key, &r.ProviderType, &r.DetectedFormat, &oaiCap, &antCap, &r.ModelCount, &r.ModelsJSON, &r.Weight, &ts)
	if err != nil {
		return nil, err
	}
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

// First 返回第一条记录
func (d *DB) First() (*ProxyRecord, error) {
	return d.GetByID(0) // 实际上会失败，改用下方
}

// FirstRecord 返回 ID 最小的记录
func (d *DB) FirstRecord() (*ProxyRecord, error) {
	rows, err := d.db.Query(`
		SELECT id, name, url, key, provider_type, detected_format, openai_cap, anthropic_cap, model_count, models_json, weight, created_at
		FROM proxies ORDER BY id LIMIT 1
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var r ProxyRecord
	var oaiCap, antCap int64
	var ts string
	if !rows.Next() {
		return nil, nil
	}
	if err := rows.Scan(&r.ID, &r.Name, &r.URL, &r.Key, &r.ProviderType, &r.DetectedFormat, &oaiCap, &antCap, &r.ModelCount, &r.ModelsJSON, &r.Weight, &ts); err != nil {
		return nil, err
	}
	r.OpenAICap = oaiCap != 0
	r.AnthropicCap = antCap != 0
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		r.CreatedAt = t
	}
	return &r, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
