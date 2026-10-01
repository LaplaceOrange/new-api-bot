package store

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/fsykk/new-api-bot/internal/model"
)

func TestHongbaoPersistsAcrossReopenAndIndexesPendingSummaries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hongbao.db")
	storage, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	packet := model.Hongbao{
		ID: "packet", GroupOpenID: "g", Actor: "admin", QuotaPerUnit: 500000,
		AllowedGroups: []string{"gpt-cheap", "gpt-smart"},
		TotalQuota:    500000, TotalCount: 2, RemainingQuota: 250000, RemainingCount: 1,
		Claims:    map[int]model.HongbaoClaim{42: {CanonicalID: "member:g:alice", RawQuota: 250000, Status: "pending_confirmation"}},
		CreatedAt: time.Now().UTC(),
	}
	if err := storage.PutHongbao(packet); err != nil {
		t.Fatal(err)
	}
	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}
	storage, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	got, err := storage.GetHongbao("g")
	if err != nil || !reflect.DeepEqual(got, packet) {
		t.Fatalf("packet = %+v, err = %v", got, err)
	}
	if _, err := storage.GetHongbao("other"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing packet: %v", err)
	}
	items, err := storage.ListPendingHongbaoSummaries()
	if err != nil || len(items) != 0 {
		t.Fatalf("pending summaries = %+v, err = %v", items, err)
	}
	packet.CompletedAt = packet.CreatedAt.Add(time.Second)
	packet.RemainingCount = 0
	packet.RemainingQuota = 0
	packet.GrantedCount = 2
	packet.Claims = map[int]model.HongbaoClaim{
		42: {CanonicalID: "member:g:alice", RawQuota: 250000, Status: "granted"},
		43: {CanonicalID: "member:g:bob", RawQuota: 250000, Status: "granted"},
	}
	if err := storage.PutHongbao(packet); err != nil {
		t.Fatal(err)
	}
	items, err = storage.ListPendingHongbaoSummaries()
	if err != nil || len(items) != 1 || items[0].ID != packet.ID {
		t.Fatalf("pending summaries = %+v, err = %v", items, err)
	}
	packet.SummarySent = true
	if err := storage.PutHongbao(packet); err != nil {
		t.Fatal(err)
	}
	items, err = storage.ListPendingHongbaoSummaries()
	if err != nil || len(items) != 0 {
		t.Fatalf("pending summaries = %+v, err = %v", items, err)
	}
}

func TestHongbaoRejectsInvalidState(t *testing.T) {
	storage := openTestStore(t)
	if err := storage.PutHongbao(model.Hongbao{}); err == nil {
		t.Fatal("empty packet accepted")
	}
	packet := model.Hongbao{
		ID: "packet", GroupOpenID: "g", QuotaPerUnit: 500000,
		TotalQuota: 500000, TotalCount: 1, RemainingQuota: -1,
	}
	if err := storage.PutHongbao(packet); err == nil {
		t.Fatal("negative remaining quota accepted")
	}
}
