package bot

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fsykk/new-api-bot/internal/model"
	"github.com/fsykk/new-api-bot/internal/store"
	"github.com/fsykk/new-api-bot/internal/vendorstatus"
)

func TestVendorProgressModesControlAcknowledgementStepsAndHeartbeat(t *testing.T) {
	for _, mode := range []string{"detailed", "simple", "off"} {
		t.Run(mode, func(t *testing.T) {
			service, _, _, fake, _ := testService(t)
			q := &vendorProgressQQ{fakeQQ: fake}
			service.qq = q
			cfg := service.vendorBaseConfig()
			cfg.ProgressMode = mode
			started, release := make(chan struct{}), make(chan struct{})
			service.vendorRunner = &fakeVendorRunner{run: func(ctx context.Context, _ vendorstatus.Request, callbacks vendorstatus.Callbacks) (vendorstatus.Packet, error) {
				callbacks.Progress(vendorstatus.Packet{Stage: "fetch", Text: "正在收集 OpenAI 信息……", Completed: 1, Total: 20})
				callbacks.Progress(vendorstatus.Packet{Stage: "render", Text: "正在生成状态总览图片……"})
				close(started)
				select {
				case <-release:
					return vendorstatus.Packet{PNG: []byte("png")}, nil
				case <-ctx.Done():
					return vendorstatus.Packet{}, ctx.Err()
				}
			}}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- service.runVendorQueryWithProgress(ctx, groupEvent("g", "u", "/vendor_status"), cfg, 10*time.Millisecond)
			}()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("worker not started")
			}
			if mode == "off" {
				time.Sleep(35 * time.Millisecond)
			} else {
				awaitVendorProgress(t, func() bool {
					q.historyMu.Lock()
					defer q.historyMu.Unlock()
					if mode == "simple" {
						return len(q.texts) >= 2
					}
					for _, text := range q.texts {
						if strings.Contains(text, "正在生成状态总览图片") {
							return true
						}
					}
					return false
				})
			}
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "off":
				if len(q.texts) != 0 || len(q.recalled) != 0 {
					t.Fatal("off emitted progress", q.texts, q.recalled)
				}
				if len(q.sequences) != 1 || q.sequences[0] != 1 || q.replyIDs[0] == "" {
					t.Fatal("off did not send the image as first reply", q.sequences, q.replyIDs)
				}
			case "simple":
				for _, text := range q.texts {
					if text != "正在生成中……" {
						t.Fatal("simple leaked detailed progress", q.texts)
					}
				}
				if len(q.recalled) != len(q.texts) {
					t.Fatal("simple progress not cleaned", q.recalled)
				}
			case "detailed":
				if !strings.Contains(strings.Join(q.texts, "\n"), "OpenAI") {
					t.Fatal("detailed lost provider stage", q.texts)
				}
			}
		})
	}
}

func TestVendorProgressModesKeepRedactedErrors(t *testing.T) {
	for _, mode := range []string{"simple", "off"} {
		for _, fatal := range []bool{false, true} {
			service, _, _, fake, _ := testService(t)
			q := &vendorProgressQQ{fakeQQ: fake}
			service.qq = q
			cfg := service.vendorBaseConfig()
			cfg.ProgressMode = mode
			service.vendorRunner = &fakeVendorRunner{run: func(_ context.Context, _ vendorstatus.Request, callbacks vendorstatus.Callbacks) (vendorstatus.Packet, error) {
				if fatal {
					return vendorstatus.Packet{}, errors.New("ProxyError: socks5h://u:private-pass@host:1080")
				}
				callbacks.Progress(vendorstatus.Packet{Stage: "warning", Text: "OpenAI: TimeoutError"})
				return vendorstatus.Packet{PNG: []byte("png")}, nil
			}}
			if err := service.runVendorQueryWithProgress(context.Background(), groupEvent("g", "u", "/vendor_status"), cfg, time.Hour); err != nil {
				t.Fatal(err)
			}
			text := strings.Join(q.texts, "\n")
			if strings.Contains(text, "private-pass") {
				t.Fatal("error not redacted", text)
			}
			want := "TimeoutError"
			if fatal {
				want = "ProxyError"
			}
			if !strings.Contains(text, want) {
				t.Fatal("mode swallowed errors", mode, text)
			}
		}
	}
}

func TestVendorProgressModeCommandPersistsAndControlsRealHandler(t *testing.T) {
	service, _, _, fake, _ := testService(t)
	path := filepath.Join(t.TempDir(), "progress-mode.db")
	storage, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	service.store = storage
	service.process(context.Background(), c2cEvent("admin", "/vendor_config progress_mode off"))
	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}
	storage, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	service.store = storage
	cfg, err := service.vendorConfigSnapshot()
	if err != nil || cfg.ProgressMode != "off" {
		t.Fatal("mode not persisted", cfg.ProgressMode, err)
	}
	service.cfg.QQReadOnlyAdminOpenIDs = map[string]struct{}{"user:readonly": {}}
	for _, actor := range []string{"ordinary", "readonly"} {
		service.process(context.Background(), c2cEvent(actor, "/vendor_config progress_mode detailed"))
	}
	service.process(context.Background(), c2cEvent("admin", "/vendor_config progress_mode invalid"))
	cfg, err = service.vendorConfigSnapshot()
	if err != nil || cfg.ProgressMode != "off" {
		t.Fatal("invalid/unauthorized mode changed configuration", cfg.ProgressMode, err)
	}
	service.process(context.Background(), c2cEvent("readonly", "/vendor_config progress_mode"))
	if !strings.Contains(lastReply(t, fake), "off") {
		t.Fatal("read-only query failed", lastReply(t, fake))
	}
	q := &vendorProgressQQ{fakeQQ: fake}
	service.qq = q
	service.vendorRunner = &fakeVendorRunner{}
	service.process(context.Background(), groupEvent("g", "unbound", "/vendor_status"))
	if len(q.texts) != 0 || len(q.sequences) != 1 || q.sequences[0] != 1 {
		t.Fatal("handler ignored persisted mode", q.texts, q.sequences)
	}
	service.process(context.Background(), c2cEvent("admin", "/vendor_config reset progress_mode"))
	cfg, err = service.vendorConfigSnapshot()
	if err != nil || cfg.ProgressMode != "detailed" {
		t.Fatal("mode reset failed", cfg.ProgressMode, err)
	}
	help := service.helpTextFor(model.QQIdentity{UserOpenID: "admin"}, "/vendor_config progress_mode")
	if !strings.Contains(help, "detailed|simple|off") {
		t.Fatal("mode missing from help", help)
	}
}
