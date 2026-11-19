package urlpreview

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGuardAllowed(t *testing.T) {
	allow, err := ParsePrefixes([]string{"10.1.2.0/24", "192.0.2.7"})
	require.NoError(t, err)
	deny, err := ParsePrefixes([]string{"93.184.216.0/24"})
	require.NoError(t, err)
	g := Guard{Allow: allow, Deny: deny}

	for addr, want := range map[string]bool{
		"93.184.215.14":          true,
		"2606:2800:220:1::1":     true,
		"127.0.0.1":              false,
		"10.0.0.1":               false,
		"10.1.2.3":               true, // allowed by the administrator
		"192.0.2.7":              true,
		"192.0.2.8":              false,
		"172.31.255.255":         false,
		"192.168.1.1":            false,
		"169.254.169.254":        false, // metadata services of clouds
		"100.64.0.1":             false,
		"0.0.0.0":                false,
		"255.255.255.255":        false,
		"224.0.0.1":              false,
		"93.184.216.34":          false, // denied by the administrator
		"::1":                    false,
		"::":                     false,
		"fe80::1":                false,
		"fd00::1":                false,
		"::ffff:127.0.0.1":       false, // IPv4 in IPv6
		"::ffff:8.8.8.8":         true,
		"64:ff9b::7f00:1":        false, // NAT64 of 127.0.0.1
		"64:ff9b::808:808":       true,  // NAT64 of 8.8.8.8
		"64:ff9b:1::1":           false,
		"2002:7f00:1::":          false, // 6to4 of 127.0.0.1
		"2002:808:808::":         true,
		"::7f00:1":               false, // IPv4 compatible
		"2001:4860:4860::8888":   true,
		"fe80::1%eth0":           false,
		"::ffff:10.1.2.3":        true,
		"2001:db8::1":            false,
		"ff02::1":                false,
		"::ffff:169.254.169.254": false,
	} {
		a, err := netip.ParseAddr(addr)
		require.NoError(t, err, addr)
		assert.Equal(t, want, g.Allowed(a), addr)
	}

	_, err = ParsePrefixes([]string{"not an address"})
	assert.Error(t, err)
}

func TestValidateURL(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://example.org/page?x=1":                     true,
		"http://example.org:8080/":                         true,
		"ftp://example.org/":                               false,
		"file:///etc/passwd":                               false,
		"javascript:alert(1)":                              false,
		"https:///nohost":                                  false,
		"https://user:pass@example.org/":                   false,
		"https://example.org/" + strings.Repeat("a", 2100): false,
		"::not a url":                                      false,
	} {
		_, err := ValidateURL(raw)
		assert.Equal(t, ok, err == nil, raw)
	}
}

func TestParseHTML(t *testing.T) {
	base, _ := url.Parse("https://example.org/dir/page.html")
	t.Run("open graph tags", func(t *testing.T) {
		og := ParseHTML(strings.NewReader(`<html><head>
			<title>Plain title</title>
			<meta property="og:title" content="  The   title ">
			<meta property="og:description" content="A description">
			<meta property="og:image" content="/img/cover.png">
			<meta property="og:site_name" content="Example">
			<meta property="og:title" content="not the first">
			</head><body><meta property="og:description" content="in the body"></body></html>`), "text/html", base)
		assert.Equal(t, map[string]string{
			"og:title":       "The title",
			"og:description": "A description",
			"og:image":       "https://example.org/img/cover.png",
			"og:site_name":   "Example",
		}, og)
	})
	t.Run("falls back to the page", func(t *testing.T) {
		og := ParseHTML(strings.NewReader(`<head><title>Page
			title</title><meta name="description" content="Plain description"><link rel="image_src" href="cover.jpg"></head>`), "text/html", base)
		assert.Equal(t, "Page title", og["og:title"])
		assert.Equal(t, "Plain description", og["og:description"])
		assert.Equal(t, "https://example.org/dir/cover.jpg", og["og:image"])
	})
	t.Run("images that are not web addresses are dropped", func(t *testing.T) {
		for _, src := range []string{"javascript:alert(1)", "data:image/png;base64,AAAA", "file:///etc/passwd", "ftp://example.org/a.png"} {
			og := ParseHTML(strings.NewReader(`<head><meta property="og:image" content="`+src+`"></head>`), "text/html", base)
			assert.NotContains(t, og, "og:image", src)
		}
	})
	t.Run("text is cut", func(t *testing.T) {
		og := ParseHTML(strings.NewReader(`<head><meta property="og:description" content="`+strings.Repeat("é", 5000)+`"></head>`), "text/html", base)
		assert.LessOrEqual(t, len(og["og:description"]), maxFieldLength)
		assert.True(t, strings.HasPrefix(og["og:description"], "éé"))
	})
	t.Run("other character sets", func(t *testing.T) {
		og := ParseHTML(bytes.NewReader([]byte("<head><title>caf\xe9</title></head>")), "text/html; charset=iso-8859-1", base)
		assert.Equal(t, "café", og["og:title"])
	})
	t.Run("not html", func(t *testing.T) {
		assert.Empty(t, ParseHTML(strings.NewReader("\x00\x01\x02 not a page"), "text/html", base))
	})
}

func testPreviewer(allow ...string) *Previewer {
	prefixes, _ := ParsePrefixes(allow)
	return New(Config{
		MaxPageBytes: 1 << 20, MaxImageBytes: 1 << 20, Timeout: 5 * time.Second, CacheTTL: time.Hour,
		MaxConcurrent: 4, MaxCached: 10, UserAgent: "test",
		Guard: Guard{Allow: prefixes},
	})
}

func pngBytes(t *testing.T, w, h int) []byte {
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h))))
	return buf.Bytes()
}

func TestPreviewer(t *testing.T) {
	var pageHits atomic.Int32
	cover := pngBytes(t, 40, 30)
	mux := http.NewServeMux()
	mux.HandleFunc("/page", func(w http.ResponseWriter, r *http.Request) {
		pageHits.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html><head><title>T</title><meta property="og:description" content="D"><meta property="og:image" content="/cover.png"></head><body>`+strings.Repeat("x", 3<<20))
	})
	mux.HandleFunc("/cover.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(cover)
	})
	mux.HandleFunc("/to-metadata", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	})
	mux.HandleFunc("/loop", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/loop", http.StatusFound) })
	mux.HandleFunc("/to-file", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "file:///etc/passwd", http.StatusFound)
	})
	mux.HandleFunc("/missing", http.NotFound)
	mux.HandleFunc("/huge-image", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<head><meta property="og:image" content="/huge.png"><meta property="og:title" content="Has a huge image"></head>`)
	})
	mux.HandleFunc("/huge.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(bytes.Repeat([]byte{0}, 2<<20))
	})
	mux.HandleFunc("/pdf", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Write([]byte("%PDF"))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	var stored atomic.Int32
	store := func(ctx context.Context, img *Image) (string, error) {
		stored.Add(1)
		assert.Equal(t, "image/png", img.ContentType)
		return fmt.Sprintf("mxc://test/%d", len(img.Data)), nil
	}
	ctx := context.Background()

	t.Run("the guard keeps the server from connecting to itself", func(t *testing.T) {
		_, err := testPreviewer().Preview(ctx, server.URL+"/page", "", store)
		require.ErrorIs(t, err, ErrFetch)
		assert.ErrorIs(t, err, ErrBlocked)
	})

	t.Run("a page, its image, and the cache", func(t *testing.T) {
		p := testPreviewer("127.0.0.0/8")
		got, err := p.Preview(ctx, server.URL+"/page", "en", store)
		require.NoError(t, err)
		assert.Equal(t, "T", got["og:title"])
		assert.Equal(t, "D", got["og:description"])
		assert.Equal(t, fmt.Sprintf("mxc://test/%d", len(cover)), got["og:image"])
		assert.Equal(t, "image/png", got["og:image:type"])
		assert.Equal(t, len(cover), got["matrix:image:size"])
		assert.Equal(t, 40, got["og:image:width"])
		assert.Equal(t, 30, got["og:image:height"])

		before := pageHits.Load()
		again, err := p.Preview(ctx, server.URL+"/page", "en", store)
		require.NoError(t, err)
		assert.Equal(t, got, again)
		assert.Equal(t, before, pageHits.Load(), "the second preview comes from the cache")
	})

	t.Run("redirects to addresses that are not allowed are refused", func(t *testing.T) {
		_, err := testPreviewer("127.0.0.0/8").Preview(ctx, server.URL+"/to-metadata", "", store)
		require.ErrorIs(t, err, ErrFetch)
		assert.ErrorIs(t, err, ErrBlocked)
	})

	t.Run("redirects to other schemes and endless redirects are refused", func(t *testing.T) {
		_, err := testPreviewer("127.0.0.0/8").Preview(ctx, server.URL+"/to-file", "", store)
		assert.ErrorIs(t, err, ErrFetch)
		_, err = testPreviewer("127.0.0.0/8").Preview(ctx, server.URL+"/loop", "", store)
		assert.ErrorIs(t, err, ErrFetch)
	})

	t.Run("errors from the server", func(t *testing.T) {
		_, err := testPreviewer("127.0.0.0/8").Preview(ctx, server.URL+"/missing", "", store)
		assert.ErrorIs(t, err, ErrFetch)
	})

	t.Run("things that are not pages have no preview", func(t *testing.T) {
		got, err := testPreviewer("127.0.0.0/8").Preview(ctx, server.URL+"/pdf", "", store)
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("an image that is too large is left out", func(t *testing.T) {
		got, err := testPreviewer("127.0.0.0/8").Preview(ctx, server.URL+"/huge-image", "", store)
		require.NoError(t, err)
		assert.Equal(t, "Has a huge image", got["og:title"])
		assert.NotContains(t, got, "og:image")
	})

	t.Run("the URL is an image", func(t *testing.T) {
		got, err := testPreviewer("127.0.0.0/8").Preview(ctx, server.URL+"/cover.png", "", store)
		require.NoError(t, err)
		assert.Equal(t, fmt.Sprintf("mxc://test/%d", len(cover)), got["og:image"])
	})

	t.Run("URLs that cannot be fetched", func(t *testing.T) {
		for _, raw := range []string{"file:///etc/passwd", "ftp://example.org/", "http://user:pw@example.org/"} {
			_, err := testPreviewer("127.0.0.0/8").Preview(ctx, raw, "", store)
			assert.Error(t, err, raw)
			assert.NotErrorIs(t, err, ErrBusy)
		}
		for _, raw := range []string{"http://127.0.0.1:1/", "http://[::1]:1/", "http://localhost:1/", "http://0.0.0.0/", "http://169.254.169.254/"} {
			_, err := testPreviewer().Preview(ctx, raw, "", store)
			assert.ErrorIs(t, err, ErrFetch, raw)
		}
	})

	t.Run("too many at once", func(t *testing.T) {
		p := testPreviewer("127.0.0.0/8")
		for i := 0; i < cap(p.slots); i++ {
			p.slots <- struct{}{}
		}
		_, err := p.Preview(ctx, server.URL+"/page", "", store)
		assert.ErrorIs(t, err, ErrBusy)
		cached, _ := p.Preview(ctx, server.URL+"/page", "", store) // still busy: nothing cached on this previewer
		assert.Nil(t, cached)
	})
}
