package media

import (
	"sync"
	"time"
)

// tokenBucket is a byte-rate limiter. Tokens refill continuously at rate per second
// up to burst; a packet is admitted when its cost fits in the current balance.
type tokenBucket struct {
	mu     sync.Mutex
	rate   float64 // tokens per second
	burst  float64
	tokens float64
	last   time.Time
}

func newTokenBucket(rate float64, burst int) *tokenBucket {
	return &tokenBucket{rate: rate, burst: float64(burst), tokens: float64(burst)}
}

// allow reports whether cost tokens are available at now and, if so, spends them.
// A non-positive rate disables limiting.
func (b *tokenBucket) allow(cost int, now time.Time) bool {
	if b.rate <= 0 {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.last.IsZero() {
		if dt := now.Sub(b.last).Seconds(); dt > 0 {
			b.tokens = min(b.burst, b.tokens+dt*b.rate)
		}
	}
	b.last = now
	if float64(cost) > b.tokens {
		return false
	}
	b.tokens -= float64(cost)
	return true
}
