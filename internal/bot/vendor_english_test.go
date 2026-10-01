package bot

import (
	"context"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/fsykk/new-api-bot/internal/model"
	"github.com/fsykk/new-api-bot/internal/vendorstatus"
)

func TestVendorChineseCommandsAndKeywordsAreNotAccepted(t *testing.T) {
	service, storage, _, qqAPI, _ := testService(t)
	service.cfg.QQAdminOpenIDs["member:g:admin"] = struct{}{}
	runner := &fakeVendorRunner{}
	service.vendorRunner = runner
	before, groupsBefore, err := storage.VendorSettingsSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{
		"/厂商状态", "/厂商订阅 开", "/厂商配置 enabled on",
		"/vendor_config 帮助", "/vendor_config 查看 enabled",
		"/vendor_config 重置 all", "/vendor_config 监控 on",
		"/vendor_config enabled 开", "/vendor_config enabled 关",
		"/vendor_config card_theme 液态玻璃", "/vendor_config display_language 简体中文",
		"/vendor_config timezone 跟随", "/vendor_config sources openai 开",
		"/vendor_config group_whitelist 添加 g", "/vendor_config group_whitelist 清空",
		`/vendor_config custom 添加 "中文名称" https://status.test`,
		`/vendor_config custom 设置 "中文名称" enabled on`,
	} {
		service.process(context.Background(), c2cEvent("admin", command))
	}
	for _, argument := range []string{"开", "关", "开启", "关闭", "true", "enable"} {
		service.process(context.Background(), groupEvent("g", "admin", "/vendor_subscribe "+argument))
	}
	after, groupsAfter, err := storage.VendorSettingsSnapshot()
	if err != nil || !reflect.DeepEqual(before, after) || !reflect.DeepEqual(groupsBefore, groupsAfter) {
		t.Fatal("removed Chinese command changed persistent settings", err)
	}
	if len(runner.requests) != 0 {
		t.Fatal("removed query command invoked the worker")
	}
	if len(qqAPI.messages) == 0 {
		t.Fatal("invalid commands did not get a usage response")
	}
}

func TestVendorHelpAdvertisesEnglishCommandsOnly(t *testing.T) {
	service, _, _, _, _ := testService(t)
	identity := model.QQIdentity{UserOpenID: "admin"}
	text := service.helpTextFor(identity, "") + "\n" + service.helpTextFor(identity, "/vendor_config") + "\n" + service.helpTextFor(identity, "/vendor_config custom")
	if regexp.MustCompile(`/[\p{Han}]+`).MatchString(text) {
		t.Fatal("help still advertises Chinese command names")
	}
	for _, expected := range []string{
		"/vendor_status", "/vendor_subscribe", "/vendor_config",
		"/vendor_config reset <key|all>", "custom add", "custom set",
	} {
		if !strings.Contains(text, expected) {
			t.Fatal("English command missing from help", expected)
		}
	}
}

func TestVendorEnglishCustomCommandPreservesChineseFreeformName(t *testing.T) {
	service, _, _, _, _ := testService(t)
	service.process(context.Background(), c2cEvent("admin", `/vendor_config custom add "中文服务名称" https://status.test off`))
	cfg, err := service.vendorConfigSnapshot()
	if err != nil || len(cfg.CustomStatuspageSources) != 1 ||
		cfg.CustomStatuspageSources[0].Name != "中文服务名称" || cfg.CustomStatuspageSources[0].Enabled {
		t.Fatal(cfg.CustomStatuspageSources, err)
	}
	service.process(context.Background(), c2cEvent("admin", `/vendor_config custom set "中文服务名称" enabled on`))
	cfg, err = service.vendorConfigSnapshot()
	if err != nil || !cfg.CustomStatuspageSources[0].Enabled {
		t.Fatal(cfg.CustomStatuspageSources, err)
	}
}

func TestVendorEnglishConfigurationRestoresExistingCanonicalData(t *testing.T) {
	service, storage, _, _, _ := testService(t)
	service.process(context.Background(), c2cEvent("admin", `/vendor_config card_theme midnight`))
	service.process(context.Background(), c2cEvent("admin", `/vendor_config sources.openai off`))
	runtime, _, _ := storage.VendorSettingsSnapshot()
	cfg, err := vendorstatus.ApplyOverrides(vendorstatus.DefaultConfig(), runtime.Overrides)
	if err != nil || cfg.CardTheme != "midnight" || cfg.Sources["openai"] {
		t.Fatal("canonical persistent format changed", err)
	}
	service.process(context.Background(), c2cEvent("admin", `/vendor_config reset card_theme`))
	cfg, err = service.vendorConfigSnapshot()
	if err != nil || cfg.CardTheme != "paper" || cfg.Sources["openai"] {
		t.Fatal("English reset did not preserve other settings", err)
	}
}
