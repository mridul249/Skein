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
			// 1100000 clears the 1 MiB floor, so only the frame-alignment
			// branch can be what rejects it. 1100000 % 65536 = 51424.
			name:      "shard size not a whole number of AEAD frames",
			overrides: map[string]string{"SKEIN_SHARD_SIZE_BYTES": "1100000"},
			wantIn:    "must be a multiple of the 65536-byte AEAD frame size; 1100000 leaves a remainder of 51424",
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

// The boot check that a shard is a whole number of AEAD frames. It shipped
// unasserted: the "shard size below 1MiB" case above matches on the variable
// name alone, and an undersized value trips the 1 MiB floor as well, so nothing
// proved the alignment branch rejects for its own reason or that a valid size is
// still accepted.
//
// Plaintext basis is the point. A shard's ciphertext is 5 + N*65552, which is
// odd, so a ciphertext-basis check could never be satisfied by any shard size.
func TestShardSizeMustBeAWholeNumberOfAEADFrames(t *testing.T) {
	t.Run("rejects a size that is not a frame multiple", func(t *testing.T) {
		_, err := loadWith(t, map[string]string{"SKEIN_SHARD_SIZE_BYTES": "1100000"})
		if err == nil {
			t.Fatal("Load() = nil, want the frame-alignment error")
		}
		const want = "SKEIN_SHARD_SIZE_BYTES must be a multiple of the 65536-byte AEAD frame size; " +
			"1100000 leaves a remainder of 51424"
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Load() = %v, want an error containing %q", err, want)
		}
		// 1100000 > 1 MiB, so the floor must not be what fired. Without this
		// the case would pass for the wrong reason at any undersized value.
		if strings.Contains(err.Error(), "at least 1 MiB") {
			t.Errorf("Load() rejected 1100000 for being under 1 MiB: %v", err)
		}
	})

	t.Run("accepts an exact frame multiple", func(t *testing.T) {
		// 1048576 / 65536 = 16 frames exactly.
		cfg, err := loadWith(t, map[string]string{"SKEIN_SHARD_SIZE_BYTES": "1048576"})
		if err != nil {
			t.Fatalf("Load() = %v, want a 1 MiB shard size to be accepted", err)
		}
		if cfg.ShardSizeBytes != 1048576 {
			t.Errorf("ShardSizeBytes = %d, want 1048576", cfg.ShardSizeBytes)
		}
	})

	t.Run("accepts the 256 MiB default", func(t *testing.T) {
		cfg, err := loadWith(t, nil)
		if err != nil {
			t.Fatalf("Load() = %v", err)
		}
		if rem := cfg.ShardSizeBytes % (64 * 1024); rem != 0 {
			t.Errorf("the default shard size %d is not frame-aligned (remainder %d)",
				cfg.ShardSizeBytes, rem)
		}
	})
}
