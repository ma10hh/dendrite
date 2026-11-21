package routing

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/element-hq/dendrite/internal/httputil"
	"github.com/element-hq/dendrite/internal/sqlutil"
	"github.com/element-hq/dendrite/mediaapi/fileutils"
	"github.com/element-hq/dendrite/mediaapi/storage"
	"github.com/element-hq/dendrite/mediaapi/types"
	"github.com/element-hq/dendrite/setup/config"
)

func retentionTestSetup(t *testing.T, name string) (*config.Dendrite, storage.Database, httputil.Routers) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Dendrite{}
	cfg.Global.ServerName = "test"
	cfg.MediaAPI.Matrix = &cfg.Global
	cfg.MediaAPI.MaxFileSizeBytes = 1 << 20
	cfg.MediaAPI.BasePath = config.Path(dir)
	cfg.MediaAPI.AbsBasePath = config.Path(dir)
	cfg.ClientAPI.RateLimiting.Enabled = false
	db, err := storage.NewMediaAPIDatasource(sqlutil.NewConnectionManager(nil, config.DatabaseOptions{}), &config.DatabaseOptions{
		ConnectionString: config.DataSource("file:" + filepath.Join(t.TempDir(), name+".db")), MaxOpenConnections: 10, MaxIdleConnections: 2, ConnMaxLifetimeSeconds: -1,
	})
	require.NoError(t, err)
	routers := httputil.NewRouters()
	Setup(routers, cfg, db, fakeTokens{}, &fclient.Client{}, nil, nil)
	return cfg, db, routers
}

func TestRunRetention(t *testing.T) {
	cfg, db, _ := retentionTestSetup(t, "retention")
	ctx := context.Background()
	store := func(id string, origin spec.ServerName, hash string) string {
		meta := &types.MediaMetadata{
			MediaID: types.MediaID(id), Origin: origin, ContentType: "text/plain", FileSizeBytes: 3,
			UploadName: "f.txt", Base64Hash: types.Base64Hash(hash), UserID: "@bob:test",
		}
		require.NoError(t, db.StoreMediaMetadata(ctx, meta))
		path, err := fileutils.GetPathFromBase64Hash(meta.Base64Hash, cfg.MediaAPI.AbsBasePath)
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte("abc"), 0o644))
		return path
	}
	idle := store("local-idle", "test", "hashlocalidle")
	store("local-used", "test", "hashlocalused")
	store("remote-idle", "other.example", "hashremoteidle")
	store("remote-used", "other.example", "hashremoteused")
	store("remote-quarantined", "other.example", "hashremotequarantined")
	// The same file is a local and a remote media.
	sharedPath := store("local-shared", "test", "hashshared")
	store("remote-shared", "other.example", "hashshared")
	_, err := db.SetMediaQuarantined(ctx, "remote-quarantined", "other.example", true)
	require.NoError(t, err)

	now := time.Now().Add(48 * time.Hour)
	recent := spec.AsTimestamp(now.Add(-time.Hour))
	require.NoError(t, db.TouchMedia(ctx, "local-used", "test", recent))
	require.NoError(t, db.TouchMedia(ctx, "remote-used", "other.example", recent))
	require.NoError(t, db.TouchMedia(ctx, "local-shared", "test", recent))

	exists := func(id string, origin spec.ServerName) bool {
		m, err := db.GetMediaMetadata(ctx, types.MediaID(id), origin)
		require.NoError(t, err)
		return m != nil
	}

	t.Run("no lifetime deletes nothing", func(t *testing.T) {
		local, remote, err := RunRetention(ctx, &cfg.MediaAPI, db, now)
		require.NoError(t, err)
		assert.Zero(t, local+remote)
		assert.True(t, exists("local-idle", "test"))
	})

	t.Run("remote only", func(t *testing.T) {
		cfg.MediaAPI.Retention.RemoteMediaLifetime = 24 * time.Hour
		defer func() { cfg.MediaAPI.Retention.RemoteMediaLifetime = 0 }()
		local, remote, err := RunRetention(ctx, &cfg.MediaAPI, db, now)
		require.NoError(t, err)
		assert.Equal(t, 0, local)
		assert.Equal(t, 2, remote, "remote-idle and remote-shared")
		assert.False(t, exists("remote-idle", "other.example"))
		assert.True(t, exists("remote-used", "other.example"), "downloaded within the lifetime")
		assert.True(t, exists("remote-quarantined", "other.example"), "or it could be fetched again")
		assert.True(t, exists("local-idle", "test"))
		_, err = os.Stat(sharedPath)
		assert.NoError(t, err, "the file is still the local media's")
	})

	t.Run("local", func(t *testing.T) {
		cfg.MediaAPI.Retention.LocalMediaLifetime = 24 * time.Hour
		defer func() { cfg.MediaAPI.Retention.LocalMediaLifetime = 0 }()
		local, remote, err := RunRetention(ctx, &cfg.MediaAPI, db, now)
		require.NoError(t, err)
		assert.Equal(t, 1, local)
		assert.Equal(t, 0, remote)
		assert.False(t, exists("local-idle", "test"))
		_, err = os.Stat(idle)
		assert.True(t, os.IsNotExist(err), "the file of deleted media goes")
		assert.True(t, exists("local-used", "test"))
		assert.True(t, exists("local-shared", "test"))
	})
}

func TestUploadQuotaAndLastAccess(t *testing.T) {
	cfg, _, routers := retentionTestSetup(t, "quota")
	cfg.MediaAPI.MaxUserStorageBytes = 20

	upload := func(token, body string) (int, gjson.Result) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/_matrix/media/v3/upload?filename=f.txt", bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "text/plain")
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		routers.Media.ServeHTTP(rec, req)
		return rec.Code, gjson.Parse(rec.Body.String())
	}
	code, res := upload("user", "0123456789")
	require.Equal(t, http.StatusOK, code, res.Raw)
	mediaID := res.Get("content_uri").String()[len("mxc://test/"):]
	code, res = upload("user", "abcdefghij")
	require.Equal(t, http.StatusOK, code, res.Raw)
	code, res = upload("user", "x")
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, "M_RESOURCE_LIMIT_EXCEEDED", res.Get("errcode").String())
	code, res = upload("admin", "x")
	assert.Equal(t, http.StatusOK, code, "the quota is per user: %s", res.Raw)

	t.Run("a download is recorded", func(t *testing.T) {
		list := func() gjson.Result {
			req := httptest.NewRequest(http.MethodGet, "/_synapse/admin/v1/users/@user:test/media?order_by=created_ts", nil)
			req.Header.Set("Authorization", "Bearer admin")
			rec := httptest.NewRecorder()
			routers.SynapseAdmin.ServeHTTP(rec, req)
			return gjson.Parse(rec.Body.String())
		}
		before := list().Get(`media.#(media_id=="` + mediaID + `").last_access_ts`)
		assert.Equal(t, gjson.Null, before.Type)

		req := httptest.NewRequest(http.MethodGet, "/_matrix/client/v1/media/download/test/"+mediaID, nil)
		req.Header.Set("Authorization", "Bearer user")
		rec := httptest.NewRecorder()
		routers.Client.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code)

		after := list().Get(`media.#(media_id=="` + mediaID + `").last_access_ts`)
		assert.Greater(t, after.Int(), int64(0))
	})
}

// A chunked upload of a file that is stored already is charged what it is, not what the request announced (-1).
func TestDeduplicatedUploadIsChargedItsSize(t *testing.T) {
	cfg, db, routers := retentionTestSetup(t, "dedup")
	cfg.MediaAPI.MaxUserStorageBytes = 100
	upload := func(chunked bool) gjson.Result {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/_matrix/media/v3/upload?filename=f.txt", bytes.NewReader([]byte("0123456789")))
		if chunked {
			req.ContentLength = -1
		}
		req.Header.Set("Content-Type", "text/plain")
		req.Header.Set("Authorization", "Bearer user")
		rec := httptest.NewRecorder()
		routers.Media.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		return gjson.Parse(rec.Body.String())
	}
	upload(false)
	upload(true)
	used, err := db.UserMediaBytes(context.Background(), "@user:test")
	require.NoError(t, err)
	assert.Equal(t, int64(20), used, "both media count their ten bytes")
}
