package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const Version = "0.3.0"

type Config struct {
	Host                    string
	Port                    int
	LogLevel                string
	ChatProxyBaseURL        string
	ChatProxyVersion        string
	SessionToken            string
	AuthFile                string
	OAuthClientID           string
	OAuthClientSecret       string
	ClientName              string
	ClientVersion           string
	ClientSurface           string
	ClientIdentifier        string
	TokenAuth               string
	AuthenticateResponseTag string
	TLSInsecureSkipVerify   bool
	ProxyURL                string
	NoProxy                 []string
	APIKeys                 []string
}

func Load() (Config, error) {
	if err := loadDotEnv(".env"); err != nil && !os.IsNotExist(err) {
		return Config{}, fmt.Errorf("load .env: %w", err)
	}
	port, err := envInt("GROK2API_PORT", 8088)
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		Host:                    env("GROK2API_HOST", "0.0.0.0"),
		Port:                    port,
		LogLevel:                strings.ToUpper(env("GROK2API_LOG_LEVEL", "INFO")),
		ChatProxyBaseURL:        strings.TrimRight(env("GROK_CHAT_PROXY_BASE_URL", "https://cli-chat-proxy.grok.com"), "/"),
		ChatProxyVersion:        strings.Trim(env("GROK_CHAT_PROXY_VERSION", "v1"), "/"),
		SessionToken:            os.Getenv("GROK_SESSION_TOKEN"),
		AuthFile:                expandHome(os.Getenv("GROK_AUTH_FILE")),
		OAuthClientID:           os.Getenv("GROK_OAUTH_CLIENT_ID"),
		OAuthClientSecret:       os.Getenv("GROK_OAUTH_CLIENT_SECRET"),
		ClientName:              env("GROK_CLIENT_NAME", "grok-shell"),
		ClientVersion:           env("GROK_CLIENT_VERSION", "0.2.93"),
		ClientSurface:           env("GROK_CLIENT_SURFACE", "tui"),
		ClientIdentifier:        env("GROK_CLIENT_IDENTIFIER", "grok-shell"),
		TokenAuth:               env("GROK_TOKEN_AUTH", "xai-grok-cli"),
		AuthenticateResponseTag: env("GROK_AUTHENTICATE_RESPONSE_TAG", "authenticate-response"),
		TLSInsecureSkipVerify:   envBool("GROK_TLS_INSECURE_SKIP_VERIFY", false),
		ProxyURL:                strings.TrimSpace(os.Getenv("GROK_PROXY_URL")),
		NoProxy:                 splitCSV(os.Getenv("GROK_NO_PROXY")),
	}
	cfg.APIKeys = unique(append(splitCSV(os.Getenv("GROK_API_KEYS")), splitCSV(os.Getenv("GROK_API_KEY"))...))
	return cfg, nil
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
