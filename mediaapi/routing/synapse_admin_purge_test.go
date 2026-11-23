package routing

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/element-hq/dendrite/internal/httputil"
	"github.com/element-hq/dendrite/internal/sqlutil"
	"github.com/element-hq/dendrite/mediaapi/storage"
	"github.com/element-hq/dendrite/mediaapi/types"
	"github.com/element-hq/dendrite/setup/config"
)

func TestSynapseAdminPurgeMediaCache(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Dendrite{}
	cfg.Global.ServerName = "test"
	cfg.MediaAPI.Matrix = &cfg.Global
	cfg.MediaAPI.MaxFileSizeBytes = 1 << 20
	cfg.MediaAPI.BasePath = config.Path(dir)
	cfg.MediaAPI.AbsBasePath = config.Path(dir)
	cfg.ClientAPI.RateLimiting.Enabled = false
	db, err := storage.NewMediaAPIDatasource(sqlutil.NewConnectionManager(nil, config.DatabaseOptions{}), &config.DatabaseOptions{
		ConnectionString: config.DataSource("file:" + dir + "/media.db"), MaxOpenConnections: 10, MaxIdleConnections: 2, ConnMaxLifetimeSeconds: -1,
	})
	require.NoError(t, err)
	routers := httputil.NewRouters()
	Setup(routers, cfg, db, fakeTokens{}, &fclient.Client{}, nil, nil)

	ctx := context.Background()
	store := func(id, origin, user string) {
		require.NoError(t, db.StoreMediaMetadata(ctx, &types.MediaMetadata{
			MediaID: types.MediaID(id), Origin: spec.ServerName(origin), ContentType: "text/plain", FileSizeBytes: 5,
			UploadName: "f", Base64Hash: types.Base64Hash("hash" + id), UserID: types.MatrixUserID(user),
		}))
	}
	store("mine", "test", "@a:test")
	store("copy1", "elsewhere.org", "")
	store("copy2", "elsewhere.org", "")
	store("copy3", "other.org", "")
	_, err = db.SetMediaQuarantined(ctx, "copy3", "other.org", true)
	require.NoError(t, err)

	post := func(token, query string) (int, gjson.Result) {
		req := httptest.NewRequest(http.MethodPost, "/_synapse/admin/v1/purge_media_cache"+query, bytes.NewReader(nil))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		routers.SynapseAdmin.ServeHTTP(rec, req)
		return rec.Code, gjson.Parse(rec.Body.String())
	}
	code, _ := post("user", "?before_ts=1")
	assert.Equal(t, http.StatusForbidden, code)
	code, _ = post("admin", "")
	assert.Equal(t, http.StatusBadRequest, code, "before_ts is required")
	code, _ = post("admin", "?before_ts=x")
	assert.Equal(t, http.StatusBadRequest, code)

	code, res := post("admin", "?before_ts=1")
	require.Equal(t, http.StatusOK, code, res.Raw)
	assert.Equal(t, int64(0), res.Get("deleted").Int(), "nothing is that old")

	code, res = post("admin", "?before_ts="+strconv.FormatInt(time.Now().Add(time.Hour).UnixMilli(), 10))
	require.Equal(t, http.StatusOK, code, res.Raw)
	assert.Equal(t, int64(2), res.Get("deleted").Int(), "the two copies; not the local media and not the quarantined copy")
	left, err := db.GetMediaMetadata(ctx, "mine", "test")
	require.NoError(t, err)
	assert.NotNil(t, left)
	left, err = db.GetMediaMetadata(ctx, "copy3", "other.org")
	require.NoError(t, err)
	assert.NotNil(t, left)
	gone, err := db.GetMediaMetadata(ctx, "copy1", "elsewhere.org")
	require.NoError(t, err)
	assert.Nil(t, gone)
}
