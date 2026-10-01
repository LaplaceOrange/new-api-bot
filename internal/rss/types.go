// Package rss implements bounded feed collection without external processes.
package rss

import "time"

const (
	MaxBodyBytes = 2 << 20
	MaxItems     = 10000
)

type Article struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Summary     string    `json:"summary,omitempty"`
	URL         string    `json:"url,omitempty"`
	PublishedAt time.Time `json:"published_at,omitempty"`
}

type Feed struct {
	Title    string    `json:"title"`
	Articles []Article `json:"articles"`
}

type Validators struct {
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
}

type Result struct {
	Feed
	Validators
	NotModified bool
}

type Subscription struct {
	ID            string    `json:"id"`
	Group         string    `json:"group"`
	URL           string    `json:"url"`
	Title         string    `json:"title"`
	Enabled       bool      `json:"enabled"`
	Version       uint64    `json:"version"`
	CreatedAt     time.Time `json:"created_at"`
	LastChecked   time.Time `json:"last_checked"`
	LastError     string    `json:"last_error,omitempty"`
	DeliveryError string    `json:"delivery_error,omitempty"`
	Validators
}

type GroupSettings struct {
	Interval  time.Duration `json:"interval"`
	LastCycle time.Time     `json:"last_cycle"`
	LastSent  time.Time     `json:"last_sent"`
}

type Delivery struct {
	QueueID      string `json:"queue_id"`
	Subscription string `json:"subscription"`
	Group        string `json:"group"`
	Source       string `json:"source"`
	Version      uint64 `json:"version"`
	Article
}
