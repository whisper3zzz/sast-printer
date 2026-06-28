package api

import (
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	ipMaxQPS         = 10
	ipWindowDuration = time.Second
	ipBlockDuration  = 10 * time.Second
)

type ipRateState struct {
	windowStart  time.Time
	count        int
	blockedUntil time.Time
}

var ipRateLimiter = struct {
	sync.Mutex
	items map[string]*ipRateState
}{
	items: map[string]*ipRateState{},
}

// IPRateLimit limits each IP to ipMaxQPS requests per ipWindowDuration.
func IPRateLimit() gin.HandlerFunc {
	return func(c *gin.Context) {
		now := time.Now()
		ip := c.ClientIP()

		blocked, shouldBlockNow := checkAndUpdateIPRate(ip, now)
		if blocked || shouldBlockNow {
			c.Header("Retry-After", strconv.Itoa(int(ipBlockDuration.Seconds())))
			if shouldBlockNow {
				log.Printf("[ratelimit] block ip=%s reason=qps_exceeded threshold=%d window=%s", ip, ipMaxQPS, ipWindowDuration)
			} else {
				log.Printf("[ratelimit] reject blocked ip=%s", ip)
			}
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error": "too many requests",
			})
			c.Abort()
			return
		}

		c.Next()
	}
}

func checkAndUpdateIPRate(ip string, now time.Time) (alreadyBlocked bool, newlyBlocked bool) {
	ipRateLimiter.Lock()
	defer ipRateLimiter.Unlock()

	state, ok := ipRateLimiter.items[ip]
	if !ok {
		ipRateLimiter.items[ip] = &ipRateState{windowStart: now, count: 1}
		return false, false
	}

	if now.Before(state.blockedUntil) {
		return true, false
	}

	if now.Sub(state.windowStart) >= ipWindowDuration {
		state.windowStart = now
		state.count = 0
	}

	state.count++
	if state.count > ipMaxQPS {
		state.blockedUntil = now.Add(ipBlockDuration)
		return false, true
	}

	return false, false
}
