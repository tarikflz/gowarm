// Package parser streams URLs out of XML sitemaps and sitemap-index files
// without loading the entire document into memory. The parser exposes results
// over a buffered channel so a downstream pipeline can begin warming long
// before the document is fully consumed.
package parser

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

// SitemapParser is the contract used by main.go to consume sitemap content.
type SitemapParser interface {
	// Parse streams URL strings from r. The returned channel is closed when
	// parsing finishes, the reader EOFs, or ctx is cancelled.
	Parse(ctx context.Context, r io.Reader) (<-chan string, error)
}

// Result is the output of Parse: either a discovered <loc> URL or a nested
// sitemap index entry. Callers that want to honour <sitemapindex> can fetch
// the IndexURLs themselves and feed them back through Parse.
type Result struct {
	URLs      []string
	IndexURLs []string
	IsIndex   bool
}

// StreamingXMLParser implements SitemapParser using encoding/xml token streaming.
type StreamingXMLParser struct {
	BufferSize int
}

// NewStreamingXMLParser returns a parser with a sensible channel buffer size.
func NewStreamingXMLParser() *StreamingXMLParser {
	return &StreamingXMLParser{BufferSize: 1024}
}

// Parse consumes r as either a <urlset> sitemap or a <sitemapindex>. URLs from
// either container are sent to the returned channel. The caller is expected to
// recursively re-Parse nested sitemap indexes if FollowIndex is desired.
func (p *StreamingXMLParser) Parse(ctx context.Context, r io.Reader) (<-chan string, error) {
	out := make(chan string, p.BufferSize)
	if r == nil {
		close(out)
		return out, fmt.Errorf("nil reader")
	}

	go func() {
		defer close(out)

		dec := xml.NewDecoder(r)
		dec.Strict = false // tolerate sloppy sitemaps
		dec.AutoClose = xml.HTMLAutoClose

		for {
			if ctx.Err() != nil {
				return
			}
			tok, err := dec.Token()
			if err == io.EOF {
				return
			}
			if err != nil {
				// Malformed XML is logged-by-omission; we silently stop so the
				// caller's channel range loop terminates. Production code may
				// want to surface this via a second error channel.
				return
			}
			start, ok := tok.(xml.StartElement)
			if !ok {
				continue
			}
			switch strings.ToLower(start.Name.Local) {
			case "loc":
				var v string
				if err := dec.DecodeElement(&v, &start); err != nil {
					continue
				}
				v = strings.TrimSpace(v)
				if v == "" {
					continue
				}
				select {
				case out <- v:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return out, nil
}

// IsIndex peeks the head of an XML payload (already buffered into b) and
// reports whether it looks like a <sitemapindex>. This lets main.go decide
// whether to recurse before draining the URL channel.
func IsIndex(headBytes []byte) bool {
	s := strings.ToLower(string(headBytes))
	return strings.Contains(s, "<sitemapindex")
}
