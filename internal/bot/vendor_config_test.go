package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fsykk/new-api-bot/internal/store"
	"github.com/fsykk/new-api-bot/internal/vendorstatus"
)

func TestVendorConfigScalarCommandsCoverEveryWritableScalar(t *testing.T) {
	cases := []struct {
		key, input, expected string
	}{
		{"enabled", "on", "true"},
		{"poll_interval_seconds", "2m", "120"},
		{"notify_maintenance", "true", "true"},
		{"history_lookback_hours", "0", "0"},
		{"notify_source_failures", "off", "false"},
		{"source_failure_threshold", "5", "5"},
		{"source_failure_cooldown_seconds", "2h", "7200"},
		{"notify_existing_on_first_startup", "false", "false"},
		{"display_language", "zh-CN", `"zh-CN"`},
		{"progress_mode", "simple", `"simple"`},
		{"card_theme", "liquid_glass", `"liquid_glass"`},
		{"timezone", "UTC", `"UTC"`},
		{"enable_ai_translation", "off", "false"},
		{"translation.model", "translator", `"translator"`},
		{"python", `"C:\Python folder\python.exe"`, `"C:\\Python folder\\python.exe"`},
		{"http_timeout_seconds", "20s", "20"},
		{"worker_timeout_seconds", "3m", "180"},
		{"font_path", `"C:\font folder\font.ttf"`, `"C:\\font folder\\font.ttf"`},
		{"translation.base_url", "https://model.test/v1", `"https://model.test/v1"`},
	}
	covered := map[string]bool{}
	for _, tc := range cases {
		covered[tc.key] = true
		t.Run(tc.key, func(t *testing.T) {
			service, _, _, qqAPI, _ := testService(t)
			service.process(context.Background(), c2cEvent("admin", "/vendor_config "+tc.key+" "+tc.input))
			cfg, err := service.vendorConfigSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			option, _ := vendorstatus.FindOption(tc.key)
			if got := sTrim(service.vendorOptionDisplay(cfg, option, false)); got != tc.expected {
				t.Fatalf("value = %s, want %s; replies=%v", got, tc.expected, qqAPI.messages)
			}
		})
	}
	for _, option := range vendorstatus.Options {
		if option.ReadOnly || option.Secret || option.Kind == "groups" || option.Kind == "custom" || option.Kind == "sources" {
			continue
		}
		if !covered[option.Key] {
			t.Fatalf("writable scalar %s has no command test", option.Key)
		}
	}
}

func sTrim(value string) string { return strings.TrimSpace(value) }

func TestVendorConfigAllTwentySourceCommandsAndWholeMapReset(t *testing.T) {
	service, _, _, _, _ := testService(t)
	for _, id := range vendorstatus.SourceIDs {
		service.process(context.Background(), c2cEvent("admin", "/vendor_config sources "+id+" off"))
	}
	cfg, err := service.vendorConfigSnapshot()
	if err != nil || len(cfg.Sources) != 20 {
		t.Fatal(cfg.Sources, err)
	}
	for _, id := range vendorstatus.SourceIDs {
		if cfg.Sources[id] {
			t.Fatal("source not disabled", id)
		}
	}
	service.process(context.Background(), c2cEvent("admin", `/vendor_config sources {"openai":true}`))
	cfg, err = service.vendorConfigSnapshot()
	if err != nil || len(cfg.Sources) != 20 {
		t.Fatal(cfg.Sources, err)
	}
	for id, enabled := range cfg.Sources {
		if !enabled {
			t.Fatal("bulk replacement retained obsolete disabled sources", id)
		}
	}
	service.process(context.Background(), c2cEvent("admin", `/vendor_config sources.openai false`))
	cfg, _ = service.vendorConfigSnapshot()
	if cfg.Sources["openai"] {
		t.Fatal("dotted source command failed")
	}
	service.process(context.Background(), c2cEvent("admin", `/vendor_config reset sources`))
	cfg, _ = service.vendorConfigSnapshot()
	if len(cfg.Sources) != 0 {
		t.Fatal("source reset did not remove whole/leaf overrides", cfg.Sources)
	}
}

func TestVendorResetOneSourceUnderBulkRestoresDeploymentOnlyForThatSource(t *testing.T) {
	service, storage, _, _, _ := testService(t)
	service.cfg.VendorStatus = vendorstatus.DefaultConfig()
	service.cfg.VendorStatus.Sources["claude"] = false
	// A legacy bulk object must be normalized without losing its other values.
	if err := storage.PutVendorRuntimeConfig(store.VendorRuntimeConfig{
		Version: 1, Overrides: map[string]json.RawMessage{"sources": []byte(`{"openai":false,"claude":true}`)},
	}, false); err != nil {
		t.Fatal(err)
	}
	service.process(context.Background(), c2cEvent("admin", `/vendor_config reset sources.claude`))
	cfg, err := service.vendorConfigSnapshot()
	if err != nil || cfg.Sources["claude"] || cfg.Sources["openai"] || !cfg.Sources["groq"] {
		t.Fatal(cfg.Sources, err)
	}
}

func TestVendorConfigWhitelistCRUDReplacesAndResetsSubscriptions(t *testing.T) {
	service, storage, _, _, _ := testService(t)
	service.cfg.VendorStatus = vendorstatus.DefaultConfig()
	service.cfg.VendorStatus.GroupWhitelist = []string{"deployment"}
	_, _, _ = storage.SetVendorSubscription("manual", true, nil)
	for _, command := range []string{
		`/vendor_config group_whitelist ["one", "two", "one"]`,
		`/vendor_config group_whitelist add three`,
		`/vendor_config group_whitelist remove two`,
	} {
		service.process(context.Background(), c2cEvent("admin", command))
	}
	cfg, err := service.vendorConfigSnapshot()
	if err != nil || !reflect.DeepEqual(cfg.GroupWhitelist, []string{"one", "three"}) {
		t.Fatal(cfg.GroupWhitelist, err)
	}
	_, overrides, _ := storage.VendorSettingsSnapshot()
	if len(overrides) != 0 {
		t.Fatal("replacement retained stale subscription overrides", overrides)
	}
	service.cfg.QQAdminOpenIDs["member:g:admin"] = struct{}{}
	service.process(context.Background(), groupEvent("g", "admin", "/vendor_subscribe on"))
	cfg, _ = service.vendorConfigSnapshot()
	if !reflect.DeepEqual(cfg.GroupWhitelist, []string{"g", "one", "three"}) {
		t.Fatal("subscription did not merge with runtime whitelist", cfg.GroupWhitelist)
	}
	service.process(context.Background(), c2cEvent("admin", `/vendor_config group_whitelist clear`))
	cfg, _ = service.vendorConfigSnapshot()
	if len(cfg.GroupWhitelist) != 0 {
		t.Fatal(cfg.GroupWhitelist)
	}
	service.process(context.Background(), c2cEvent("admin", `/vendor_config reset group_whitelist`))
	cfg, _ = service.vendorConfigSnapshot()
	if !reflect.DeepEqual(cfg.GroupWhitelist, []string{"deployment"}) {
		t.Fatal(cfg.GroupWhitelist)
	}
}

func TestVendorConfigCustomSourceCRUDAndEveryTemplateField(t *testing.T) {
	service, _, _, qqAPI, _ := testService(t)
	commands := []string{
		`/vendor_config custom add "My Status" https://status.test`,
		`/vendor_config custom set "My Status" name "New Name"`,
		`/vendor_config custom set "New Name" base_url https://new-status.test`,
		`/vendor_config custom set "New Name" enabled off`,
	}
	for _, command := range commands {
		service.process(context.Background(), c2cEvent("admin", command))
	}
	cfg, err := service.vendorConfigSnapshot()
	want := []vendorstatus.CustomSource{{Name: "New Name", BaseURL: "https://new-status.test", Enabled: false}}
	if err != nil || !reflect.DeepEqual(cfg.CustomStatuspageSources, want) {
		t.Fatal(cfg.CustomStatuspageSources, err, qqAPI.messages)
	}
	service.process(context.Background(), c2cEvent("admin", `/vendor_config custom enable "New Name"`))
	cfg, _ = service.vendorConfigSnapshot()
	if !cfg.CustomStatuspageSources[0].Enabled {
		t.Fatal(cfg.CustomStatuspageSources)
	}
	service.process(context.Background(), c2cEvent("admin", `/vendor_config custom remove "New Name"`))
	cfg, _ = service.vendorConfigSnapshot()
	if len(cfg.CustomStatuspageSources) != 0 {
		t.Fatal(cfg.CustomStatuspageSources)
	}
	service.process(context.Background(), c2cEvent("admin", `/vendor_config custom [{"name":"Bulk Status", "base_url":"https://bulk.test"}]`))
	cfg, err = service.vendorConfigSnapshot()
	if err != nil || len(cfg.CustomStatuspageSources) != 1 || !cfg.CustomStatuspageSources[0].Enabled {
		t.Fatal(cfg.CustomStatuspageSources, err)
	}
}

func TestVendorConfigPermissionInvalidParametersAndFixedPlatformDoNotWrite(t *testing.T) {
	service, storage, _, qqAPI, _ := testService(t)
	service.cfg.QQReadOnlyAdminOpenIDs = map[string]struct{}{"user:readonly": {}}
	before, _, _ := storage.VendorSettingsSnapshot()
	commands := []string{
		`enabled bad`, `poll_interval_seconds 59`, `poll_interval_seconds 99999999999999`,
		`history_lookback_hours 169`, `source_failure_threshold 0`, `source_failure_cooldown_seconds 2`,
		`display_language invalid`, `card_theme invalid`, `timezone Not/AZone`,
		`python ""`, `http_timeout_seconds 0`, `worker_timeout_seconds 10`,
		`group_whitelist ["1:GroupMessage:100"]`, `group_whitelist remove invalid:group`,
		`sources {"typo":false}`, `sources {"openai":null}`,
		`custom add test file:///test`, `custom [{"name":"a","base_url":"https://a.test"},{"name":"a","base_url":"https://b.test"}]`,
		`translation.base_url not-a-url`, `platform_type aiocqhttp`, `platform_id other`,
		`card_theme "unterminated`, `查看 enabled false`,
	}
	for _, command := range commands {
		service.process(context.Background(), c2cEvent("admin", "/vendor_config "+command))
	}
	service.process(context.Background(), c2cEvent("ordinary", `/vendor_config card_theme terminal`))
	service.process(context.Background(), c2cEvent("readonly", `/vendor_config card_theme terminal`))
	after, _, _ := storage.VendorSettingsSnapshot()
	if !reflect.DeepEqual(before, after) {
		t.Fatal("invalid/unauthorized commands changed runtime", after)
	}
	beforeReplies := len(qqAPI.messages)
	service.process(context.Background(), c2cEvent("readonly", `/vendor_config show card_theme`))
	if len(qqAPI.messages) != beforeReplies+1 || !strings.Contains(qqAPI.messages[len(qqAPI.messages)-1], `"paper"`) {
		t.Fatal("read-only admin could not query", qqAPI.messages)
	}
	service.process(context.Background(), c2cEvent("admin", `/disable "vendor_config"`))
	beforeReplies = len(qqAPI.messages)
	service.process(context.Background(), c2cEvent("admin", `/vendor_config card_theme terminal`))
	if len(qqAPI.messages) != beforeReplies {
		t.Fatal("disabled command was not silent")
	}
}

func TestVendorConfigSecretsArePrivateEncryptedRedactedAndPersisted(t *testing.T) {
	service, _, _, qqAPI, _ := testService(t)
	path := filepath.Join(t.TempDir(), "secret.db")
	storage, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	service.store = storage
	key := `sk-unique<angle> token\path`
	service.cfg.QQAdminOpenIDs["member:g:admin"] = struct{}{}
	service.process(context.Background(), groupEvent("g", "admin", `/vendor_config translation.api_key "`+key+`"`))
	runtime, _, _ := storage.VendorSettingsSnapshot()
	if runtime.TranslationAPIKeyOverridden {
		t.Fatal("group key was saved")
	}
	service.process(context.Background(), c2cEvent("admin", `/vendor_config translation.api_key "`+key+`"`))
	service.process(context.Background(), c2cEvent("admin", `/vendor_config translation.base_url https://model.test/v1`))
	service.process(context.Background(), c2cEvent("admin", `/vendor_config translation_provider_id test-model`))
	service.process(context.Background(), c2cEvent("admin", `/vendor_config show`))
	service.process(context.Background(), c2cEvent("admin", `/vendor_config translation.api_key`))
	cfg, err := service.vendorConfigSnapshot()
	if err != nil || cfg.Translation.APIKey != key || cfg.Translation.Model != "test-model" {
		t.Fatal("translator settings were not applied", err)
	}
	runtime, _, _ = storage.VendorSettingsSnapshot()
	encoded, _ := json.Marshal(runtime)
	if runtime.EncryptedTranslationAPIKey == "" || bytes.Contains(encoded, []byte(key)) {
		t.Fatal("plaintext persisted in settings")
	}
	for _, reply := range qqAPI.messages {
		if strings.Contains(reply, key) {
			t.Fatal("key echoed")
		}
	}
	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}
	database, err := os.ReadFile(path)
	if err != nil || bytes.Contains(database, []byte(key)) {
		t.Fatal("key appeared in the database/audit", err)
	}
	storage, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	service.store = storage
	service.cfg.VendorStatus = vendorstatus.DefaultConfig()
	service.cfg.VendorStatus.CardTheme = "terminal" // Unchanged deployment fields stay live.
	cfg, err = service.vendorConfigSnapshot()
	if err != nil || cfg.Translation.APIKey != key || cfg.CardTheme != "terminal" {
		t.Fatal("runtime overrides did not survive restart", err)
	}
	service.process(context.Background(), c2cEvent("admin", `/vendor_config translation.api_key ""`))
	cfg, _ = service.vendorConfigSnapshot()
	if cfg.Translation.APIKey != "" {
		t.Fatal("empty-string key clear did not take effect")
	}
}

func TestVendorConfigConcurrentChangesAreNotLost(t *testing.T) {
	service, _, _, _, _ := testService(t)
	var wg sync.WaitGroup
	for _, id := range vendorstatus.SourceIDs {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			service.process(context.Background(), c2cEvent("admin", "/vendor_config sources."+id+" false"))
		}(id)
	}
	wg.Wait()
	cfg, err := service.vendorConfigSnapshot()
	if err != nil || len(cfg.Sources) != 20 {
		t.Fatal(cfg.Sources, err)
	}
}

func TestVendorConfigHotEnableDisableCancelsPollAndCanRestart(t *testing.T) {
	service, _, _, _, _ := testService(t)
	service.cfg.VendorStatus = vendorstatus.DefaultConfig()
	service.cfg.VendorStatus.Enabled = false
	started := make(chan struct{}, 4)
	cancelled := make(chan struct{}, 4)
	service.vendorRunner = &fakeVendorRunner{run: func(ctx context.Context, _ vendorstatus.Request, _ vendorstatus.Callbacks) (vendorstatus.Packet, error) {
		started <- struct{}{}
		<-ctx.Done()
		cancelled <- struct{}{}
		return vendorstatus.Packet{}, ctx.Err()
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		service.runVendorStatusWorker(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("worker did not stop")
		}
	}()
	for range 2 {
		service.process(context.Background(), c2cEvent("admin", "/vendor_config enabled on"))
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("dormant scheduler did not start after enable")
		}
		service.process(context.Background(), c2cEvent("admin", "/vendor_config enabled off"))
		select {
		case <-cancelled:
		case <-time.After(2 * time.Second):
			t.Fatal("disable did not cancel the running poll")
		}
	}
}

func TestVendorRuntimeSettingsReachNewQueriesAndResetKeepsIncidentLedger(t *testing.T) {
	service, storage, _, _, _ := testService(t)
	service.process(context.Background(), c2cEvent("admin", "/vendor_config card_theme midnight"))
	service.process(context.Background(), c2cEvent("admin", "/vendor_config timezone inherit"))
	service.cfg.CheckinTimezoneName = "Europe/London"
	service.vendorRunner = &fakeVendorRunner{run: func(_ context.Context, request vendorstatus.Request, _ vendorstatus.Callbacks) (vendorstatus.Packet, error) {
		if request.Config.CardTheme != "midnight" || request.Config.Timezone != "Europe/London" {
			t.Fatal(request.Config.CardTheme, request.Config.Timezone)
		}
		return vendorstatus.Packet{Text: "test"}, nil
	}}
	service.process(context.Background(), c2cEvent("ordinary", "/vendor_status"))
	ledger := json.RawMessage(`{"version":1,"sources":{"keep":{}},"deliveries":{}}`)
	_ = storage.PutVendorStatusValue(vendorstatus.StateKey, ledger)
	service.process(context.Background(), c2cEvent("admin", "/vendor_config reset all"))
	cfg, err := service.vendorConfigSnapshot()
	if err != nil || cfg.CardTheme != "paper" {
		t.Fatal(cfg.CardTheme, err)
	}
	values, _ := storage.VendorStatusValues()
	if !bytes.Equal(values[vendorstatus.StateKey], ledger) {
		t.Fatal("configuration reset erased the incident ledger")
	}
}
