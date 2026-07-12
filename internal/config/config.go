package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const Version = "0.4.0"

type Config struct {
	Host                   string
	Port                   int
	LogLevel               string
	ChatProxyBaseURL       string
	ChatProxyVersion       string
	AuthsDir               string
	AuthsReloadInterval    time.Duration
	AuthRefreshConcurrency int
	AccountMaxInflight     int
	ModelsRefreshInterval  time.Duration
	RetryMaxAttempts       int
	RetryBaseDelay         time.Duration
	RateLimitCooldown      time.Duration
	QuotaCooldown          time.Duration
	AffinityTTL            time.Duration
	AffinityMaxEntries     int
	ClientName             string
	ClientVersion          string
	ClientSurface          string
	ClientIdentifier       string
	TokenAuth              string
	TLSInsecureSkipVerify  bool
	StreamCompression      string
	ProxyURL               string
	NoProxy                []string
	APIKeys                []string
	AdminKey               string
}

func Load() (Config, error) {
	if err := loadDotEnv(".env"); err != nil && !os.IsNotExist(err) {
		return Config{}, fmt.Errorf("load .env: %w", err)
	}
	port, err := envInt("GROK2API_PORT", 8088)
	if err != nil {
		return Config{}, err
	}
	refreshConcurrency, err := envPositiveInt("GROK_AUTH_REFRESH_CONCURRENCY", 4)
	if err != nil {
		return Config{}, err
	}
	accountMaxInflight, err := envPositiveInt("GROK_ACCOUNT_MAX_INFLIGHT", 16)
	if err != nil {
		return Config{}, err
	}
	retryAttempts, err := envPositiveInt("GROK_RETRY_MAX_ATTEMPTS", 3)
	if err != nil {
		return Config{}, err
	}
	affinityMax, err := envPositiveInt("GROK_AFFINITY_MAX_ENTRIES", 100000)
	if err != nil {
		return Config{}, err
	}
	reloadInterval, err := envDuration("GROK_AUTHS_RELOAD_INTERVAL", 30*time.Second)
	if err != nil {
		return Config{}, err
	}
	modelsRefreshInterval, err := envDuration("GROK_MODELS_REFRESH_INTERVAL", 6*time.Hour)
	if err != nil {
		return Config{}, err
	}
	retryBaseDelay, err := envDuration("GROK_RETRY_BASE_DELAY", 200*time.Millisecond)
	if err != nil {
		return Config{}, err
	}
	rateLimitCooldown, err := envDuration("GROK_RATE_LIMIT_COOLDOWN", time.Minute)
	if err != nil {
		return Config{}, err
	}
	quotaCooldown, err := envDuration("GROK_QUOTA_COOLDOWN", 24*time.Hour)
	if err != nil {
		return Config{}, err
	}
	affinityTTL, err := envDuration("GROK_AFFINITY_TTL", time.Hour)
	if err != nil {
		return Config{}, err
	}
	streamCompression := strings.ToLower(strings.TrimSpace(env("GROK_STREAM_COMPRESSION", "identity")))
	if streamCompression != "identity" && streamCompression != "gzip" {
		return Config{}, fmt.Errorf("GROK_STREAM_COMPRESSION must be identity or gzip")
	}
	cfg := Config{
		Host:                   env("GROK2API_HOST", "0.0.0.0"),
		Port:                   port,
		LogLevel:               strings.ToUpper(env("GROK2API_LOG_LEVEL", "INFO")),
		ChatProxyBaseURL:       strings.TrimRight(env("GROK_CHAT_PROXY_BASE_URL", "https://cli-chat-proxy.grok.com"), "/"),
		ChatProxyVersion:       strings.Trim(env("GROK_CHAT_PROXY_VERSION", "v1"), "/"),
		AuthsDir:               expandHome(env("GROK_AUTHS_DIR", "./auths")),
		AuthsReloadInterval:    reloadInterval,
		AuthRefreshConcurrency: refreshConcurrency,
		AccountMaxInflight:     accountMaxInflight,
		ModelsRefreshInterval:  modelsRefreshInterval,
		RetryMaxAttempts:       retryAttempts,
		RetryBaseDelay:         retryBaseDelay,
		RateLimitCooldown:      rateLimitCooldown,
		QuotaCooldown:          quotaCooldown,
		AffinityTTL:            affinityTTL,
		AffinityMaxEntries:     affinityMax,
		ClientName:             env("GROK_CLIENT_NAME", "grok-shell"),
		ClientVersion:          env("GROK_CLIENT_VERSION", "0.2.93"),
		ClientSurface:          env("GROK_CLIENT_SURFACE", "tui"),
		ClientIdentifier:       env("GROK_CLIENT_IDENTIFIER", "grok-shell"),
		TokenAuth:              env("GROK_TOKEN_AUTH", "xai-grok-cli"),
		TLSInsecureSkipVerify:  envBool("GROK_TLS_INSECURE_SKIP_VERIFY", false),
		StreamCompression:      streamCompression,
		ProxyURL:               strings.TrimSpace(os.Getenv("GROK_PROXY_URL")),
		NoProxy:                splitCSV(os.Getenv("GROK_NO_PROXY")),
		AdminKey:               strings.TrimSpace(os.Getenv("GROK_ADMIN_KEY")),
	}
	cfg.APIKeys = unique(append(splitCSV(os.Getenv("GROK_API_KEYS")), splitCSV(os.Getenv("GROK_API_KEY"))...))
	return cfg, nil
}

func envPositiveInt(name string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return n, nil
}

func envDuration(name string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return d, nil
}

func (c Config) Address() string { return c.Host + ":" + strconv.Itoa(c.Port) }

func env(name, fallback string) string {
	if value, ok := os.LookupEnv(name); ok {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("%s must be a port between 1 and 65535", name)
	}
	return n, nil
}

func envBool(name string, fallback bool) bool {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}
	return v
}

func splitCSV(raw string) []string {
	var out []string
	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func unique(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, value := range in {
		if _, ok := seen[value]; !ok {
			seen[value] = struct{}{}
			out = append(out, value)
		}
	}
	return out
}

func expandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimLeft(path[1:], `/\`))
		}
	}
	return path
}

// loadDotEnv supplies unset environment variables from a simple .env file.
// It deliberately does not implement shell expansion: credentials remain literal.
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if len(value) >= 2 && ((value[0] == '\'' && value[len(value)-1] == '\'') || (value[0] == '"' && value[len(value)-1] == '"')) {
			value = value[1 : len(value)-1]
		}
		if _, exists := os.LookupEnv(name); !exists && name != "" {
			_ = os.Setenv(name, value)
		}
	}
	return s.Err()
}
