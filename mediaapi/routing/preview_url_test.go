package routing

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/element-hq/dendrite/internal/sqlutil"
	"github.com/element-hq/dendrite/mediaapi/fileutils"
	"github.com/element-hq/dendrite/mediaapi/storage"
	"github.com/element-hq/dendrite/mediaapi/types"
	"github.com/element-hq/dendrite/setup/config"
	userapi "github.com/element-hq/dendrite/userapi/api"
)

func TestPreviewURL(t *testing.T) {
	wd, err := os.Getwd()
	require.NoError(t, err)
	store := filepath.Join(wd, "testdata-preview")
	require.NoError(t, os.Mkdir(store, os.ModePerm))
	defer fileutils.RemoveDir(types.Path(store), nil)

	cfg := &config.MediaAPI{
		Matrix:           &config.Global{},
		MaxFileSizeBytes: 1 << 20,
		BasePath:         config.Path(store),
		AbsBasePath:      config.Path(store),
	}
	cfg.Matrix.ServerName = "test"
	cfg.URLPreviews = config.URLPreviews{
		Enabled: true, MaxPageSizeBytes: 1 << 20, Timeout: 5_000_000_000, CacheLifetime: 3_600_000_000_000, MaxConcurrent: 2,
		IPRangeAllowlist: []string{"127.0.0.0/8"},
	}
	db, err := storage.NewMediaAPIDatasource(sqlutil.NewConnectionManager(nil, config.DatabaseOptions{}), &config.DatabaseOptions{
		ConnectionString: "file::memory:?cache=shared", MaxOpenConnections: 10, MaxIdleConnections: 2, ConnMaxLifetimeSeconds: -1,
	})
	require.NoError(t, err)

	var cover bytes.Buffer
	require.NoError(t, png.Encode(&cover, image.NewRGBA(image.Rect(0, 0, 8, 8))))
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cover.png":
			w.Header().Set("Content-Type", "image/png")
			w.Write(cover.Bytes())
		case "/icon.svg":
			w.Header().Set("Content-Type", "image/svg+xml")
			fmt.Fprint(w, `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
		case "/svg-page":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<head><meta property="og:title" content="Svg"><meta property="og:image" content="/icon.svg"></head>`)
		default:
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<head><meta property="og:title" content="Hello"><meta property="og:image" content="/cover.png"></head>`)
		}
	}))
	defer site.Close()

	previewer, err := newURLPreviewer(cfg)
	require.NoError(t, err)
	require.NotNil(t, previewer)
	dev := &userapi.Device{UserID: "@alice:test"}
	active := &types.ActiveThumbnailGeneration{PathToResult: map[string]*types.ThumbnailGenerationResult{}}
	preview := func(target string) (int, map[string]any, spec.MatrixError) {
		req := httptest.NewRequest(http.MethodGet, "/preview_url?url="+url.QueryEscape(target), nil)
		res := PreviewURL(req, dev, cfg, previewer, db, active)
		if body, ok := res.JSON.(map[string]any); ok {
			return res.Code, body, spec.MatrixError{}
		}
		merr, _ := res.JSON.(spec.MatrixError)
		return res.Code, nil, merr
	}

	t.Run("a page with an image", func(t *testing.T) {
		code, body, _ := preview(site.URL + "/page")
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, "Hello", body["og:title"])
		mxc, _ := body["og:image"].(string)
		require.True(t, strings.HasPrefix(mxc, "mxc://test/"), mxc)
		assert.Equal(t, cover.Len(), body["matrix:image:size"])
		assert.Equal(t, 8, body["og:image:width"])

		// The image is media of the server now.
		meta, err := db.GetMediaMetadata(t.Context(), types.MediaID(strings.TrimPrefix(mxc, "mxc://test/")), "test")
		require.NoError(t, err)
		require.NotNil(t, meta)
		assert.Equal(t, types.ContentType("image/png"), meta.ContentType)
		assert.Equal(t, types.MatrixUserID("@alice:test"), meta.UserID)
	})

	t.Run("an image that can carry scripts is not kept", func(t *testing.T) {
		code, body, _ := preview(site.URL + "/svg-page")
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, "Svg", body["og:title"])
		assert.NotContains(t, body, "og:image")
	})

	t.Run("bad requests", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/preview_url", nil)
		res := PreviewURL(req, dev, cfg, previewer, db, active)
		assert.Equal(t, http.StatusBadRequest, res.Code)
		assert.Equal(t, spec.ErrorMissingParam, res.JSON.(spec.MatrixError).ErrCode)

		code, _, merr := preview("file:///etc/passwd")
		assert.Equal(t, http.StatusBadRequest, code)
		assert.Equal(t, spec.ErrorInvalidParam, merr.ErrCode)
	})

	t.Run("the server does not reveal what it cannot reach", func(t *testing.T) {
		// The default guard still applies to a previewer without the allowlist.
		plain := *cfg
		plain.URLPreviews.IPRangeAllowlist = nil
		blocked, err := newURLPreviewer(&plain)
		require.NoError(t, err)
		res := PreviewURL(httptest.NewRequest(http.MethodGet, "/preview_url?url="+url.QueryEscape(site.URL), nil), dev, &plain, blocked, db, active)
		assert.Equal(t, http.StatusBadGateway, res.Code)
		assert.Equal(t, "Failed to fetch the URL", res.JSON.(spec.MatrixError).Err)
	})

	t.Run("off by default", func(t *testing.T) {
		off := *cfg
		off.URLPreviews.Enabled = false
		p, err := newURLPreviewer(&off)
		require.NoError(t, err)
		assert.Nil(t, p)
	})
}
