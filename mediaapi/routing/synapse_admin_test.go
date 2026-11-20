package routing

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/element-hq/dendrite/internal/httputil"
	"github.com/element-hq/dendrite/internal/sqlutil"
	"github.com/element-hq/dendrite/mediaapi/fileutils"
	"github.com/element-hq/dendrite/mediaapi/storage"
	"github.com/element-hq/dendrite/mediaapi/types"
	"github.com/element-hq/dendrite/setup/config"
	userapi "github.com/element-hq/dendrite/userapi/api"
)

type fakeTokens struct{}

func (fakeTokens) QueryAccessToken(_ context.Context, req *userapi.QueryAccessTokenRequest, res *userapi.QueryAccessTokenResponse) error {
	switch req.AccessToken {
	case "admin":
		res.Device = &userapi.Device{UserID: "@admin:test", ID: "A", AccountType: userapi.AccountTypeAdmin}
	case "user":
		res.Device = &userapi.Device{UserID: "@user:test", ID: "U", AccountType: userapi.AccountTypeUser}
	}
	return nil
}

func TestSynapseAdminMedia(t *testing.T) {
	dir := t.TempDir()
	global := &config.Global{}
	global.ServerName = "test"
	global.Metrics.Enabled = false
	cfg := &config.Dendrite{Global: *global}
	cfg.MediaAPI.Matrix = &cfg.Global
	cfg.MediaAPI.MaxFileSizeBytes = 1 << 20
	cfg.MediaAPI.BasePath = config.Path(dir)
	cfg.MediaAPI.AbsBasePath = config.Path(dir)
	cfg.ClientAPI.RateLimiting.Enabled = false
	db, err := storage.NewMediaAPIDatasource(sqlutil.NewConnectionManager(nil, config.DatabaseOptions{}), &config.DatabaseOptions{
		ConnectionString: "file::memory:?cache=shared", MaxOpenConnections: 10, MaxIdleConnections: 2, ConnMaxLifetimeSeconds: -1,
	})
	require.NoError(t, err)

	routers := httputil.NewRouters()
	Setup(routers, cfg, db, fakeTokens{}, &fclient.Client{}, nil, nil)

	ctx := context.Background()
	store := func(id, hash, user string, size int64, body string) {
		meta := &types.MediaMetadata{
			MediaID: types.MediaID(id), Origin: "test", ContentType: "text/plain", FileSizeBytes: types.FileSizeBytes(size),
			UploadName: types.Filename(id + ".txt"), Base64Hash: types.Base64Hash(hash), UserID: types.MatrixUserID(user),
		}
		require.NoError(t, db.StoreMediaMetadata(ctx, meta))
		path, perr := fileutils.GetPathFromBase64Hash(meta.Base64Hash, cfg.MediaAPI.AbsBasePath)
		require.NoError(t, perr)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	}
	store("one", "hashone", "@bob:test", 5, "hello")
	store("two", "hashtwo", "@bob:test", 5, "world")
	store("twin", "hashtwo", "@carol:test", 5, "world") // the same file as "two"
	store("big", "hashbig", "@carol:test", 500, "b")

	do := func(router string, token, method, path string) (int, string, gjson.Result) {
		t.Helper()
		req := httptest.NewRequest(method, path, bytes.NewReader(nil))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		if router == "admin" {
			routers.SynapseAdmin.ServeHTTP(rec, req)
		} else {
			routers.Client.ServeHTTP(rec, req)
		}
		return rec.Code, rec.Body.String(), gjson.Parse(rec.Body.String())
	}
	download := func(id string) int {
		t.Helper()
		code, _, _ := do("client", "user", http.MethodGet, "/_matrix/client/v1/media/download/test/"+id)
		return code
	}

	t.Run("only admins", func(t *testing.T) {
		code, _, _ := do("admin", "user", http.MethodPost, "/_synapse/admin/v1/media/quarantine/test/one")
		assert.Equal(t, http.StatusForbidden, code)
		code, _, _ = do("admin", "", http.MethodGet, "/_synapse/admin/v1/users/@bob:test/media")
		assert.Equal(t, http.StatusUnauthorized, code)
	})

	t.Run("quarantine", func(t *testing.T) {
		assert.Equal(t, http.StatusOK, download("one"))
		code, _, _ := do("admin", "admin", http.MethodPost, "/_synapse/admin/v1/media/quarantine/test/one")
		assert.Equal(t, http.StatusOK, code)
		assert.Equal(t, http.StatusNotFound, download("one"), "quarantined media is not served")
		assert.Equal(t, http.StatusOK, download("two"), "other media is")
		code, _, _ = do("admin", "admin", http.MethodPost, "/_synapse/admin/v1/media/unquarantine/test/one")
		assert.Equal(t, http.StatusOK, code)
		assert.Equal(t, http.StatusOK, download("one"))
		code, _, _ = do("admin", "admin", http.MethodPost, "/_synapse/admin/v1/media/quarantine/test/missing")
		assert.Equal(t, http.StatusNotFound, code)

		_, _, res := do("admin", "admin", http.MethodPost, "/_synapse/admin/v1/user/@bob:test/media/quarantine")
		assert.Equal(t, int64(2), res.Get("num_quarantined").Int())
		assert.Equal(t, http.StatusNotFound, download("one"))
		assert.Equal(t, http.StatusNotFound, download("two"))
		assert.Equal(t, http.StatusOK, download("twin"), "the same file under another media ID is not quarantined")
		_, _, _ = do("admin", "admin", http.MethodPost, "/_synapse/admin/v1/media/unquarantine/test/one")
		_, _, _ = do("admin", "admin", http.MethodPost, "/_synapse/admin/v1/media/unquarantine/test/two")
	})

	t.Run("list a user's media", func(t *testing.T) {
		code, _, res := do("admin", "admin", http.MethodGet, "/_synapse/admin/v1/users/@carol:test/media?limit=1&order_by=media_length&dir=b")
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, int64(2), res.Get("total").Int())
		assert.Equal(t, "big", res.Get("media.0.media_id").String())
		assert.Equal(t, int64(500), res.Get("media.0.media_length").Int())
		assert.Equal(t, int64(1), res.Get("next_token").Int())
		code, _, _ = do("admin", "admin", http.MethodGet, "/_synapse/admin/v1/users/@carol:test/media?order_by=x")
		assert.Equal(t, http.StatusBadRequest, code)
	})

	fileExists := func(hash string) bool {
		path, perr := fileutils.GetPathFromBase64Hash(types.Base64Hash(hash), cfg.MediaAPI.AbsBasePath)
		require.NoError(t, perr)
		_, serr := os.Stat(path)
		return serr == nil
	}

	t.Run("delete", func(t *testing.T) {
		code, _, _ := do("admin", "admin", http.MethodDelete, "/_synapse/admin/v1/media/other.example/one")
		assert.Equal(t, http.StatusBadRequest, code, "only media of this server")

		code, _, res := do("admin", "admin", http.MethodDelete, "/_synapse/admin/v1/media/test/two")
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, int64(1), res.Get("total").Int())
		assert.True(t, fileExists("hashtwo"), "the file stays while other media uses it")
		assert.Equal(t, http.StatusNotFound, download("two"))
		assert.Equal(t, http.StatusOK, download("twin"))

		code, _, res = do("admin", "admin", http.MethodPost, "/_synapse/admin/v1/media/test/delete?before_ts=1&size_gt=100")
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, int64(0), res.Get("total").Int(), "nothing is that old")
		future := time.Now().Add(time.Hour).UnixMilli()
		code, _, res = do("admin", "admin", http.MethodPost, "/_synapse/admin/v1/media/test/delete?size_gt=100&before_ts="+itoa(future))
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, []string{"big"}, func() (out []string) {
			for _, m := range res.Get("deleted_media").Array() {
				out = append(out, m.String())
			}
			return
		}(), "only what is larger")
		assert.False(t, fileExists("hashbig"), "the file goes with its last media")
		code, _, _ = do("admin", "admin", http.MethodPost, "/_synapse/admin/v1/media/test/delete")
		assert.Equal(t, http.StatusBadRequest, code, "before_ts is required")

		_, _, res = do("admin", "admin", http.MethodDelete, "/_synapse/admin/v1/users/@carol:test/media")
		assert.Equal(t, int64(1), res.Get("total").Int())
		assert.False(t, fileExists("hashtwo"))
		_, _, res = do("admin", "admin", http.MethodGet, "/_synapse/admin/v1/users/@bob:test/media")
		assert.Equal(t, int64(1), res.Get("total").Int(), "bob has one left: one")
	})
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
