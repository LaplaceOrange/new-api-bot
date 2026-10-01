package bot

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/fsykk/new-api-bot/internal/vendorstatus"
)

func TestVendorProxyEncryptedRedactedAndUsedByWorker(t *testing.T) {
	service, storage, _, qqAPI, _ := testService(t)
	service.cfg.QQAdminOpenIDs["member:g:admin"] = struct{}{}
	proxy := "socks5h://username:unique-password@127.0.0.1:1080"
	service.process(context.Background(), groupEvent("g", "admin", `/vendor_config proxy "`+proxy+`"`))
	runtime, _, err := storage.VendorSettingsSnapshot()
	data, _ := json.Marshal(runtime)
	if err != nil || !runtime.ProxyOverridden || runtime.EncryptedProxy == "" || strings.Contains(string(data), "unique-password") {
		t.Fatal("proxy not encrypted", err)
	}
	cfg, err := service.vendorConfigSnapshot()
	if err != nil || cfg.Proxy != proxy {
		t.Fatal("proxy not loaded", err)
	}
	service.vendorRunner = &fakeVendorRunner{run: func(_ context.Context, request vendorstatus.Request, _ vendorstatus.Callbacks) (vendorstatus.Packet, error) {
		if request.Config.Proxy != proxy {
			t.Fatal("status worker did not receive proxy")
		}
		return vendorstatus.Packet{Text: "test"}, nil
	}}
	service.process(context.Background(), c2cEvent("ordinary", "/vendor_status"))
	service.process(context.Background(), c2cEvent("admin", "/vendor_config proxy"))
	service.process(context.Background(), c2cEvent("admin", "/vendor_config show"))
	for _, message := range qqAPI.messages {
		if strings.Contains(message, "unique-password") || strings.Contains(message, proxy) {
			t.Fatal("proxy credentials echoed")
		}
	}
	service.process(context.Background(), c2cEvent("admin", "/vendor_config proxy off"))
	cfg, _ = service.vendorConfigSnapshot()
	if cfg.Proxy != "" {
		t.Fatal("off did not override saved proxy")
	}
	service.cfg.VendorStatus = vendorstatus.DefaultConfig()
	service.cfg.VendorStatus.Proxy = "socks5://host:1080"
	service.process(context.Background(), c2cEvent("admin", "/vendor_config reset proxy"))
	cfg, _ = service.vendorConfigSnapshot()
	if cfg.Proxy != service.cfg.VendorStatus.Proxy {
		t.Fatal("reset did not restore deployment proxy")
	}
}
