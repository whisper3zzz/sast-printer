package api

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestIPRateLimitRetryAfterMatchesBlockDuration(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetIPRateLimiterForTest()
	t.Cleanup(resetIPRateLimiterForTest)

	router := gin.New()
	router.Use(IPRateLimit())
	router.GET("/limited", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	var resp *httptest.ResponseRecorder
	for i := 0; i <= ipMaxQPS; i++ {
		req := httptest.NewRequest(http.MethodGet, "/limited", nil)
		req.RemoteAddr = "192.0.2.10:12345"
		resp = httptest.NewRecorder()
		router.ServeHTTP(resp, req)
	}

	if resp.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusTooManyRequests)
	}
	want := strconv.Itoa(int(ipBlockDuration.Seconds()))
	if got := resp.Header().Get("Retry-After"); got != want {
		t.Fatalf("Retry-After = %q, want %q", got, want)
	}
}

func resetIPRateLimiterForTest() {
	ipRateLimiter.Lock()
	defer ipRateLimiter.Unlock()
	ipRateLimiter.items = map[string]*ipRateState{}
}
