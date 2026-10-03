package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

func safeSourceURL(raw string) bool {
	u, e := url.Parse(raw)
	return len(raw) <= 2048 && e == nil && u.Host != "" && u.User == nil && (u.Scheme == "http" || u.Scheme == "https")
}
func normalizeSources(sources []Source) []Source {
	var out []Source
	seen := map[string]bool{}
	for _, s := range sources {
		if !safeSourceURL(s.URL) || seen[s.URL] {
			continue
		}
		seen[s.URL] = true
		s.Title = clip(s.Title, 200)
		s.Snippet = clip(s.Snippet, 1200)
		out = append(out, s)
		if len(out) == 5 {
			break
		}
	}
	return out
}

func (c *Client) Search(parent context.Context, cfg Config, query string) (SearchResult, error) {
	query = strings.TrimSpace(query)
	if query == "" || len(query) > 2048 {
		return SearchResult{}, errors.New("搜索词为空或过长")
	}
	ctx, cancel := toolContext(parent, cfg)
	defer cancel()
	var data []byte
	var err error
	var sources []Source
	switch cfg.SearchBackend {
	case "off":
		return SearchResult{}, errors.New("联网搜索未配置")
	}
	switch cfg.SearchBackend {
	case "tavily":
		if cfg.TavilyURL == "" || cfg.TavilyKey == "" {
			return SearchResult{}, errors.New("Tavily 未配置")
		}
		data, err = c.request(ctx, http.MethodPost, cfg.TavilyURL, cfg.TavilyKey, map[string]any{"query": query, "max_results": 5, "search_depth": "basic", "include_raw_content": false, "include_answer": false})
		if err == nil {
			var r struct {
				Results []struct {
					Title   string `json:"title"`
					URL     string `json:"url"`
					Content string `json:"content"`
				} `json:"results"`
			}
			if json.Unmarshal(data, &r) != nil {
				return SearchResult{}, errors.New("Tavily 响应格式无效")
			}
			for _, s := range r.Results {
				sources = append(sources, Source{Title: s.Title, URL: s.URL, Snippet: s.Content})
			}
		}
	case "searxng":
		if cfg.SearXNGURL == "" {
			return SearchResult{}, errors.New("SearXNG 未配置")
		}
		q := url.Values{"q": {query}, "format": {"json"}}
		data, err = c.request(ctx, http.MethodGet, strings.TrimRight(cfg.SearXNGURL, "/")+"/search?"+q.Encode(), "", nil)
		if err == nil {
			var r struct {
				Results []struct {
					Title   string `json:"title"`
					URL     string `json:"url"`
					Content string `json:"content"`
				} `json:"results"`
			}
			if json.Unmarshal(data, &r) != nil {
				return SearchResult{}, errors.New("SearXNG 响应格式无效（实例须开启 JSON）")
			}
			for _, s := range r.Results {
				sources = append(sources, Source{Title: s.Title, URL: s.URL, Snippet: s.Content})
			}
		}
	case "bing_serpapi":
		if cfg.BingURL == "" || cfg.BingKey == "" {
			return SearchResult{}, errors.New("SerpApi Bing 未配置")
		}
		q := url.Values{"engine": {"bing"}, "q": {query}, "api_key": {cfg.BingKey}, "output": {"json"}}
		data, err = c.request(ctx, http.MethodGet, cfg.BingURL+"?"+q.Encode(), "", nil)
		if err == nil {
			var r struct {
				Error   string `json:"error"`
				Results []struct {
					Title   string `json:"title"`
					Link    string `json:"link"`
					Snippet string `json:"snippet"`
				} `json:"organic_results"`
			}
			if json.Unmarshal(data, &r) != nil || r.Error != "" {
				return SearchResult{}, errors.New("SerpApi Bing 搜索失败")
			}
			for _, s := range r.Results {
				sources = append(sources, Source{Title: s.Title, URL: s.Link, Snippet: s.Snippet})
			}
		}
	case "model_native":
		if cfg.SearchModelURL == "" || cfg.SearchModelKey == "" || cfg.SearchModel == "" {
			return SearchResult{}, errors.New("原生搜索模型未配置")
		}
		// Never forward business results or the conversation to this provider.
		data, err = c.request(ctx, http.MethodPost, strings.TrimRight(cfg.SearchModelURL, "/")+"/chat/completions", cfg.SearchModelKey, map[string]any{
			"model": cfg.SearchModel, "messages": []Message{{Role: "user", Content: query}}, "stream": false, "web_search_options": map[string]any{},
		})
		if err == nil {
			r, e := decodeCompletion(data)
			if e != nil {
				return SearchResult{}, e
			}
			sources = normalizeSources(r.Sources)
			if len(sources) == 0 {
				return SearchResult{}, errors.New("原生搜索模型未返回来源，无法验证联网结果")
			}
			return SearchResult{Sources: sources, Summary: clip(r.Message.Content, 3000)}, nil
		}
	default:
		return SearchResult{}, errors.New("未知搜索后端")
	}
	if err != nil {
		return SearchResult{}, err
	}
	return SearchResult{Sources: normalizeSources(sources)}, nil
}
