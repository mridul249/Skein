// Package config loads and validates process configuration from the
// environment. Loading fails fast: an invalid configuration is a startup
// error, never a runtime surprise.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"

	skcrypto "github.com/mridul60214/skein/internal/crypto"
)

// MasterKeyLen is the required length, in bytes, of the decoded master key.
const MasterKeyLen = 32

// Config is the complete runtime configuration of a Skein process.
type Config struct {
	Env      string `env:"SKEIN_ENV"       envDefault:"development"`
	Addr     string `env:"SKEIN_ADDR"      envDefault:":8080"`
	LogLevel string `env:"SKEIN_LOG_LEVEL" envDefault:"info"`
	LogJSON  bool   `env:"SKEIN_LOG_JSON"  envDefault:"false"`

	DatabaseURL string `env:"SKEIN_DATABASE_URL,required"`

	// MasterKeyB64 is the base64 (standard encoding) of a 32-byte key. All
	// other keys in the system derive from it via HKDF-SHA256.
	MasterKeyB64 string `env:"SKEIN_MASTER_KEY,required"`

	// JWTSecret signs access tokens. Distinct from the master key so that
	// rotating one does not invalidate the other.
	JWTSecret string `env:"SKEIN_JWT_SECRET,required"`

	AccessTokenTTL  time.Duration `env:"SKEIN_ACCESS_TOKEN_TTL"  envDefault:"15m"`
	RefreshTokenTTL time.Duration `env:"SKEIN_REFRESH_TOKEN_TTL" envDefault:"720h"`

	GoogleClientID     string `env:"SKEIN_GOOGLE_CLIENT_ID"`
	GoogleClientSecret string `env:"SKEIN_GOOGLE_CLIENT_SECRET"`
	GoogleRedirectURL  string `env:"SKEIN_GOOGLE_REDIRECT_URL"`

	// PublicURL is the externally reachable base URL. Used to build share
	// links and to decide whether cookies may be marked Secure.
	PublicURL string `env:"SKEIN_PUBLIC_URL" envDefault:"http://localhost:8080"`

	// PreviewOrigin, when set, is the separate origin that inline previews
	// are served from. See Architecture.md §10.
	PreviewOrigin string `env:"SKEIN_PREVIEW_ORIGIN"`

	// TrustedProxies is a CIDR allowlist. X-Forwarded-For is honoured only
	// when the direct peer falls inside one of these. Empty means never.
	TrustedProxies []string `env:"SKEIN_TRUSTED_PROXIES" envSeparator:","`

	CORSOrigins []string `env:"SKEIN_CORS_ORIGINS" envSeparator:","`

	MaxUploadBytes    int64 `env:"SKEIN_MAX_UPLOAD_BYTES"    envDefault:"107374182400"`
	MaxUploadsPerUser int   `env:"SKEIN_MAX_UPLOADS_PER_USER" envDefault:"10"`

	ShutdownTimeout time.Duration `env:"SKEIN_SHUTDOWN_TIMEOUT" envDefault:"30s"`
	QuotaSyncEvery  time.Duration `env:"SKEIN_QUOTA_SYNC_EVERY"  envDefault:"5m"`
	ReclaimEvery    time.Duration `env:"SKEIN_RECLAIM_EVERY"     envDefault:"60s"`

	// ShardSizeBytes is the target size of a single shard. The tail shard of
	// a striped file is short.
	ShardSizeBytes int64 `env:"SKEIN_SHARD_SIZE_BYTES" envDefault:"268435456"`

	// RoutingPolicy chooses which drive a shard lands on:
	// most-available, priority or round-robin.
	RoutingPolicy string `env:"SKEIN_ROUTING_POLICY" envDefault:"most-available"`

	// EncryptionEnabled exists so the encrypted-at-rest guarantee can be
	// disabled in a local development loop. It defaults to on and the
	// startup log warns loudly when it is not.
	EncryptionEnabled bool `env:"SKEIN_ENCRYPTION_ENABLED" envDefault:"true"`
}

// Load reads the configuration from the environment and validates it.
func Load() (*Config, error) {
	var c Config
	if err := env.Parse(&c); err != nil {
		return nil, fmt.Errorf("parse environment: %w", err)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// IsProduction reports whether the process is configured for production.
func (c *Config) IsProduction() bool { return c.Env == "production" }

// MasterKey decodes and returns the 32-byte master key.
func (c *Config) MasterKey() ([]byte, error) {
	k, err := base64.StdEncoding.DecodeString(c.MasterKeyB64)
	if err != nil {
		return nil, fmt.Errorf("SKEIN_MASTER_KEY is not valid base64: %w", err)
	}
	if len(k) != MasterKeyLen {
		return nil, fmt.Errorf("SKEIN_MASTER_KEY must decode to %d bytes, got %d", MasterKeyLen, len(k))
	}
	return k, nil
}

// TrustedProxyPrefixes parses TrustedProxies into netip prefixes.
func (c *Config) TrustedProxyPrefixes() ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(c.TrustedProxies))
	for _, raw := range c.TrustedProxies {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		p, err := netip.ParsePrefix(raw)
		if err != nil {
			return nil, fmt.Errorf("SKEIN_TRUSTED_PROXIES entry %q: %w", raw, err)
		}
		out = append(out, p)
	}
	return out, nil
}

// GoogleConfigured reports whether OAuth credentials are present. Drive
// endpoints return a clear error rather than a 500 when they are not.
func (c *Config) GoogleConfigured() bool {
	return c.GoogleClientID != "" && c.GoogleClientSecret != "" && c.GoogleRedirectURL != ""
}

func (c *Config) validate() error {
	var errs []error

	switch c.Env {
	case "development", "production", "test":
	default:
		errs = append(errs, fmt.Errorf("SKEIN_ENV must be development, test or production, got %q", c.Env))
	}

	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("SKEIN_LOG_LEVEL must be debug, info, warn or error, got %q", c.LogLevel))
	}

	if _, err := c.MasterKey(); err != nil {
		errs = append(errs, err)
	}

	if len(c.JWTSecret) < 32 {
		errs = append(errs, errors.New("SKEIN_JWT_SECRET must be at least 32 characters"))
	}

	if _, err := c.TrustedProxyPrefixes(); err != nil {
		errs = append(errs, err)
	}

	if c.AccessTokenTTL <= 0 || c.AccessTokenTTL > time.Hour {
		errs = append(errs, errors.New("SKEIN_ACCESS_TOKEN_TTL must be > 0 and <= 1h"))
	}
	if c.RefreshTokenTTL <= c.AccessTokenTTL {
		errs = append(errs, errors.New("SKEIN_REFRESH_TOKEN_TTL must exceed SKEIN_ACCESS_TOKEN_TTL"))
	}

	if c.MaxUploadBytes <= 0 {
		errs = append(errs, errors.New("SKEIN_MAX_UPLOAD_BYTES must be positive"))
	}
	if c.MaxUploadsPerUser <= 0 {
		errs = append(errs, errors.New("SKEIN_MAX_UPLOADS_PER_USER must be positive"))
	}
	if c.ShardSizeBytes < 1<<20 {
		errs = append(errs, errors.New("SKEIN_SHARD_SIZE_BYTES must be at least 1 MiB"))
	}
	// A shard is encrypted as a whole number of AEAD frames. The format
	// tolerates a short final frame, so an odd shard size would still work —
	// but every non-tail shard would then carry one runt frame, which wastes
	// a tag per shard and makes the ciphertext layout non-uniform for
	// anybody reasoning about offsets later. Requiring the multiple keeps
	// full shards identical in size and costs nothing.
	if c.ShardSizeBytes%skcrypto.FrameSize != 0 {
		errs = append(errs, fmt.Errorf(
			"SKEIN_SHARD_SIZE_BYTES must be a multiple of the %d-byte AEAD frame size; "+
				"%d leaves a remainder of %d",
			skcrypto.FrameSize, c.ShardSizeBytes, c.ShardSizeBytes%skcrypto.FrameSize))
	}

	switch c.RoutingPolicy {
	case "most-available", "priority", "round-robin":
	default:
		errs = append(errs, fmt.Errorf(
			"SKEIN_ROUTING_POLICY must be most-available, priority or round-robin, got %q",
			c.RoutingPolicy))
	}
	if c.ShutdownTimeout <= 0 {
		errs = append(errs, errors.New("SKEIN_SHUTDOWN_TIMEOUT must be positive"))
	}

	// A production deployment behind a proxy that does not declare its
	// proxies writes RemoteAddr into audit records. That is correct but
	// usually not what the operator meant, so it is a hard error only when
	// combined with a plaintext public URL.
	if c.IsProduction() && strings.HasPrefix(c.PublicURL, "http://") {
		errs = append(errs, errors.New("SKEIN_PUBLIC_URL must be https in production; refresh cookies require Secure"))
	}

	return errors.Join(errs...)
}
