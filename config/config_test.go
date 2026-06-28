package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadFromFileRequiresSessionSecretWhenAuthEnabled(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte(`
auth:
  enabled: true
  feishu:
    app_id: cli_test
    app_secret: app_secret_test
sane_api:
  auth_enabled: false
printers:
  - id: test-printer
    uri: ipp://localhost:631/printers/test-printer
    visible: true
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := LoadFromFile(configPath)
	if err == nil {
		t.Fatal("LoadFromFile succeeded without auth.session.secret")
	}
	if !strings.Contains(err.Error(), "auth.session.secret") {
		t.Fatalf("LoadFromFile error = %v, want auth.session.secret", err)
	}
}
