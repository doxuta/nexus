package middleware

// #354 (specs/045 §5): AuthRateLimit checks-and-records BOTH windows (per-(ip,tenant,email) and
// per-IP) atomically. A peek()+record() pair lets every goroutine that peeked before the first
// record through, so a parallel burst overshoots the cap by 1-2.

import (
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// TestAuthRateLimit_ConcurrentStormAdmitsExactlyMax_BothWindows runs the shared storm helper
// with each window as the binding one.
func TestAuthRateLimit_ConcurrentStormAdmitsExactlyMax_BothWindows(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const max = 5
	t.Run("per-(ip,tenant,email) window binds", func(t *testing.T) {
		assertStormAdmitsExactly(t, max,
			func() *gin.Engine {
				return newRateLimitRouter(RateLimitConfig{MaxAttempts: max, IPMaxAttempts: 1000, Window: time.Minute})
			},
			func(r *gin.Engine) int {
				return doRequest(r, "203.0.113.50:1", `{"tenant":"acme","email":"same@x.com"}`).Code
			})
	})
	t.Run("per-IP window binds", func(t *testing.T) {
		var n atomic.Int64
		assertStormAdmitsExactly(t, max,
			func() *gin.Engine {
				return newRateLimitRouter(RateLimitConfig{MaxAttempts: 1000, IPMaxAttempts: max, Window: time.Minute})
			},
			func(r *gin.Engine) int {
				body := fmt.Sprintf(`{"tenant":"acme","email":"u%d@x.com"}`, n.Add(1))
				return doRequest(r, "203.0.113.51:1", body).Code
			})
	})
}

// TestAuthRateLimit_ConcurrentRefusalNeverChargesOtherWindow: a flood of one email (refused by
// the per-email window) must not spend per-IP budget beyond what it was ADMITTED for.
func TestAuthRateLimit_ConcurrentRefusalNeverChargesOtherWindow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for round := 0; round < 20; round++ {
		// per-email cap 3, per-IP cap 10: the flood admits exactly 3 (spends 3 IP slots), leaving 7.
		r := newRateLimitRouter(RateLimitConfig{MaxAttempts: 3, IPMaxAttempts: 10, Window: time.Minute})
		var wg sync.WaitGroup
		for i := 0; i < 40; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				doRequest(r, "203.0.113.52:1", `{"tenant":"acme","email":"victim@x.com"}`)
			}()
		}
		wg.Wait()
		for i := 0; i < 7; i++ {
			body := fmt.Sprintf(`{"tenant":"acme","email":"o%d@x.com"}`, i)
			if w := doRequest(r, "203.0.113.52:1", body); w.Code != http.StatusOK {
				t.Fatalf("round %d: other email %d refused (%d): a refused flood charged the per-IP window", round, i, w.Code)
			}
		}
		if w := doRequest(r, "203.0.113.52:1", `{"tenant":"acme","email":"o99@x.com"}`); w.Code != http.StatusTooManyRequests {
			t.Fatalf("round %d: IP window should be spent at exactly 10, got %d", round, w.Code)
		}
	}
}
