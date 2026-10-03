package config

import (
	"strings"
	"testing"

	"github.com/fsykk/new-api-bot/internal/llm"
)

func TestLLMDeploymentConfig(t *testing.T) {
	for _, key := range llm.Keys() {
		t.Setenv("LLM_"+strings.ToUpper(key), "")
	}
	cfg, err := Load()
	if cfg.LLM.Enabled || cfg.LLM.SystemPrompt != "" || cfg.LLM.SearchBackend != "off" || cfg.LLM.HistoryTurns != 20 {
		t.Fatal(cfg.LLM)
	}
	if err != nil && strings.Contains(err.Error(), "LLM") {
		t.Fatal(err)
	}
	t.Setenv("LLM_SYSTEM_PROMPT", "自定义人格\n不写死")
	t.Setenv("LLM_ENABLED", "true")
	t.Setenv("LLM_CONCURRENCY", "3")
	cfg, err = Load()
	if !cfg.LLM.Enabled || cfg.LLM.Concurrency != 3 || cfg.LLM.SystemPrompt != "自定义人格\n不写死" || (err != nil && strings.Contains(err.Error(), "LLM")) {
		t.Fatal(cfg.LLM, err)
	}
	t.Setenv("LLM_BASE_URL", "https://user:secret@example.test")
	_, err = Load()
	if err == nil || !strings.Contains(err.Error(), "LLM") || strings.Contains(err.Error(), "secret") {
		t.Fatal(err)
	}
}
