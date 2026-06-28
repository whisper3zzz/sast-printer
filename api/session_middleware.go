package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/gob"
	"fmt"
	"goprint/config"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
)

const (
	sessionName     = "goprint_session"
	sessionKeyUser  = "user"
	sessionKeyToken = "token"
)

var fallbackSessionSecret = randomSessionSecret()

func init() {
	// Register types for gob encoding/decoding in cookie sessions
	gob.Register(feishuUserInfo{})
}

// SetupSessionMiddleware configures session store and returns middleware
func SetupSessionMiddleware(cfg *config.Config) gin.HandlerFunc {
	hashKey, blockKey := sessionCookieKeys(cfg)
	store := cookie.NewStore(hashKey, blockKey)

	store.Options(sessions.Options{
		Path:     "/",
		MaxAge:   sessionMaxAge(cfg),
		HttpOnly: true,
		Secure:   sessionCookieSecure(cfg),
		SameSite: http.SameSiteLaxMode,
	})

	return sessions.Sessions(sessionName, store)
}

func sessionCookieKeys(cfg *config.Config) ([]byte, []byte) {
	secret := fallbackSessionSecret
	if cfg != nil {
		if configured := strings.TrimSpace(cfg.Auth.Session.Secret); configured != "" {
			secret = configured
		}
	}

	hashKey := sha256.Sum256([]byte("goprint session hash key\n" + secret))
	blockKey := sha256.Sum256([]byte("goprint session block key\n" + secret))
	return hashKey[:], blockKey[:]
}

func randomSessionSecret() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Sprintf("failed to generate fallback session secret: %v", err))
	}
	return base64.StdEncoding.EncodeToString(buf)
}

func sessionMaxAge(cfg *config.Config) int {
	if cfg != nil && cfg.Auth.Session.MaxAgeSeconds > 0 {
		return cfg.Auth.Session.MaxAgeSeconds
	}
	return 86400 * 7
}

func sessionCookieSecure(cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	if cfg.Auth.Session.Secure != nil {
		return *cfg.Auth.Session.Secure
	}
	return cfg.Auth.Enabled
}

// SessionAuthRequired validates session-based authentication
func SessionAuthRequired() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		method := c.Request.Method
		path := c.FullPath()
		if path == "" {
			path = c.Request.URL.Path
		}
		clientIP := c.ClientIP()

		cfg, err := requireConfig()
		if err != nil {
			log.Printf("[auth][session] config error method=%s path=%s ip=%s err=%v", method, path, clientIP, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			c.Abort()
			return
		}

		if !cfg.Auth.Enabled {
			log.Printf("[auth][session] skip auth (disabled) method=%s path=%s ip=%s", method, path, clientIP)
			c.Next()
			return
		}

		session := sessions.Default(c)
		userDataRaw := session.Get(sessionKeyUser)

		if userDataRaw == nil {
			log.Printf("[auth][session] reject no session method=%s path=%s ip=%s", method, path, clientIP)
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			c.Abort()
			return
		}

		// userDataRaw is stored as feishuUserInfo
		userData, ok := userDataRaw.(feishuUserInfo)
		if !ok {
			log.Printf("[auth][session] reject invalid session data method=%s path=%s ip=%s", method, path, clientIP)
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid session"})
			c.Abort()
			return
		}

		tokenStr := ""

		// Optional: re-validate token with Feishu if needed
		tokenRaw := session.Get(sessionKeyToken)
		if rawTokenStr, ok := tokenRaw.(string); ok {
			tokenStr = strings.TrimSpace(rawTokenStr)
		}
		if tokenStr != "" {
			// Validate token freshness
			if validatedUser, err := validateFeishuToken(tokenStr, cfg); err == nil {
				// Update session with fresh user data
				userData = validatedUser
			} else {
				log.Printf("[auth][session] token validation failed method=%s path=%s ip=%s err=%v", method, path, clientIP, err)
				// Token expired, clear session
				session.Clear()
				session.Save()
				c.JSON(http.StatusUnauthorized, gin.H{"error": "session expired"})
				c.Abort()
				return
			}
		}

		c.Set("auth_provider", "feishu")
		c.Set("auth_user", userData)
		if tokenStr != "" {
			c.Set("auth_token", tokenStr)
		}
		log.Printf("[auth][session] auth success method=%s path=%s ip=%s open_id=%s cost=%s",
			method, path, clientIP, maskSensitive(userData.OpenID), time.Since(start))
		c.Next()
	}
}

// SetSession stores user info in session after successful OAuth login
func SetSession(c *gin.Context, user feishuUserInfo, token string) error {
	session := sessions.Default(c)
	session.Set(sessionKeyUser, user)
	session.Set(sessionKeyToken, token)

	if err := session.Save(); err != nil {
		log.Printf("[auth][session] save failed ip=%s err=%v", c.ClientIP(), err)
		return fmt.Errorf("failed to save session: %w", err)
	}

	log.Printf("[auth][session] created ip=%s open_id=%s", c.ClientIP(), maskSensitive(user.OpenID))
	return nil
}

// ClearSession destroys the current session
func ClearSession(c *gin.Context) {
	session := sessions.Default(c)
	session.Clear()
	session.Save()
	log.Printf("[auth][session] cleared ip=%s", c.ClientIP())
}

// CheckSession verifies if a valid session exists
func CheckSession(c *gin.Context) {
	cfg, err := requireConfig()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if !cfg.Auth.Enabled {
		c.JSON(http.StatusOK, gin.H{"authenticated": false, "reason": "auth disabled"})
		return
	}

	session := sessions.Default(c)
	userDataRaw := session.Get(sessionKeyUser)

	if userDataRaw == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"authenticated": false})
		return
	}

	userData, ok := userDataRaw.(feishuUserInfo)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"authenticated": false})
		return
	}

	// Optional: validate token if stored
	tokenRaw := session.Get(sessionKeyToken)
	if tokenStr, ok := tokenRaw.(string); ok && tokenStr != "" {
		if _, err := validateFeishuToken(tokenStr, cfg); err != nil {
			// Token invalid, clear session
			session.Clear()
			session.Save()
			c.JSON(http.StatusUnauthorized, gin.H{"authenticated": false, "reason": "token expired"})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"authenticated": true,
		"user": gin.H{
			"open_id":    userData.OpenID,
			"user_id":    userData.UserID,
			"name":       userData.Name,
			"avatar_url": userData.Avatar,
		},
	})
}
