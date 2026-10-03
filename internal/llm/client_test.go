package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCompletionProtocol(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("wrong endpoint or authorization")
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("invalid body")
		}
		if body["stream"] != false || body["max_tokens"] != float64(2000) || body["tool_choice"] != "auto" || body["parallel_tool_calls"] != false {
			t.Errorf("body = %#v", body)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"account","arguments":"{}"}}]}}],"usage":{"prompt_tokens":10,"completion_tokens":3}}`))
	}))
	defer server.Close()
	client := NewClient()
	defer client.Close()
	cfg := DefaultConfig()
	cfg.BaseURL = server.URL + "/v1/"
	cfg.APIKey = "secret"
	cfg.Model = "configured-model"
	got, err := client.Complete(context.Background(), cfg, []Message{{Role: "user", Content: "hello"}}, []Tool{{Type: "function", Function: Function{Name: "account"}}})
	if err != nil || len(got.Message.ToolCalls) != 1 || got.PromptTokens != 10 || got.CompletionTokens != 3 {
		t.Fatal(got, err)
	}
}
func TestCompletionFailuresAreBoundedAndSanitized(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"auth", 401, "secret diagnostic"}, {"rate", 429, "secret"}, {"invalid", 200, `not json`},
		{"empty", 200, `{"choices":[]}`}, {"blank", 200, `{"choices":[{"message":{"content":""}}]}`},
		{"tool_shape", 200, `{"choices":[{"message":{"tool_calls":[{"type":"function","id":"","function":{"arguments":"{}"}}]}}]}`},
		{"large", 200, strings.Repeat("x", maxBody+1)}, {"redirect", 302, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", "https://example.invalid/secret")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			client := NewClient()
			defer client.Close()
			cfg := DefaultConfig()
			cfg.BaseURL = server.URL
			cfg.APIKey = "secret"
			cfg.Model = "m"
			_, err := client.Complete(context.Background(), cfg, nil, nil)
			if err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatal(err)
			}
		})
	}
}
func TestCompletionCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(100 * time.Millisecond):
		}
	}))
	defer server.Close()
	client := NewClient()
	defer client.Close()
	cfg := DefaultConfig()
	cfg.BaseURL = server.URL
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.Complete(ctx, cfg, nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err = client.Complete(ctx, cfg, nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}
func TestSearchBackends(t *testing.T) {
	for _, backend := range []string{"tavily", "searxng", "bing_serpapi", "model_native"} {
		t.Run(backend, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch backend {
				case "tavily":
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					if r.Method != "POST" || r.URL.Path != "/search" || body["query"] != "a question" || body["include_raw_content"] != false || r.Header.Get("Authorization") != "Bearer only-search-secret" {
						t.Error(body, r.URL)
					}
					_, _ = w.Write([]byte(`{"results":[{"title":"result","url":"https://example.test/a","content":"snippet"},{"url":"file:///private"},{"url":"https://example.test/a"}]}`))
				case "searxng":
					if r.Method != "GET" || r.URL.Path != "/search" || r.URL.Query().Get("q") != "a question" || r.URL.Query().Get("format") != "json" {
						t.Error(r.URL)
					}
					_, _ = w.Write([]byte(`{"results":[{"title":"result","url":"https://example.test/a","content":"snippet"}]}`))
				case "bing_serpapi":
					if r.URL.Query().Get("engine") != "bing" || r.URL.Query().Get("api_key") != "only-search-secret" || r.URL.Query().Get("q") != "a question" {
						t.Error(r.URL)
					}
					_, _ = w.Write([]byte(`{"organic_results":[{"title":"result","link":"https://example.test/a","snippet":"snippet"}]}`))
				case "model_native":
					var body struct {
						Messages []Message      `json:"messages"`
						Options  map[string]any `json:"web_search_options"`
						Model    string         `json:"model"`
					}
					_ = json.NewDecoder(r.Body).Decode(&body)
					if len(body.Messages) != 1 || body.Messages[0].Content != "a question" || body.Options == nil || body.Model != "search-only" {
						t.Error(body)
					}
					_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"summary","annotations":[{"type":"url_citation","url_citation":{"url":"https://example.test/a","title":"result"}}]}}]}`))
				}
			}))
			defer server.Close()
			client := NewClient()
			defer client.Close()
			cfg := DefaultConfig()
			cfg.SearchBackend = backend
			cfg.TavilyURL = server.URL + "/search"
			cfg.TavilyKey = "only-search-secret"
			cfg.SearXNGURL = server.URL
			cfg.BingURL = server.URL + "/search"
			cfg.BingKey = "only-search-secret"
			cfg.SearchModelURL = server.URL + "/v1"
			cfg.SearchModelKey = "only-search-secret"
			cfg.SearchModel = "search-only"
			result, err := client.Search(context.Background(), cfg, "a question")
			if err != nil || len(result.Sources) != 1 || result.Sources[0].URL != "https://example.test/a" {
				t.Fatal(result, err)
			}
		})
	}
}
func TestSearchUnavailableAndInvalid(t *testing.T) {
	client := NewClient()
	defer client.Close()
	for _, backend := range []string{"off", "tavily", "searxng", "bing_serpapi", "model_native", "unknown"} {
		cfg := DefaultConfig()
		cfg.SearchBackend = backend
		if _, err := client.Search(context.Background(), cfg, "query"); err == nil {
			t.Fatalf("%s should be unavailable", backend)
		}
	}
	for _, tc := range []struct{ backend, body string }{
		{"model_native", `{"choices":[{"message":{"content":"claimed search without citations"}}]}`},
		{"bing_serpapi", `{"error":"secret error"}`}, {"tavily", `not json`}, {"searxng", `not json`},
	} {
		t.Run(tc.backend, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(tc.body)) }))
			defer server.Close()
			cfg := DefaultConfig()
			cfg.SearchBackend = tc.backend
			cfg.SearchModelURL = server.URL
			cfg.SearchModelKey = "secret"
			cfg.SearchModel = "m"
			cfg.BingURL = server.URL
			cfg.BingKey = "secret"
			cfg.TavilyURL = server.URL
			cfg.TavilyKey = "secret"
			cfg.SearXNGURL = server.URL
			_, err := client.Search(context.Background(), cfg, "query")
			if err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatal(err)
			}
		})
	}
}
