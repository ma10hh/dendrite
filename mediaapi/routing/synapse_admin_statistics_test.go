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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/element-hq/dendrite/internal/httputil"
	"github.com/element-hq/dendrite/internal/sqlutil"
	"github.com/element-hq/dendrite/mediaapi/storage"
	"github.com/element-hq/dendrite/mediaapi/types"
	"github.com/element-hq/dendrite/setup/config"
)

func TestSynapseAdminUserMediaStatistics(t *testing.T) {
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
	store := func(id, user string, size int64) {
		require.NoError(t, db.StoreMediaMetadata(ctx, &types.MediaMetadata{
			MediaID: types.MediaID(id), Origin: "test", ContentType: "text/plain", FileSizeBytes: types.FileSizeBytes(size),
			UploadName: "f", Base64Hash: types.Base64Hash("hash" + id), UserID: types.MatrixUserID(user),
		}))
	}
	store("a1", "@alice:test", 10)
	store("a2", "@alice:test", 20)
	store("b1", "@bob_x:test", 100)

	get := func(query string) (int, gjson.Result) {
		req := httptest.NewRequest(http.MethodGet, "/_synapse/admin/v1/statistics/users/media"+query, bytes.NewReader(nil))
		req.Header.Set("Authorization", "Bearer admin")
		rec := httptest.NewRecorder()
		routers.SynapseAdmin.ServeHTTP(rec, req)
		return rec.Code, gjson.Parse(rec.Body.String())
	}

	code, res := get("")
	require.Equal(t, http.StatusOK, code, res.Raw)
	assert.Equal(t, int64(2), res.Get("total").Int())
	assert.Equal(t, "@alice:test", res.Get("users.0.user_id").String(), "by user ID unless asked otherwise")
	assert.Equal(t, int64(2), res.Get("users.0.media_count").Int())
	assert.Equal(t, int64(30), res.Get("users.0.media_length").Int())

	_, res = get("?order_by=media_length&dir=b")
	assert.Equal(t, "@bob_x:test", res.Get("users.0.user_id").String(), "the one with the most bytes first")
	_, res = get("?order_by=media_count&dir=b")
	assert.Equal(t, "@alice:test", res.Get("users.0.user_id").String())

	_, res = get("?limit=1")
	assert.Equal(t, int64(1), res.Get("next_token").Int())
	assert.Len(t, res.Get("users").Array(), 1)
	_, res = get("?limit=1&from=1")
	assert.False(t, res.Get("next_token").Exists())
	assert.Equal(t, "@bob_x:test", res.Get("users.0.user_id").String())

	_, res = get("?search_term=bob_x")
	assert.Equal(t, int64(1), res.Get("total").Int())
	_, res = get("?search_term=bob%25")
	assert.Equal(t, int64(0), res.Get("total").Int(), "a percent sign is not a wildcard")
	_, res = get("?search_term=BOB")
	assert.Equal(t, int64(1), res.Get("total").Int(), "in any case")

	for _, bad := range []string{"?order_by=colour", "?dir=up", "?from=-1", "?limit=x", "?from_ts=x", "?from_ts=10&until_ts=5"} {
		code, _ = get(bad)
		assert.Equal(t, http.StatusBadRequest, code, bad)
	}
	now := time.Now().UnixMilli()
	_, res = get("?from_ts=" + strconv.FormatInt(now-60000, 10) + "&until_ts=" + strconv.FormatInt(now+60000, 10))
	assert.Equal(t, int64(2), res.Get("total").Int(), "the uploads of the last minute")
	_, res = get("?from_ts=" + strconv.FormatInt(now+60000, 10))
	assert.Equal(t, int64(0), res.Get("total").Int(), "nothing was uploaded in the future")
	_, res = get("?until_ts=" + strconv.FormatInt(now-60000, 10))
	assert.Equal(t, int64(0), res.Get("total").Int(), "nor before")
}
