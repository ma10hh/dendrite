// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package urlpreview

import (
	"io"
	"net/url"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

// maxFieldLength bounds the text taken from a page, so a page cannot fill the response.
const maxFieldLength = 2048

// ParseHTML reads the head of a page and returns its Open Graph tags (as "og:..."), with the title and the
// description of the page for pages that have no tags for them. Relative URLs are resolved against base.
func ParseHTML(body io.Reader, contentType string, base *url.URL) map[string]string {
	reader, err := charset.NewReader(body, contentType)
	if err != nil {
		reader = body
	}
	tokens := html.NewTokenizer(reader)
	og := map[string]string{}
	var title, description, imageSrc string
	inTitle := false

loop:
	for {
		switch tokens.Next() {
		case html.ErrorToken:
			break loop
		case html.EndTagToken:
			name, _ := tokens.TagName()
			switch string(name) {
			case "title":
				inTitle = false
			case "head":
				break loop
			}
		case html.TextToken:
			if inTitle && title == "" {
				title = clean(string(tokens.Text()))
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := tokens.TagName()
			switch string(name) {
			case "body":
				break loop
			case "title":
				inTitle = true
			case "meta":
				attrs := readAttributes(tokens, hasAttr)
				key := attrs["property"]
				if key == "" {
					key = attrs["name"]
				}
				key, content := strings.ToLower(key), attrs["content"]
				switch {
				case strings.HasPrefix(key, "og:"):
					if _, seen := og[key]; !seen && content != "" {
						og[key] = clean(content)
					}
				case key == "description" && description == "":
					description = clean(content)
				}
			case "link":
				attrs := readAttributes(tokens, hasAttr)
				if strings.EqualFold(attrs["rel"], "image_src") && imageSrc == "" {
					imageSrc = attrs["href"]
				}
			}
		}
	}

	if og["og:title"] == "" && title != "" {
		og["og:title"] = title
	}
	if og["og:description"] == "" && description != "" {
		og["og:description"] = description
	}
	if og["og:image"] == "" && imageSrc != "" {
		og["og:image"] = imageSrc
	}
	// URLs of the page may be relative to it. Only http and https are followed, anything else is not a link.
	for _, key := range []string{"og:image", "og:image:url", "og:image:secure_url"} {
		if raw := og[key]; raw != "" {
			if abs := resolve(base, raw); abs != "" {
				og[key] = abs
			} else {
				delete(og, key)
			}
		}
	}
	return og
}

func readAttributes(tokens *html.Tokenizer, hasAttr bool) map[string]string {
	attrs := map[string]string{}
	for hasAttr {
		var key, value []byte
		key, value, hasAttr = tokens.TagAttr()
		name := strings.ToLower(string(key))
		if _, seen := attrs[name]; !seen {
			attrs[name] = string(value)
		}
	}
	return attrs
}

func resolve(base *url.URL, raw string) string {
	ref, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	abs := base.ResolveReference(ref)
	if abs.Scheme != "http" && abs.Scheme != "https" || abs.Host == "" {
		return ""
	}
	return abs.String()
}

// clean collapses whitespace and cuts long text.
func clean(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxFieldLength {
		s = strings.ToValidUTF8(s[:maxFieldLength], "")
	}
	return s
}
