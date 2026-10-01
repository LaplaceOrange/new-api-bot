package rss

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestParseFeedFormats(t *testing.T) {
	cases := []struct {
		name, body, title, summary, link string
	}{
		{"RSS2", `<rss version="2.0"><channel><title>News</title><item><guid>a</guid><title>First</title><description><![CDATA[<p>Hello &amp; world</p><script>secret</script><style>bad</style>]]></description><link>/post?id=7&amp;token=secret</link><pubDate>Thu, 01 Oct 2026 12:00:00 +0000</pubDate></item></channel></rss>`, "News", "Hello & world", "https://example.com/post?id=7"},
		{"RSS1", `<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#" xmlns="http://purl.org/rss/1.0/" xmlns:dc="http://purl.org/dc/elements/1.1/"><channel><title>Old</title></channel><item rdf:about="a"><title>First</title><description>Hello</description><link>/post</link><dc:date>2026-10-01T12:00:00Z</dc:date></item></rdf:RDF>`, "Old", "Hello", "https://example.com/post"},
		{"Atom", `<feed xmlns="http://www.w3.org/2005/Atom"><title>Atom</title><entry><id>a</id><title>First</title><summary type="xhtml"><div xmlns="http://www.w3.org/1999/xhtml"><p>Hello</p><script>secret</script></div></summary><link rel="self" href="/ignored"/><link rel="alternate" href="/post"/><published>2026-10-01T12:00:00Z</published></entry></feed>`, "Atom", "Hello", "https://example.com/post"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			feed, err := Parse([]byte(tc.body), "https://example.com/rss")
			if err != nil || feed.Title != tc.title || len(feed.Articles) != 1 {
				t.Fatalf("%+v %v", feed, err)
			}
			article := feed.Articles[0]
			if article.Summary != tc.summary || article.URL != tc.link || article.PublishedAt.IsZero() || article.ID == "" {
				t.Fatalf("%+v", article)
			}
		})
	}
}

func TestIdentityMissingFieldsAndOrdering(t *testing.T) {
	body := `<rss><channel><item><guid>same</guid><title>Later</title><pubDate>2026-10-02T00:00:00Z</pubDate></item>
<item><guid>same</guid><title>Changed</title></item>
<item><link>/post</link><pubDate>2026-10-01T00:00:00Z</pubDate></item>
<item><title>No link</title></item></channel></rss>`
	feed, err := Parse([]byte(body), "https://example.com/rss")
	if err != nil || len(feed.Articles) != 3 || feed.Title != "RSS" {
		t.Fatal(feed, err)
	}
	if feed.Articles[0].Title != "（无标题）" || feed.Articles[2].Title != "No link" {
		t.Fatal(feed.Articles)
	}
	changed, err := Parse([]byte(strings.ReplaceAll(body, "Later", "Different title")), "https://example.com/rss")
	if err != nil || changed.Articles[1].ID != feed.Articles[1].ID {
		t.Fatal("GUID identity changed with title", changed, err)
	}
}

func TestParseRejectsInvalidAndBoundedInputs(t *testing.T) {
	for _, body := range []string{"", "<html/>", "<rss/>", "<rss><channel>", "<rss><channel/></rss><rss/>", "<!DOCTYPE rss [<!ENTITY x SYSTEM 'file:///secret'>]><rss><channel><title>&x;</title></channel></rss>", strings.Repeat("x", MaxBodyBytes+1), strings.Repeat("<x>", 70)} {
		if _, err := Parse([]byte(body), "https://example.com"); err == nil {
			t.Fatalf("accepted invalid body prefix %q", body[:min(len(body), 100)])
		}
	}
	tooMany := "<rss><channel>" + strings.Repeat("<item><title>x</title></item>", MaxItems+1) + "</channel></rss>"
	// Use unique IDs to exercise the article limit rather than deduplication.
	var builder strings.Builder
	builder.WriteString("<rss><channel>")
	for i := 0; i <= MaxItems; i++ {
		builder.WriteString("<item><guid>")
		builder.WriteString(time.Unix(int64(i), 0).Format(time.RFC3339))
		builder.WriteString("</guid></item>")
	}
	builder.WriteString("</channel></rss>")
	if _, err := Parse([]byte(builder.String()), "https://example.com"); err == nil {
		t.Fatal("article bound not enforced")
	}
	if _, err := Parse([]byte(tooMany), "https://example.com"); err != nil {
		t.Fatal("duplicate entries should deduplicate below limit", err)
	}
}

func TestCleanTextAndLinkSafety(t *testing.T) {
	text := CleanText(`&lt;script&gt;secret&lt;/script&gt;<style>bad</style><p>A&nbsp; B</p><qqbot-at-user id="abc"/>&amp;`, 300)
	if text != "A B &" {
		t.Fatal(text)
	}
	if n := utf8.RuneCountInString(CleanText(strings.Repeat("中", 400), 300)); n != 301 {
		t.Fatal(n)
	}
	if got := resolveLink("https://example.com", "javascript:alert(1)"); got != "" {
		t.Fatal(got)
	}
	if got := resolveLink("https://example.com", "https://user:pass@site/post?id=9&api_key=secret#secret"); got != "https://site/post?id=9" {
		t.Fatal(got)
	}
	if !parseTime("Thu, 1 Oct 2026 12:00:00 +0000").Equal(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)) {
		t.Fatal("single-digit RFC date unsupported")
	}
}
