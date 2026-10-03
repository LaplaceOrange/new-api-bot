package llm

import (
	"strings"
	"testing"
)

func TestConfigDefaultsAndValidation(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.SystemPrompt != "" || cfg.Enabled || cfg.SearchBackend != "off" || cfg.Validate() != nil {
		t.Fatal(cfg)
	}
	if err := cfg.Set("system_prompt", "名字、人格由管理员决定\n多行 <提示>"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Set("system_prompt", ""); err != nil || cfg.SystemPrompt != "" {
		t.Fatal(err, cfg.SystemPrompt)
	}
	if err := cfg.Set("enabled", "on"); err != nil || !cfg.Enabled {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{"api_key", "bad\nkey"}, {"base_url", "ftp://test"}, {"base_url", "https://user:secret@example.test"}, {"minute_limit", "0"}, {"concurrency", "17"}, {"timeout_seconds", "999"}, {"chunk_runes", "200"}, {"search_backend", "automatic"}, {"unknown", "x"}} {
		before := cfg
		if err := cfg.Set(pair[0], pair[1]); err == nil {
			t.Fatalf("%v accepted", pair)
		}
		if cfg != before {
			t.Fatal("failed validation mutated config")
		}
	}
}
func TestEnvironmentConfig(t *testing.T) {
	for _, key := range Keys() {
		t.Setenv("LLM_"+strings.ToUpper(key), "")
	}
	t.Setenv("LLM_SYSTEM_PROMPT", "管理员人格\n第二行")
	t.Setenv("LLM_ENABLED", "true")
	t.Setenv("LLM_MINUTE_LIMIT", "3")
	cfg, err := LoadEnvironment()
	if err != nil || !cfg.Enabled || cfg.MinuteLimit != 3 || cfg.SystemPrompt != "管理员人格\n第二行" {
		t.Fatal(cfg, err)
	}
	t.Setenv("LLM_BASE_URL", "https://user:secret@example.test")
	_, err = LoadEnvironment()
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal(err)
	}
}
