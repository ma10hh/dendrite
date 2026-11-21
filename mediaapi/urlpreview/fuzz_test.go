package urlpreview

import (
	"net/url"
	"strings"
	"testing"
)

// FuzzParseHTML reads pages that anybody on the internet can serve, as a user asks for a preview of them.
func FuzzParseHTML(f *testing.F) {
	f.Add(`<html><head><title>T</title><meta property="og:image" content="/i.png"><meta name="description" content="d"></head>`, "text/html; charset=utf-8")
	f.Add(`<meta property="og:title" content="`+strings.Repeat("x", 5000)+`">`, "text/html")
	f.Add("<meta charset=\"shift_jis\"><title>\x82\xa0", "text/html; charset=bogus")
	f.Add(`<title><title><meta property=og:image content=//a:b@c/>`, "")
	f.Add("\x00\xff<", "image/png")
	base, _ := url.Parse("https://example.com/a/b")
	f.Fuzz(func(t *testing.T, page, contentType string) {
		for key, value := range ParseHTML(strings.NewReader(page), contentType, base) {
			if len(value) > 4*maxFieldLength {
				t.Fatalf("%s is %d bytes long, a page must not fill the response", key, len(value))
			}
		}
	})
}

func FuzzValidateURL(f *testing.F) {
	for _, s := range []string{"https://example.com/", "http://user:pw@example.com", "ftp://x", "https:///a", "http://[::1]:80/", "https://exa mple.com", "//x", "https://%zz"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		u, err := ValidateURL(raw)
		if err != nil {
			return
		}
		if u.Hostname() == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
			t.Fatalf("ValidateURL(%q) accepted %v", raw, u)
		}
	})
}
