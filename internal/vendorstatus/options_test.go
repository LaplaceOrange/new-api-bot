package vendorstatus

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"unicode"
)

func TestCommandRegistryCoversEveryUpstreamConfiguration(t *testing.T) {
	data, err := os.ReadFile("worker/statusmonitor/_conf_schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]struct {
		Items map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	if len(schema) != 18 {
		t.Fatalf("upstream inventory changed; re-audit command coverage: %d", len(schema))
	}
	for key := range schema {
		option, ok := FindOption(key)
		if !ok || option.Example == "" || option.Label == "" {
			t.Fatalf("no documented command mapping for upstream %s", key)
		}
	}
	for id := range schema["sources"].Items {
		if option, ok := FindOption("sources." + id); !ok || option.Kind != "bool" {
			t.Fatalf("source %s has no command", id)
		}
	}
	if option, _ := FindOption("translation_provider_id"); option.Key != "translation.model" {
		t.Fatal(option)
	}
}

func TestOptionCommandTokensAndExamplesUseEnglishOnly(t *testing.T) {
	for _, option := range Options {
		values := append([]string{option.Key, option.Example}, option.Aliases...)
		for _, value := range values {
			for _, char := range value {
				if unicode.Is(unicode.Han, char) {
					t.Fatalf("Chinese command token remains for %s: %s", option.Key, value)
				}
			}
		}
	}
	for _, alias := range []string{"监控", "主题", "语言", "来源", "自定义", "模型", "字体"} {
		if _, ok := FindOption(alias); ok {
			t.Fatalf("removed Chinese option alias still accepted: %s", alias)
		}
	}
}

func TestRegistryAlsoCoversAllNativeConfigFields(t *testing.T) {
	data, err := json.Marshal(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]json.RawMessage
	_ = json.Unmarshal(data, &config)
	for key := range config {
		if key == "translation" {
			for _, field := range []string{"base_url", "api_key", "model"} {
				if _, ok := FindOption("translation." + field); !ok {
					t.Fatal(field)
				}
			}
		} else if _, ok := FindOption(key); !ok {
			t.Fatalf("native field %s has no command", key)
		}
	}
}

func TestRuntimeOverlayDeepCopiesAndHasStableObjectLeafPrecedence(t *testing.T) {
	base := DefaultConfig()
	base.Sources["claude"] = false
	overrides := map[string]json.RawMessage{
		"sources":        []byte(`{"openai":false}`),
		"sources.openai": []byte(`true`),
		"card_theme":     []byte(`"terminal"`),
	}
	for range 30 {
		result, err := ApplyOverrides(base, overrides)
		if err != nil || !result.Sources["openai"] || result.CardTheme != "terminal" {
			t.Fatal(result, err)
		}
		if _, ok := result.Sources["claude"]; ok {
			t.Fatal("whole-object replacement retained obsolete keys")
		}
		result.Sources["openai"] = false
		if !reflect.DeepEqual(base.Sources, map[string]bool{"claude": false}) {
			t.Fatal("runtime changed deployment map", base.Sources)
		}
	}
	for key, value := range map[string]string{
		"translation.api_key": `"plaintext"`, "platform_type": `"aiocqhttp"`,
		"missing": `true`, "sources.typo": `false`, "poll_interval_seconds": `9999999999999`,
	} {
		if _, err := ApplyOverrides(base, map[string]json.RawMessage{key: []byte(value)}); err == nil {
			t.Fatalf("invalid or plaintext override accepted: %s", key)
		}
	}
}

func TestRuntimeTranslationAllowsStepByStepConfiguration(t *testing.T) {
	cfg, err := ApplyOverrides(DefaultConfig(), map[string]json.RawMessage{
		"translation.model": []byte(`"translator"`),
	})
	if err != nil || cfg.Translation.Model != "translator" {
		t.Fatal(cfg, err)
	}
	if _, err := ApplyOverrides(cfg, map[string]json.RawMessage{
		"translation.base_url": []byte(`"file:///secret"`),
	}); err == nil {
		t.Fatal("invalid partial URL accepted")
	}
}
