package server

import "time"

// rateLimiter 简单令牌桶限流器
type rateLimiter struct {
	rps    float64
	burst  int
	tokens float64
	last   time.Time
}

func newRateLimiter(rps int, burst int) *rateLimiter {
	return &rateLimiter{
		rps:    float64(rps),
		burst:  burst,
		tokens: float64(burst),
		last:   time.Now(),
	}
}

func (rl *rateLimiter) Allow() bool {
	now := time.Now()
	elapsed := now.Sub(rl.last).Seconds()
	rl.last = now

	rl.tokens += elapsed * rl.rps
	if rl.tokens > float64(rl.burst) {
		rl.tokens = float64(rl.burst)
	}

	if rl.tokens >= 1 {
		rl.tokens--
		return true
	}

	return false
}
