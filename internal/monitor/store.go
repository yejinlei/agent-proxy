package monitor

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

// Store 监控数据存储
type Store struct {
	// 请求日志（环形缓冲区）
	logs    *ringBuffer
	logSize int

	// 聚合指标（按 provider + model + 秒）
	metrics       map[string][]*schema.ProviderMetrics
	metricsLock   sync.RWMutex
	currentSecond int64

	// 全局统计（原子计数器）
	totalRequests atomic.Int64
	totalErrors   atomic.Int64
	activeConns   atomic.Int64

	// 状态
	closed atomic.Bool
}

// ringBuffer 简单环形缓冲区
type ringBuffer struct {
	data  [10000]schema.RequestRecord // 固定大小
	size  int
	index int
	count int
	lock  sync.RWMutex
}

func NewStore(logSize int) *Store {
	store := &Store{
		logSize: logSize,
		logs:    &ringBuffer{size: logSize},
		metrics: make(map[string][]*schema.ProviderMetrics),
	}

	// 初始化 currentSecond
	store.currentSecond = time.Now().Unix()

	return store
}

// Record 记录一次请求
func (s *Store) Record(record schema.RequestRecord) {
	s.totalRequests.Add(1)
	if record.StatusCode >= 400 {
		s.totalErrors.Add(1)
	}

	// 写入环形缓冲区
	s.logs.lock.Lock()
	if s.logs.count >= s.logSize {
		s.logs.data[s.logs.index] = record
		s.logs.index = (s.logs.index + 1) % s.logSize
	} else {
		s.logs.data[s.logs.count] = record
		s.logs.count++
		s.logs.index = s.logs.count
	}
	s.logs.lock.Unlock()

	// 聚合指标
	s.aggregateMetrics(record)
}

func (s *Store) aggregateMetrics(record schema.RequestRecord) {
	now := time.Now().Unix()
	key := record.Provider + "|" + record.Model + "|" + string(now)

	s.metricsLock.Lock()
	defer s.metricsLock.Unlock()

	metrics, ok := s.metrics[key]
	if !ok {
		metrics = []*schema.ProviderMetrics{
			{
				Second:   now,
				Provider: record.Provider,
				Model:    record.Model,
			},
		}
		s.metrics[key] = metrics
	}

	m := metrics[len(metrics)-1]
	m.RequestCount++
	m.LatencySum += float64(record.LatencyMs)

	if record.StatusCode >= 400 {
		m.ErrorCount++
	} else {
		m.SuccessCount++
	}
}

// GetLogs 获取请求日志
func (s *Store) GetLogs(offset, limit int) []schema.RequestRecord {
	s.logs.lock.RLock()
	defer s.logs.lock.RUnlock()

	result := make([]schema.RequestRecord, 0, limit)
	start := (s.logs.index - s.logs.count + offset) % s.logSize
	for i := 0; i < limit && i < s.logs.count; i++ {
		idx := (start + i) % s.logSize
		result = append(result, s.logs.data[idx])
	}

	return result
}

// GetMetrics 获取聚合指标
func (s *Store) GetMetrics(provider, model string) []*schema.ProviderMetrics {
	s.metricsLock.RLock()
	defer s.metricsLock.RUnlock()

	for key, metrics := range s.metrics {
		// 简单 key 格式匹配
		if provider != "" && !contains(key, provider) {
			continue
		}
		if model != "" && !contains(key, model) {
			continue
		}
		return metrics
	}

	return nil
}

// GetSummary 获取全局摘要
func (s *Store) GetSummary() map[string]interface{} {
	return map[string]interface{}{
		"total_requests": int64(s.totalRequests.Load()),
		"total_errors":   int64(s.totalErrors.Load()),
		"active_conns":   int64(s.activeConns.Load()),
		"log_count":      s.logs.count,
		"timestamp":      time.Now().Unix(),
	}
}

// IncrActiveConns 增加活跃连接数
func (s *Store) IncrActiveConns() {
	s.activeConns.Add(1)
}

// DecrActiveConns 减少活跃连接数
func (s *Store) DecrActiveConns() {
	s.activeConns.Add(-1)
}

// GetProviderStatus 获取所有 provider 状态
func (s *Store) GetProviderStatus() map[string]ProviderStatus {
	s.metricsLock.RLock()
	defer s.metricsLock.RUnlock()

	status := make(map[string]*ProviderStatus)

	for _, metrics := range s.metrics {
		if len(metrics) == 0 {
			continue
		}
		m := metrics[len(metrics)-1]
		if status[m.Provider] == nil {
			status[m.Provider] = &ProviderStatus{
				Name:         m.Provider,
				RequestCount: 0,
				ErrorCount:   0,
				SuccessCount: 0,
				LatencySum:   0,
				LatencyCount: 0,
			}
		}
		p := status[m.Provider]
		p.RequestCount += m.RequestCount
		p.ErrorCount += m.ErrorCount
		p.SuccessCount += m.SuccessCount
		p.LatencySum += m.LatencySum
		p.LatencyCount += m.RequestCount
	}

	// 计算平均延迟和状态
	for _, p := range status {
		if p.LatencyCount > 0 {
			p.AvgLatency = p.LatencySum / float64(p.LatencyCount)
		}
		if float64(p.ErrorCount) > float64(p.RequestCount)*0.1 {
			p.Status = "degraded"
		} else if p.RequestCount > 0 {
			p.Status = "healthy"
		} else {
			p.Status = "idle"
		}
	}

	returnStatus := make(map[string]ProviderStatus)
	for name, p := range status {
		returnStatus[name] = *p
	}
	return returnStatus
}

type ProviderStatus struct {
	Name         string  `json:"name"`
	Status       string  `json:"status"`
	RequestCount int     `json:"request_count"`
	ErrorCount   int     `json:"error_count"`
	SuccessCount int     `json:"success_count"`
	AvgLatency   float64 `json:"avg_latency_ms"`
	LatencySum   float64 `json:"-"`
	LatencyCount int     `json:"-"`
}

func contains(s, substr string) bool {
	return len(s) > 0 && (s == substr || len(s) > len(substr))
}

// MarshalJSON 为 SSE 推送格式
func (s *Store) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]interface{}{
		"summary":   s.GetSummary(),
		"providers": s.GetProviderStatus(),
	})
}
