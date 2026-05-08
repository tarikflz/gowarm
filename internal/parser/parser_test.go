package parser

import (
	"context"
	"strings"
	"testing"
	"time"
)

const sitemapXML = `<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <url><loc>https://example.com/en</loc></url>
  <url><loc>https://example.com/fr</loc></url>
  <url><loc>https://example.com/en/phones</loc></url>
</urlset>`

const sitemapIndexXML = `<?xml version="1.0" encoding="UTF-8"?>
<sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <sitemap><loc>https://example.com/sitemap-1.xml</loc></sitemap>
  <sitemap><loc>https://example.com/sitemap-2.xml</loc></sitemap>
</sitemapindex>`

func TestStreamingXMLParser_Parse_Urlset(t *testing.T) {
	t.Parallel()
	p := NewStreamingXMLParser()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ch, err := p.Parse(ctx, strings.NewReader(sitemapXML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var got []string
	for u := range ch {
		got = append(got, u)
	}
	want := []string{
		"https://example.com/en",
		"https://example.com/fr",
		"https://example.com/en/phones",
	}
	if len(got) != len(want) {
		t.Fatalf("len: got %d want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] got %q want %q", i, got[i], want[i])
		}
	}
}

func TestStreamingXMLParser_Parse_SitemapIndex(t *testing.T) {
	t.Parallel()
	p := NewStreamingXMLParser()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ch, err := p.Parse(ctx, strings.NewReader(sitemapIndexXML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var got []string
	for u := range ch {
		got = append(got, u)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(got))
	}
}

func TestStreamingXMLParser_Parse_RespectsContextCancel(t *testing.T) {
	t.Parallel()
	p := NewStreamingXMLParser()
	p.BufferSize = 1

	// Build a large XML body so the goroutine actually has tokens to stream.
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`)
	for i := 0; i < 5000; i++ {
		b.WriteString("<url><loc>https://example.com/")
		b.WriteString("path")
		b.WriteString("</loc></url>")
	}
	b.WriteString(`</urlset>`)

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := p.Parse(ctx, strings.NewReader(b.String()))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// pull a couple, then cancel
	<-ch
	<-ch
	cancel()
	// drain — channel must close eventually
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return // channel closed, success
			}
		case <-deadline:
			t.Fatal("channel did not close after cancel")
		}
	}
}

func TestIsIndex(t *testing.T) {
	t.Parallel()
	if !IsIndex([]byte("<sitemapindex xmlns")) {
		t.Error("expected true for sitemapindex")
	}
	if IsIndex([]byte("<urlset xmlns")) {
		t.Error("expected false for urlset")
	}
}
