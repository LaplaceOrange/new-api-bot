package bot

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/fsykk/new-api-bot/internal/qq"
	"github.com/fsykk/new-api-bot/internal/vendorstatus"
)

type fakeVendorRunner struct {
	requests []vendorstatus.Request
	run      func(context.Context, vendorstatus.Request, vendorstatus.Callbacks) (vendorstatus.Packet, error)
}

func (f *fakeVendorRunner) Run(ctx context.Context, request vendorstatus.Request, callbacks vendorstatus.Callbacks) (vendorstatus.Packet, error) {
	f.requests = append(f.requests, request)
	if f.run != nil {
		return f.run(ctx, request, callbacks)
	}
	return vendorstatus.Packet{PNG: []byte("png")}, nil
}

type vendorImageQQ struct {
	*fakeQQ
	scenes    []string
	targets   []string
	replyTo   []string
	sequences []int
}

func (f *vendorImageQQ) SendGroupFile(_ context.Context, group, replyTo, _ string, kind int, data []byte) (qq.SentMessage, error) {
	f.scenes = append(f.scenes, "group")
	f.targets = append(f.targets, group)
	f.replyTo = append(f.replyTo, replyTo)
	return qq.SentMessage{ID: "status-image"}, nil
}

func (f *vendorImageQQ) SendGroupFileWithSequence(ctx context.Context, group, replyTo, file string, kind int, data []byte, sequence int) (qq.SentMessage, error) {
	f.sequences = append(f.sequences, sequence)
	return f.SendGroupFile(ctx, group, replyTo, file, kind, data)
}

func (f *vendorImageQQ) SendC2CFileWithSequence(ctx context.Context, user, replyTo, file string, kind int, data []byte, sequence int) (qq.SentMessage, error) {
	f.sequences = append(f.sequences, sequence)
	return f.SendC2CFile(ctx, user, replyTo, file, kind, data)
}

func (f *vendorImageQQ) SendC2CFile(_ context.Context, user, replyTo, _ string, kind int, data []byte) (qq.SentMessage, error) {
	f.scenes = append(f.scenes, "c2c")
	f.targets = append(f.targets, user)
	f.replyTo = append(f.replyTo, replyTo)
	return qq.SentMessage{ID: "private-status-image"}, nil
}

func TestVendorQueryIsPublicAndSupportsGroupAndC2C(t *testing.T) {
	for _, command := range []string{"/vendor_status"} {
		for _, private := range []bool{false, true} {
			t.Run(command+map[bool]string{true: "-private", false: "-group"}[private], func(t *testing.T) {
				service, _, _, qqAPI, _ := testService(t)
				runner := &fakeVendorRunner{}
				service.vendorRunner = runner
				imageQQ := &vendorImageQQ{fakeQQ: qqAPI}
				service.qq = imageQQ
				event := groupEvent("g", "unbound", command)
				if private {
					event = c2cEvent("unbound", command)
				}
				service.process(context.Background(), event)
				if len(runner.requests) != 1 || runner.requests[0].Operation != "query" || len(imageQQ.scenes) != 1 {
					t.Fatal(runner.requests, imageQQ.scenes, qqAPI.messages)
				}
				if imageQQ.replyTo[0] != event.Message.ID {
					t.Fatal(imageQQ.replyTo)
				}
				if imageQQ.sequences[0] <= 1 {
					t.Fatal("image reused initial progress sequence", imageQQ.sequences)
				}
			})
		}
	}
}

func TestVendorSubscriptionPermissionsStrictArgumentsAndIdempotency(t *testing.T) {
	service, storage, _, qqAPI, _ := testService(t)
	service.cfg.VendorStatus = vendorstatus.DefaultConfig()
	service.cfg.VendorStatus.GroupWhitelist = []string{"configured"}
	for _, event := range []qq.MessageEvent{
		groupEvent("g", "user", "/vendor_subscribe on"),
		c2cEvent("admin", "/vendor_subscribe on"),
		groupEvent("g", "admin", "/vendor_subscribe"),
		groupEvent("g", "admin", "/vendor_subscribe enable"),
		groupEvent("g", "admin", "/vendor_subscribe on extra"),
	} {
		if event.Message.GroupOpenID != "" && event.Message.Author.MemberOpenID == "admin" {
			service.cfg.QQAdminOpenIDs["member:g:admin"] = struct{}{}
		}
		service.process(context.Background(), event)
	}
	if groups, _ := storage.VendorSubscriptions(service.cfg.VendorStatus.GroupWhitelist); !reflect.DeepEqual(groups, []string{"configured"}) {
		t.Fatal("invalid commands changed subscriptions", groups)
	}
	service.process(context.Background(), groupEvent("g", "admin", "/vendor_subscribe on"))
	service.process(context.Background(), groupEvent("g", "admin", "/vendor_subscribe on"))
	if got := qqAPI.messages[len(qqAPI.messages)-1]; !strings.Contains(got, "无需重复") {
		t.Fatal(got)
	}
	if groups, _ := storage.VendorSubscriptions(service.cfg.VendorStatus.GroupWhitelist); !reflect.DeepEqual(groups, []string{"configured", "g"}) {
		t.Fatal(groups)
	}
	service.process(context.Background(), groupEvent("g", "admin", "/vendor_subscribe off"))
	service.process(context.Background(), groupEvent("g", "admin", "/vendor_subscribe off"))
	if got := qqAPI.messages[len(qqAPI.messages)-1]; !strings.Contains(got, "无需关闭") {
		t.Fatal(got)
	}
}

func TestVendorReadOnlyAndDisabledCommandRulesApply(t *testing.T) {
	service, storage, _, qqAPI, _ := testService(t)
	service.cfg.QQReadOnlyAdminOpenIDs = map[string]struct{}{"member:g:readonly": {}}
	service.process(context.Background(), groupEvent("g", "readonly", "/vendor_subscribe on"))
	if groups, _ := storage.VendorSubscriptions(nil); len(groups) != 0 {
		t.Fatal(groups)
	}
	runner := &fakeVendorRunner{}
	service.vendorRunner = runner
	service.cfg.QQAdminOpenIDs["member:g:admin"] = struct{}{}
	service.process(context.Background(), groupEvent("g", "admin", `/disable "vendor_status"`))
	before := len(qqAPI.messages)
	service.process(context.Background(), groupEvent("g", "user", "/vendor_status"))
	if len(runner.requests) != 0 || len(qqAPI.messages) != before {
		t.Fatal("disabled status command was not silently ignored")
	}
	if readOnlyAdminWriteCommand("/vendor_status", []string{"/vendor_status"}) {
		t.Fatal("read-only admin cannot query")
	}
}

func TestVendorDeliveryRechecksUnsubscribe(t *testing.T) {
	service, storage, _, qqAPI, _ := testService(t)
	service.cfg.VendorStatus = vendorstatus.DefaultConfig()
	if _, _, err := storage.SetVendorSubscription("g", true, nil); err != nil {
		t.Fatal(err)
	}
	service.vendorRunner = &fakeVendorRunner{run: func(ctx context.Context, request vendorstatus.Request, callbacks vendorstatus.Callbacks) (vendorstatus.Packet, error) {
		if !reflect.DeepEqual(request.Config.GroupWhitelist, []string{"g"}) {
			t.Fatal(request.Config.GroupWhitelist)
		}
		if _, _, err := storage.SetVendorSubscription("g", false, nil); err != nil {
			t.Fatal(err)
		}
		if err := callbacks.Send(ctx, vendorstatus.Packet{Group: "g", Text: "must not send"}); err == nil {
			t.Fatal("unsubscribed group was sent a message")
		}
		return vendorstatus.Packet{}, nil
	}}
	if _, err := service.runVendorStatus(context.Background(), "cycle"); err != nil {
		t.Fatal(err)
	}
	if len(qqAPI.messages) != 0 {
		t.Fatal(qqAPI.messages)
	}
}

func TestVendorWorkerCancellationAndSemaphoreDeadline(t *testing.T) {
	service, _, _, _, _ := testService(t)
	service.cfg.VendorStatus = vendorstatus.DefaultConfig()
	started := make(chan struct{})
	stopped := make(chan struct{})
	service.vendorRunner = &fakeVendorRunner{run: func(ctx context.Context, _ vendorstatus.Request, _ vendorstatus.Callbacks) (vendorstatus.Packet, error) {
		close(started)
		<-ctx.Done()
		return vendorstatus.Packet{}, ctx.Err()
	}}
	go func() {
		service.runVendorStatusWorker(context.Background())
		close(stopped)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := service.runVendorStatus(ctx, "query"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	close(service.notifyStop)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel the running Python worker")
	}
}
