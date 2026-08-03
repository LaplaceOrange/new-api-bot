package bot

import (
	"bytes"
	"image/png"
	"testing"
	"time"

	"github.com/fsykk/new-api-bot/internal/newapi"
)

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
