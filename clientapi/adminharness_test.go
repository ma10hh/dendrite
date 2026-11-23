package clientapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/element-hq/dendrite/internal/caching"
	"github.com/element-hq/dendrite/internal/httputil"
	"github.com/element-hq/dendrite/internal/sqlutil"
	"github.com/element-hq/dendrite/internal/transactions"
	"github.com/element-hq/dendrite/roomserver"
	rsapi "github.com/element-hq/dendrite/roomserver/api"
	"github.com/element-hq/dendrite/setup/config"
	"github.com/element-hq/dendrite/setup/jetstream"
	"github.com/element-hq/dendrite/test"
	"github.com/element-hq/dendrite/test/testrig"
	"github.com/element-hq/dendrite/userapi"
	uapi "github.com/element-hq/dendrite/userapi/api"
)

// adminHarness is a client API with a real roomserver and user API, and signed-in users.
type adminHarness struct {
	t       *testing.T
	ctx     context.Context
	cfg     *config.Dendrite
	routers httputil.Routers
	rsAPI   rsapi.RoomserverInternalAPI
	userAPI uapi.UserInternalAPI
	tokens  map[*test.User]userDevice
}

// newAdminHarness starts it. Change the configuration in tune, which runs before the routes are set up.
func newAdminHarness(t *testing.T, dbType test.DBType, tune func(*config.Dendrite), users ...*test.User) (*adminHarness, func()) {
	t.Helper()
	cfg, processCtx, closeFn := testrig.CreateConfig(t, dbType)
	if tune != nil {
		tune(cfg)
	}
	natsInstance := jetstream.NATSInstance{}
	routers := httputil.NewRouters()
	cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
	caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
	rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
	rsAPI.SetFederationAPI(nil, nil)
	userAPI := userapi.NewInternalAPI(processCtx, cfg, cm, &natsInstance, rsAPI, nil, caching.DisableMetrics, testIsBlacklistedOrBackingOff)
	AddPublicRoutes(processCtx, routers, cfg, &natsInstance, nil, rsAPI, nil, transactions.New(), nil, userAPI, nil, nil, caching.DisableMetrics)
	h := &adminHarness{t: t, ctx: context.Background(), cfg: cfg, routers: routers, rsAPI: rsAPI, userAPI: userAPI, tokens: map[*test.User]userDevice{}}
	for _, u := range users {
		h.tokens[u] = userDevice{}
	}
	createAccessTokens(t, h.tokens, userAPI, h.ctx, routers)
	return h, closeFn
}

// call makes a request as the user, with a JSON body if there is one.
func (h *adminHarness) call(router http.Handler, user *test.User, method, path string, body any) (int, gjson.Result) {
	h.t.Helper()
	var req *http.Request
	if body != nil {
		req = test.NewRequest(h.t, method, path, test.WithJSONBody(h.t, body))
	} else {
		req = test.NewRequest(h.t, method, path)
	}
	if user != nil {
		req.Header.Set("Authorization", "Bearer "+h.tokens[user].accessToken)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec.Code, gjson.Parse(rec.Body.String())
}

func (h *adminHarness) sendRoom(room *test.Room) {
	h.t.Helper()
	if err := rsapi.SendEvents(h.ctx, h.rsAPI, rsapi.KindNew, room.Events(), "test", "test", "test", nil, false); err != nil {
		h.t.Fatalf("SendEvents: %v", err)
	}
}
