package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"goprint/config"

	"github.com/gin-gonic/gin"
)

func TestSessionAuthRequiredSetsAuthTokenFromSession(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cfg := testSessionAuthConfig(false)
	prevCfg := getConfig()
	SetConfig(cfg)
	defer SetConfig(prevCfg)

	token := "test-user-access-token"
	user := feishuUserInfo{OpenID: "ou_session_user", UserID: "u_session_user", Name: "Session User"}
	cacheFeishuTokenForTest(token, user)
	defer clearFeishuTokenCacheForTest(token)

	router := gin.New()
	router.Use(SetupSessionMiddleware(cfg))
	router.POST("/login", func(c *gin.Context) {
		if err := SetSession(c, user, token); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.Status(http.StatusNoContent)
	})
	router.GET("/protected", SessionAuthRequired(), func(c *gin.Context) {
		rawToken, tokenPresent := c.Get("auth_token")
		authUser, _ := currentAuthUser(c)
		c.JSON(http.StatusOK, gin.H{
			"token_present": tokenPresent,
			"token":         rawToken,
			"open_id":       authUser.OpenID,
		})
	})

	loginReq := httptest.NewRequest(http.MethodPost, "/login", nil)
	loginResp := httptest.NewRecorder()
	router.ServeHTTP(loginResp, loginReq)
	if loginResp.Code != http.StatusNoContent {
		t.Fatalf("login status = %d, want %d body=%s", loginResp.Code, http.StatusNoContent, loginResp.Body.String())
	}
	cookies := loginResp.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("login did not set a session cookie")
	}

	protectedReq := httptest.NewRequest(http.MethodGet, "/protected", nil)
	for _, cookie := range cookies {
		protectedReq.AddCookie(cookie)
	}
	protectedResp := httptest.NewRecorder()
	router.ServeHTTP(protectedResp, protectedReq)
	if protectedResp.Code != http.StatusOK {
		t.Fatalf("protected status = %d, want %d body=%s", protectedResp.Code, http.StatusOK, protectedResp.Body.String())
	}

	var body struct {
		TokenPresent bool   `json:"token_present"`
		Token        string `json:"token"`
		OpenID       string `json:"open_id"`
	}
	if err := json.Unmarshal(protectedResp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode protected response: %v", err)
	}
	if !body.TokenPresent {
		t.Fatal("auth_token was not set in gin context")
	}
	if body.Token != token {
		t.Fatalf("auth_token = %q, want %q", body.Token, token)
	}
	if body.OpenID != user.OpenID {
		t.Fatalf("open_id = %q, want %q", body.OpenID, user.OpenID)
	}
}

func TestSessionMiddlewareHonorsSecureCookieConfig(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cfg := testSessionAuthConfig(true)
	router := gin.New()
	router.Use(SetupSessionMiddleware(cfg))
	router.POST("/login", func(c *gin.Context) {
		if err := SetSession(c, feishuUserInfo{OpenID: "ou_secure"}, "secure-token"); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	if resp.Code != http.StatusNoContent {
		t.Fatalf("login status = %d, want %d body=%s", resp.Code, http.StatusNoContent, resp.Body.String())
	}

	setCookies := resp.Header().Values("Set-Cookie")
	if len(setCookies) == 0 {
		t.Fatal("login did not set a session cookie")
	}
	joinedCookies := strings.Join(setCookies, "\n")
	if !strings.Contains(joinedCookies, "Secure") {
		t.Fatalf("Set-Cookie header %q does not include Secure", joinedCookies)
	}
}

func testSessionAuthConfig(secure bool) *config.Config {
	secureCookie := secure
	return &config.Config{
		Auth: config.AuthConfig{
			Enabled: true,
			Session: config.SessionConfig{
				Secret:        "0123456789abcdef0123456789abcdef",
				Secure:        &secureCookie,
				MaxAgeSeconds: 3600,
			},
			Feishu: config.FeishuAuthConfig{
				AppID:         "cli_test",
				AppSecret:     "app_secret_test",
				TokenCacheTTL: "2m",
			},
		},
	}
}

func cacheFeishuTokenForTest(token string, user feishuUserInfo) {
	feishuTokenCache.Lock()
	defer feishuTokenCache.Unlock()
	feishuTokenCache.items[token] = cachedFeishuToken{
		User:      user,
		ExpiresAt: time.Now().Add(time.Hour),
	}
}

func clearFeishuTokenCacheForTest(token string) {
	feishuTokenCache.Lock()
	defer feishuTokenCache.Unlock()
	delete(feishuTokenCache.items, token)
}
