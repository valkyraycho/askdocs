package llm

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
)

const (
	defaultBaseURL         = "https://api.openai.com/v1"
	defaultEmbedModel      = "text-embedding-3-small"
	defaultChatModel       = "gpt-4o-mini"
	defaultMaxAnswerTokens = 1024
)

type Config struct {
	BaseURL         string
	APIKey          string
	EmbedModel      string
	ChatModel       string
	MaxAnswerTokens int
}

func ConfigFromEnv() (Config, error) {
	cfg := Config{
		BaseURL:         envOr("OPENAI_BASE_URL", defaultBaseURL),
		APIKey:          os.Getenv("OPENAI_API_KEY"),
		EmbedModel:      envOr("ASKDOCS_EMBED_MODEL", defaultEmbedModel),
		ChatModel:       envOr("ASKDOCS_CHAT_MODEL", defaultChatModel),
		MaxAnswerTokens: defaultMaxAnswerTokens,
	}
	if v := os.Getenv("ASKDOCS_MAX_ANSWER_TOKENS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return Config{}, fmt.Errorf("invalid ASKDOCS_MAX_ANSWER_TOKENS %q", v)
		}
		cfg.MaxAnswerTokens = n
	}

	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Host == "" {
		return Config{}, fmt.Errorf("invalid OPENAI_BASE_URL %q", cfg.BaseURL)
	}
	loopback := isLoopback(u.Hostname())
	if u.Scheme == "http" && !loopback && os.Getenv("ASKDOCS_ALLOW_INSECURE") == "" {
		return Config{}, errors.New("OPENAI_BASE_URL uses plain http to a remote host — the API key and your documents would travel unencrypted (set ASKDOCS_ALLOW_INSECURE=1 to override)")
	}
	if cfg.APIKey == "" && !loopback {
		return Config{}, errors.New("OPENAI_API_KEY is not set (only loopback endpoints like Ollama may run keyless)")
	}
	return cfg, nil
}

func (c Config) Host() string {
	u, err := url.Parse(c.BaseURL)
	if err != nil {
		return c.BaseURL
	}
	return u.Hostname()
}

func isLoopback(hostname string) bool {
	return hostname == "localhost" || hostname == "127.0.0.1" || hostname == "::1"
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
