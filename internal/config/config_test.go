package config

import (
	"strings"
	"testing"
)

const (
	validKey    = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=" // 32 bytes
	validSecret = "0123456789abcdef0123456789abcdef0123456789"
)

func baseEnv() map[string]string {
	return map[string]string{
		"SKEIN_DATABASE_URL": "postgres://localhost/skein",
		"SKEIN_MASTER_KEY":   validKey,
		"SKEIN_JWT_SECRET":   validSecret,
	}
}

func loadWith(t *testing.T, overrides map[string]string) (*Config, error) {
	t.Helper()
	envs := baseEnv()
	for k, v := range overrides {
		if v == "" {
			delete(envs, k)
			t.Setenv(k, "")
			continue
		}
		envs[k] = v
	}
	for k, v := range envs {
		t.Setenv(k, v)
	}
	return Load()
}

func TestLoadValid(t *testing.T) {
	cfg, err := loadWith(t, nil)
	if err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}
	if cfg.Addr != ":8080" {
		t.Errorf("Addr = %q, want :8080", cfg.Addr)
	}
	if cfg.ShardSizeBytes != 256<<20 {
		t.Errorf("ShardSizeBytes = %d, want %d", cfg.ShardSizeBytes, 256<<20)
	}
	if !cfg.EncryptionEnabled {
		t.Error("EncryptionEnabled = false, want true by default")
	}
	key, err := cfg.MasterKey()
	if err != nil {
		t.Fatalf("MasterKey() = %v", err)
	}
	if len(key) != MasterKeyLen {
		t.Errorf("len(MasterKey()) = %d, want %d", len(key), MasterKeyLen)
	}
}

func TestLoadRejectsBadConfig(t *testing.T) {
	tests := []struct {
		name      string
		overrides map[string]string
		wantIn    string
	}{
		{
			name:      "short master key",
			overrides: map[string]string{"SKEIN_MASTER_KEY": "c2hvcnQ="},
			wantIn:    "must decode to 32 bytes",
		},
		{
			name:      "master key not base64",
			overrides: map[string]string{"SKEIN_MASTER_KEY": "not!base64!"},
			wantIn:    "not valid base64",
		},
		{
			name:      "short jwt secret",
			overrides: map[string]string{"SKEIN_JWT_SECRET": "tooshort"},
			wantIn:    "at least 32 characters",
		},
		{
			name:      "unknown env",
			overrides: map[string]string{"SKEIN_ENV": "staging"},
			wantIn:    "SKEIN_ENV must be",
		},
		{
			name:      "unknown log level",
			overrides: map[string]string{"SKEIN_LOG_LEVEL": "trace"},
			wantIn:    "SKEIN_LOG_LEVEL must be",
		},
		{
			name:      "bad trusted proxy cidr",
			overrides: map[string]string{"SKEIN_TRUSTED_PROXIES": "10.0.0.1"},
			wantIn:    "SKEIN_TRUSTED_PROXIES",
		},
		{
			name:      "access ttl too long",
			overrides: map[string]string{"SKEIN_ACCESS_TOKEN_TTL": "4h"},
			wantIn:    "SKEIN_ACCESS_TOKEN_TTL",
		},
		{
			name: "refresh ttl below access ttl",
			overrides: map[string]string{
				"SKEIN_ACCESS_TOKEN_TTL":  "15m",
				"SKEIN_REFRESH_TOKEN_TTL": "5m",
			},
			wantIn: "SKEIN_REFRESH_TOKEN_TTL",
		},
		{
			name:      "shard size below 1MiB",
			overrides: map[string]string{"SKEIN_SHARD_SIZE_BYTES": "1024"},
			wantIn:    "SKEIN_SHARD_SIZE_BYTES",
		},
		{
			name: "plaintext public url in production",
			overrides: map[string]string{
				"SKEIN_ENV":        "production",
				"SKEIN_PUBLIC_URL": "http://skein.example",
			},
			wantIn: "must be https in production",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadWith(t, tc.overrides)
			if err == nil {
				t.Fatalf("Load() = nil, want error containing %q", tc.wantIn)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("Load() = %v, want error containing %q", err, tc.wantIn)
			}
		})
	}
}

func TestTrustedProxyPrefixes(t *testing.T) {
	cfg, err := loadWith(t, map[string]string{
		"SKEIN_TRUSTED_PROXIES": "10.0.0.0/8, 172.16.0.0/12,::1/128",
	})
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	got, err := cfg.TrustedProxyPrefixes()
	if err != nil {
		t.Fatalf("TrustedProxyPrefixes() = %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3: %v", len(got), got)
	}
}

func TestGoogleConfigured(t *testing.T) {
	cfg, err := loadWith(t, nil)
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if cfg.GoogleConfigured() {
		t.Error("GoogleConfigured() = true with no credentials set")
	}

	cfg, err = loadWith(t, map[string]string{
		"SKEIN_GOOGLE_CLIENT_ID":     "id",
		"SKEIN_GOOGLE_CLIENT_SECRET": "secret",
		"SKEIN_GOOGLE_REDIRECT_URL":  "http://localhost:8080/cb",
	})
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if !cfg.GoogleConfigured() {
		t.Error("GoogleConfigured() = false with full credentials")
	}
}
