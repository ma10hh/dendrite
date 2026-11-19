// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

// Package urlpreview fetches pages for the URL previews of the media API. The server connects to addresses that
// users name, so every connection goes through a Guard, and every response is limited in size and time.
package urlpreview

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	maxURLLength = 2048
	maxRedirects = 5
)

// safeImageTypes are the image types that are stored. SVG can carry scripts, so it is not one of them.
var safeImageTypes = map[string]bool{
	"image/png": true, "image/apng": true, "image/jpeg": true, "image/gif": true,
	"image/webp": true, "image/avif": true, "image/bmp": true,
}

var (
	// ErrBusy is returned when too many previews are being made.
	ErrBusy = errors.New("too many previews are being made")
	// ErrFetch is returned for everything that went wrong getting the page, with the reason attached for the log.
	ErrFetch = errors.New("failed to fetch the URL")
)

// Config limits what a preview may cost.
type Config struct {
	// MaxPageBytes is how much of a page is read.
	MaxPageBytes int64
	// MaxImageBytes is the largest image that is stored.
	MaxImageBytes int64
	// Timeout is for one request, including the redirects it follows.
	Timeout time.Duration
	// CacheTTL is how long a preview is kept.
	CacheTTL time.Duration
	// MaxConcurrent is how many previews may be made at once.
	MaxConcurrent int
	// MaxCached is how many previews are kept.
	MaxCached int
	// UserAgent is sent to the servers that are asked.
	UserAgent string
	Guard     Guard
}

// Image is a picture that goes with a page.
type Image struct {
	Data        []byte
	ContentType string
	Width       int
	Height      int
}

// ImageStore keeps an image of a preview in the media repository and returns its mxc URI.
type ImageStore func(ctx context.Context, img *Image) (mxcURI string, err error)

type cached struct {
	preview map[string]any
	expires time.Time
}

// Previewer makes previews of URLs.
type Previewer struct {
	cfg    Config
	client *http.Client
	slots  chan struct{}
	group  singleflight.Group

	mu    sync.Mutex
	cache map[string]cached
}

// New returns a Previewer.
func New(cfg Config) *Previewer {
	transport := &http.Transport{
		DialContext:            cfg.Guard.DialContext,
		Proxy:                  nil, // a proxy would make the connections that the guard cannot see
		TLSHandshakeTimeout:    10 * time.Second,
		ResponseHeaderTimeout:  10 * time.Second,
		MaxResponseHeaderBytes: 64 * 1024,
		DisableKeepAlives:      true,
	}
	return &Previewer{
		cfg: cfg,
		client: &http.Client{
			Transport: transport,
			Timeout:   cfg.Timeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= maxRedirects {
					return fmt.Errorf("stopped after %d redirects", maxRedirects)
				}
				_, err := ValidateURL(req.URL.String())
				return err
			},
		},
		slots: make(chan struct{}, max(cfg.MaxConcurrent, 1)),
		cache: map[string]cached{},
	}
}

// ValidateURL checks that a URL is one that may be fetched: http or https, with a host and without credentials.
func ValidateURL(raw string) (*url.URL, error) {
	if len(raw) > maxURLLength {
		return nil, errors.New("the URL is too long")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("only http and https URLs can be previewed")
	}
	if u.Hostname() == "" {
		return nil, errors.New("the URL has no host")
	}
	if u.User != nil {
		return nil, errors.New("the URL must not have credentials")
	}
	return u, nil
}

// Preview returns the Open Graph data of a URL, with its image kept by store. The result is cached.
func (p *Previewer) Preview(ctx context.Context, rawURL, acceptLanguage string, store ImageStore) (map[string]any, error) {
	u, err := ValidateURL(rawURL)
	if err != nil {
		return nil, err
	}
	key := u.String()
	if hit, ok := p.lookup(key); ok {
		return hit, nil
	}
	select {
	case p.slots <- struct{}{}:
		defer func() { <-p.slots }()
	default:
		return nil, ErrBusy
	}
	res, err, _ := p.group.Do(key, func() (any, error) {
		// One caller going away does not stop the others waiting for the same page.
		detached := context.WithoutCancel(ctx)
		preview, err := p.build(detached, u, acceptLanguage, store)
		if err != nil {
			return nil, err
		}
		p.store(key, preview)
		return preview, nil
	})
	if err != nil {
		return nil, err
	}
	return res.(map[string]any), nil
}

func (p *Previewer) lookup(key string) (map[string]any, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	hit, ok := p.cache[key]
	if !ok || time.Now().After(hit.expires) {
		delete(p.cache, key)
		return nil, false
	}
	return hit.preview, true
}

func (p *Previewer) store(key string, preview map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if len(p.cache) >= max(p.cfg.MaxCached, 1) {
		for k, v := range p.cache {
			if now.After(v.expires) {
				delete(p.cache, k)
			}
		}
		for k := range p.cache {
			if len(p.cache) < p.cfg.MaxCached {
				break
			}
			delete(p.cache, k)
		}
	}
	p.cache[key] = cached{preview: preview, expires: now.Add(p.cfg.CacheTTL)}
}

func (p *Previewer) build(ctx context.Context, u *url.URL, acceptLanguage string, store ImageStore) (map[string]any, error) {
	resp, err := p.get(ctx, u.String(), acceptLanguage, "text/html,application/xhtml+xml;q=0.9,image/*;q=0.8,*/*;q=0.1")
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrFetch, err)
	}
	defer resp.Body.Close()
	final := resp.Request.URL

	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	og := map[string]string{}
	switch {
	case mediaType == "text/html" || mediaType == "application/xhtml+xml":
		og = ParseHTML(io.LimitReader(resp.Body, p.cfg.MaxPageBytes), resp.Header.Get("Content-Type"), final)
	case strings.HasPrefix(mediaType, "image/"):
		// The URL is the image itself.
		og["og:image"] = final.String()
	}

	preview := make(map[string]any, len(og)+4)
	for k, v := range og {
		if k != "og:image" && k != "og:image:url" && k != "og:image:secure_url" && !strings.HasPrefix(k, "og:image:") {
			preview[k] = v
		}
	}
	imageURL := og["og:image"]
	if imageURL == "" {
		imageURL = og["og:image:secure_url"]
	}
	if imageURL == "" {
		imageURL = og["og:image:url"]
	}
	if imageURL != "" && store != nil {
		if img, ierr := p.fetchImage(ctx, imageURL, acceptLanguage); ierr != nil {
			// A page is still worth showing without its picture.
			_ = ierr
		} else if mxc, serr := store(ctx, img); serr == nil {
			preview["og:image"] = mxc
			preview["og:image:type"] = img.ContentType
			preview["matrix:image:size"] = len(img.Data)
			if img.Width > 0 && img.Height > 0 {
				preview["og:image:width"] = img.Width
				preview["og:image:height"] = img.Height
			}
		}
	}
	return preview, nil
}

func (p *Previewer) get(ctx context.Context, rawURL, acceptLanguage, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", p.cfg.UserAgent)
	req.Header.Set("Accept", accept)
	if acceptLanguage != "" {
		req.Header.Set("Accept-Language", acceptLanguage)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		resp.Body.Close()
		return nil, fmt.Errorf("the server answered %d", resp.StatusCode)
	}
	return resp, nil
}

func (p *Previewer) fetchImage(ctx context.Context, rawURL, acceptLanguage string) (*Image, error) {
	if _, err := ValidateURL(rawURL); err != nil {
		return nil, err
	}
	resp, err := p.get(ctx, rawURL, acceptLanguage, "image/*")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if !safeImageTypes[mediaType] {
		return nil, errors.New("not an image that is kept")
	}
	if resp.ContentLength > p.cfg.MaxImageBytes {
		return nil, errors.New("the image is too large")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, p.cfg.MaxImageBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > p.cfg.MaxImageBytes {
		return nil, errors.New("the image is too large")
	}
	img := &Image{Data: data, ContentType: mediaType}
	if conf, _, derr := image.DecodeConfig(bytes.NewReader(data)); derr == nil {
		img.Width, img.Height = conf.Width, conf.Height
	}
	return img, nil
}
