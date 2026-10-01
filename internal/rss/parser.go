package rss

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"html"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

var (
	scriptPattern = regexp.MustCompile(`(?is)<(script|style)\b[^>]*>.*?</(?:script|style)\s*>`)
	tagPattern    = regexp.MustCompile(`(?s)<[^>]*>`)
)

// CleanText also neutralizes QQ's inline XML-like mention protocol.
func CleanText(text string, limit int) string {
	text = html.UnescapeString(text)
	text = scriptPattern.ReplaceAllString(text, " ")
	text = tagPattern.ReplaceAllString(text, " ")
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, text)
	text = strings.Join(strings.Fields(text), " ")
	runes := []rune(text)
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return text
}

type xmlNode struct {
	name     xml.Name
	attrs    []xml.Attr
	text     string
	children []*xmlNode
}

func readNode(decoder *xml.Decoder, start xml.StartElement, depth int, count *int) (*xmlNode, error) {
	(*count)++
	if depth > 64 || *count > 100000 {
		return nil, errors.New("RSS XML 结构过于复杂")
	}
	node := &xmlNode{name: start.Name, attrs: start.Attr}
	var text strings.Builder
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, errors.New("RSS XML 格式无效")
		}
		switch token := token.(type) {
		case xml.StartElement:
			child, err := readNode(decoder, token, depth+1, count)
			if err != nil {
				return nil, err
			}
			node.children = append(node.children, child)
			switch strings.ToLower(node.name.Local) {
			case "rss", "rdf", "channel", "feed", "item", "entry":
				// Structural containers do not need an additional copy of
				// every article's text.
			default:
				if !strings.EqualFold(child.name.Local, "script") && !strings.EqualFold(child.name.Local, "style") {
					text.WriteString(" " + child.text + " ")
				}
			}
		case xml.CharData:
			text.Write(token)
		case xml.EndElement:
			node.text = text.String()
			return node, nil
		}
	}
}

func (n *xmlNode) child(name string) *xmlNode {
	for _, child := range n.children {
		if strings.EqualFold(child.name.Local, name) {
			return child
		}
	}
	return nil
}

func (n *xmlNode) value(names ...string) string {
	for _, name := range names {
		if child := n.child(name); child != nil && strings.TrimSpace(child.text) != "" {
			return strings.TrimSpace(child.text)
		}
	}
	return ""
}

func (n *xmlNode) attr(name string) string {
	for _, attr := range n.attrs {
		if attr.Name.Local == name {
			return attr.Value
		}
	}
	return ""
}

func Parse(body []byte, baseURL string) (Feed, error) {
	if len(body) > MaxBodyBytes {
		return Feed{}, errors.New("RSS 响应超过 2 MiB")
	}
	decoder := xml.NewDecoder(bytes.NewReader(body))
	var root *xmlNode
	count := 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Feed{}, errors.New("RSS XML 格式无效")
		}
		if start, ok := token.(xml.StartElement); ok {
			if root != nil {
				return Feed{}, errors.New("RSS XML 包含多个根元素")
			}
			root, err = readNode(decoder, start, 0, &count)
			if err != nil {
				return Feed{}, err
			}
		} else if text, ok := token.(xml.CharData); ok && strings.TrimSpace(string(text)) != "" {
			return Feed{}, errors.New("RSS XML 格式无效")
		}
	}
	if root == nil {
		return Feed{}, errors.New("RSS 响应为空")
	}
	var container *xmlNode
	var entries []*xmlNode
	switch strings.ToLower(root.name.Local) {
	case "rss":
		container = root.child("channel")
		if container == nil {
			return Feed{}, errors.New("RSS 缺少 channel")
		}
		entries = container.children
	case "rdf":
		container = root.child("channel")
		if container == nil {
			return Feed{}, errors.New("RSS 1.0 缺少 channel")
		}
		entries = root.children
	case "feed":
		container = root
		entries = root.children
	default:
		return Feed{}, errors.New("地址未返回 RSS/Atom 订阅源")
	}
	feed := Feed{Title: CleanText(container.value("title"), 200)}
	if feed.Title == "" {
		feed.Title = "RSS"
	}
	identities := map[string]bool{}
	for _, entry := range entries {
		if entry.name.Local != "item" && entry.name.Local != "entry" {
			continue
		}
		if len(feed.Articles) >= MaxItems {
			return Feed{}, errors.New("RSS 文章数量超过上限")
		}
		link := entry.value("link")
		for _, child := range entry.children {
			if child.name.Local == "link" && child.attr("href") != "" &&
				(child.attr("rel") == "" || child.attr("rel") == "alternate") {
				link = child.attr("href")
				break
			}
		}
		rawTitle := entry.value("title")
		published := entry.value("pubDate", "published", "date", "updated")
		identity := entry.value("guid", "id")
		if identity == "" {
			identity = entry.attr("about")
		}
		if identity == "" {
			identity = resolveLink(baseURL, link)
		}
		if identity == "" {
			identity = rawTitle + "\x00" + published
		}
		hash := sha256.Sum256([]byte(identity))
		id := hex.EncodeToString(hash[:])
		if identities[id] {
			continue
		}
		identities[id] = true
		title := CleanText(rawTitle, 200)
		if title == "" {
			title = "（无标题）"
		}
		feed.Articles = append(feed.Articles, Article{
			ID: id, Title: title,
			Summary: CleanText(entry.value("description", "summary", "encoded", "content"), 300),
			URL:     resolveLink(baseURL, link), PublishedAt: parseTime(published),
		})
	}
	sort.SliceStable(feed.Articles, func(i, j int) bool {
		a, b := feed.Articles[i].PublishedAt, feed.Articles[j].PublishedAt
		if a.IsZero() != b.IsZero() {
			return !a.IsZero()
		}
		return a.Before(b)
	})
	return feed, nil
}

func parseTime(value string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, time.RFC1123Z, time.RFC1123, time.RFC822Z, time.RFC822, time.RFC850, time.ANSIC, "Mon, 2 Jan 2006 15:04:05 -0700", "2006-01-02"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

func resolveLink(base, value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	ref, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return ""
	}
	if baseURL, err := url.Parse(base); err == nil {
		ref = baseURL.ResolveReference(ref)
	}
	if (ref.Scheme != "http" && ref.Scheme != "https") || ref.Hostname() == "" {
		return ""
	}
	// Article links must not accidentally disclose embedded credentials.
	ref.User = nil
	query := ref.Query()
	for key := range query {
		lower := strings.ToLower(key)
		if lower == "key" || lower == "sig" || lower == "passwd" {
			query.Del(key)
			continue
		}
		for _, secret := range []string{"token", "secret", "password", "signature", "credential", "api_key", "apikey", "authorization", "auth", "access_key"} {
			if strings.Contains(lower, secret) {
				query.Del(key)
				break
			}
		}
	}
	ref.RawQuery = query.Encode()
	ref.Fragment = ""
	return ref.String()
}
