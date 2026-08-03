package bot

import (
	"bytes"
	"context"
	"image/png"
	"testing"
	"time"

	"github.com/fsykk/new-api-bot/internal/model"
	"github.com/fsykk/new-api-bot/internal/newapi"
	"github.com/fsykk/new-api-bot/internal/qq"
)

type chartTestQQ struct {
	*fakeQQ
	files [][]byte
}

func (q *chartTestQQ) SendGroupFile(_ context.Context, _, _, _ string, _ int, data []byte) (qq.SentMessage, error) {
	q.files = append(q.files, append([]byte(nil), data...))
	return qq.SentMessage{ID: "chart-file"}, nil
}

func TestRenderUsageChartProducesLabeledChartCanvas(t *testing.T) {
	location := time.FixedZone("UTC+8", 8*60*60)
	end := time.Date(2026, 8, 3, 12, 0, 0, 0, location)
	data, err := renderUsageChart([]newapi.UsageRecord{
		{ModelName: "gpt-test", CreatedAt: end.Add(-6 * 24 * time.Hour).Unix(), Quota: 500000, Count: 2, TokenUsed: 128},
		{ModelName: "claude-test", CreatedAt: end.Add(-2 * 24 * time.Hour).Unix(), Quota: 1500000, Count: 4, TokenUsed: 512},
	}, end.Add(-7*24*time.Hour), end, location, 500000)
	if err != nil {
		t.Fatalf("render chart: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode chart: %v", err)
	}
	if img.Bounds().Dx() != 1280 || img.Bounds().Dy() != 820 {
		t.Fatalf("unexpected chart dimensions: %v", img.Bounds())
	}
	// The header, axis/grid and model-share legend must all write visible pixels.
	if got := img.At(54, 58); got == nil || got == img.At(0, 0) {
		t.Fatal("expected chart header to be rendered")
	}
	if got := img.At(56, 688); got == img.At(0, 0) {
		t.Fatal("expected model-share legend to be rendered")
	}
}

func TestUsageChartSupportsSpecificUserAndGroupAll(t *testing.T) {
	service, storage, api, baseQQ, _ := testService(t)
	chartQQ := &chartTestQQ{fakeQQ: baseQQ}
	service.qq = chartQQ
	service.cfg.QQAdminOpenIDs["member:g1:admin"] = struct{}{}
	now := time.Now()
	for _, binding := range []model.Binding{
		{CanonicalID: "member:g1:admin", NewAPIID: 42, CreatedAt: now},
		{CanonicalID: "member:g1:member", NewAPIID: 43, CreatedAt: now},
	} {
		if err := storage.CreateBinding(binding); err != nil {
			t.Fatal(err)
		}
	}
	api.user = newapi.User{ID: 42, Username: "alice"}
	api.users = []newapi.User{api.user, {ID: 43, Username: "bob"}, {ID: 44, Username: "outside"}}
	api.usageByModel = []newapi.UsageRecord{
		{UserID: 42, Username: "alice", ModelName: "gpt-test", CreatedAt: now.Unix(), Quota: 500000},
		{UserID: 43, Username: "bob", ModelName: "claude-test", CreatedAt: now.Unix(), Quota: 1000000},
		{UserID: 44, Username: "outside", ModelName: "other", CreatedAt: now.Unix(), Quota: 9000000},
	}
	groupRecords, err := service.listGroupUsageChartRecords(context.Background(), []model.Binding{
		{CanonicalID: "member:g1:admin", NewAPIID: 42},
		{CanonicalID: "member:g1:member", NewAPIID: 43},
	}, now.Add(-7*24*time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	var groupQuota int64
	for _, record := range groupRecords {
		groupQuota += record.Quota
	}
	if len(groupRecords) != 2 || groupQuota != 1500000 {
		t.Fatalf("unexpected group chart records: %#v", groupRecords)
	}
	event := groupEvent("g1", "admin", "/usage chart 7d all")
	identity := model.QQIdentity{MemberOpenID: "admin", GroupOpenID: "g1"}
	if err := service.handleUsageChart(context.Background(), event, "member:g1:admin", identity, "7d", "all"); err != nil {
		t.Fatal(err)
	}
	if len(chartQQ.files) != 1 || !bytes.HasPrefix(chartQQ.files[0], []byte("\x89PNG")) {
		t.Fatal("expected a group summary PNG file")
	}
	if reply := lastReply(t, baseQQ); !bytes.Contains([]byte(reply), []byte("本群已绑定成员（2 人）")) {
		t.Fatalf("unexpected group summary reply: %q", reply)
	}
	if err := service.handleUsageChart(context.Background(), event, "member:g1:admin", identity, "7d", "43"); err != nil {
		t.Fatal(err)
	}
	if len(chartQQ.files) != 2 {
		t.Fatal("expected a user chart PNG file")
	}
	if reply := lastReply(t, baseQQ); !bytes.Contains([]byte(reply), []byte("指定目标（New API 用户 43）")) {
		t.Fatalf("unexpected target chart reply: %q", reply)
	}
}
