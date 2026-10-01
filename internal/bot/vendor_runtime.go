package bot

import (
	"context"
	"encoding/json"
	"errors"
	"sort"

	"github.com/fsykk/new-api-bot/internal/store"
	"github.com/fsykk/new-api-bot/internal/vendorstatus"
)

func (s *Service) vendorBaseConfig() vendorstatus.Config {
	cfg := s.cfg.VendorStatus
	if cfg.Python == "" {
		cfg = vendorstatus.DefaultConfig()
		cfg.Enabled = s.cfg.VendorStatus.Enabled
		cfg.GroupWhitelist = append([]string(nil), s.cfg.VendorStatus.GroupWhitelist...)
	}
	return cfg
}

func (s *Service) vendorConfigFromRuntime(runtime store.VendorRuntimeConfig) (vendorstatus.Config, error) {
	cfg, err := vendorstatus.ApplyOverrides(s.vendorBaseConfig(), runtime.Overrides)
	if err != nil {
		return vendorstatus.Config{}, err
	}
	if runtime.TranslationAPIKeyOverridden {
		if runtime.EncryptedTranslationAPIKey == "" {
			cfg.Translation.APIKey = ""
		} else {
			cfg.Translation.APIKey, err = s.secure.Decrypt(runtime.EncryptedTranslationAPIKey)
			if err != nil {
				return vendorstatus.Config{}, errors.New("解密厂商翻译密钥失败")
			}
		}
	}
	if runtime.ProxyOverridden {
		cfg.Proxy = ""
		if runtime.EncryptedProxy != "" {
			cfg.Proxy, err = s.secure.Decrypt(runtime.EncryptedProxy)
			if err != nil {
				return vendorstatus.Config{}, errors.New("解密厂商代理失败")
			}
		}
	}
	if err := vendorstatus.ValidateProxy(cfg.Proxy); err != nil {
		return vendorstatus.Config{}, err
	}
	return cfg, nil
}

func (s *Service) vendorConfigSnapshot() (vendorstatus.Config, error) {
	runtime, overrides, err := s.store.VendorSettingsSnapshot()
	if err != nil {
		return vendorstatus.Config{}, err
	}
	cfg, err := s.vendorConfigFromRuntime(runtime)
	if err != nil {
		return vendorstatus.Config{}, err
	}
	groups := map[string]bool{}
	for _, group := range cfg.GroupWhitelist {
		groups[group] = true
	}
	for group, enabled := range overrides {
		groups[group] = enabled
	}
	cfg.GroupWhitelist = nil
	for group, enabled := range groups {
		if enabled {
			cfg.GroupWhitelist = append(cfg.GroupWhitelist, group)
		}
	}
	sort.Strings(cfg.GroupWhitelist)
	return cfg, nil
}

func (s *Service) setVendorConfigValue(key string, value any, reset bool) error {
	runtime, _, err := s.store.VendorSettingsSnapshot()
	if err != nil {
		return err
	}
	// Normalize legacy whole-source objects into explicit leaves. This lets
	// "reset sources.foo" really restore that source's deployment value while
	// preserving every other source set by a previous bulk command.
	if raw, ok := runtime.Overrides["sources"]; ok && !(reset && (key == "all" || key == "sources")) {
		var sources map[string]bool
		if err := json.Unmarshal(raw, &sources); err != nil {
			return err
		}
		for _, id := range vendorstatus.SourceIDs {
			key := "sources." + id
			if _, exists := runtime.Overrides[key]; !exists {
				enabled, exists := sources[id]
				runtime.Overrides[key], _ = json.Marshal(!exists || enabled)
			}
		}
		delete(runtime.Overrides, "sources")
	}
	replaceGroups := key == "group_whitelist" || key == "all"
	if reset {
		if key == "all" {
			runtime = store.VendorRuntimeConfig{Version: 1, Overrides: make(map[string]json.RawMessage)}
		} else if key == "translation.api_key" {
			runtime.TranslationAPIKeyOverridden = false
			runtime.EncryptedTranslationAPIKey = ""
		} else if key == "proxy" {
			runtime.ProxyOverridden = false
			runtime.EncryptedProxy = ""
		} else {
			delete(runtime.Overrides, key)
			if key == "sources" {
				for _, id := range vendorstatus.SourceIDs {
					delete(runtime.Overrides, "sources."+id)
				}
			}
		}
	} else if key == "translation.api_key" || key == "proxy" {
		secret, ok := value.(string)
		if !ok {
			return errors.New("翻译密钥类型无效")
		}
		if key == "proxy" {
			if err := vendorstatus.ValidateProxy(secret); err != nil {
				return err
			}
		}
		ciphertext := ""
		if secret != "" {
			ciphertext, err = s.secure.Encrypt(secret)
			if err != nil {
				return errors.New("加密翻译密钥失败")
			}
		}
		if key == "proxy" {
			runtime.ProxyOverridden = true
			runtime.EncryptedProxy = ciphertext
		} else {
			runtime.TranslationAPIKeyOverridden = true
			runtime.EncryptedTranslationAPIKey = ciphertext
		}
	} else {
		if key == "sources" {
			for _, id := range vendorstatus.SourceIDs {
				delete(runtime.Overrides, "sources."+id)
			}
			sources, ok := value.(map[string]bool)
			if !ok {
				return errors.New("状态源配置类型无效")
			}
			for id := range sources {
				if _, ok := vendorstatus.FindOption("sources." + id); !ok {
					return errors.New("未知内置状态源")
				}
			}
			for _, id := range vendorstatus.SourceIDs {
				enabled, exists := sources[id]
				runtime.Overrides["sources."+id], _ = json.Marshal(!exists || enabled)
			}
		} else {
			runtime.Overrides[key], err = json.Marshal(value)
			if err != nil {
				return err
			}
		}
	}
	if _, err := s.vendorConfigFromRuntime(runtime); err != nil {
		return err
	}
	return s.store.PutVendorRuntimeConfig(runtime, replaceGroups)
}

func (s *Service) signalVendorConfigChanged() {
	s.vendorPollMu.Lock()
	if s.vendorPollCancel != nil {
		s.vendorPollCancel()
	}
	s.vendorPollMu.Unlock()
	select {
	case s.vendorConfigWake <- struct{}{}:
	default:
	}
}

func (s *Service) vendorDisplayTimezone(cfg *vendorstatus.Config) {
	if cfg.Timezone == "" {
		cfg.Timezone = s.cfg.CheckinTimezoneName
		if cfg.Timezone == "" && s.cfg.CheckinTimezone != nil {
			cfg.Timezone = s.cfg.CheckinTimezone.String()
		}
	}
}

func (s *Service) beginVendorPoll(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	s.vendorPollMu.Lock()
	s.vendorPollCancel = cancel
	s.vendorPollMu.Unlock()
	return ctx, func() {
		cancel()
		s.vendorPollMu.Lock()
		s.vendorPollCancel = nil
		s.vendorPollMu.Unlock()
	}
}
