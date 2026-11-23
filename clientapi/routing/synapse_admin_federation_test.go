package routing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/matrix-org/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/element-hq/dendrite/federationapi/api"
)

type fakeDestinations struct {
	api.ClientFederationAPI
	known []api.DestinationInfo
	// dormant are servers that are only in the database: blacklisted, never contacted since the start
	dormant map[spec.ServerName]api.DestinationInfo
	reset   []spec.ServerName
}

func (f *fakeDestinations) QueryDestinations(context.Context) ([]api.DestinationInfo, error) {
	return f.known, nil
}

func (f *fakeDestinations) QueryDestination(_ context.Context, n spec.ServerName) (api.DestinationInfo, error) {
	if d, ok := f.dormant[n]; ok {
		return d, nil
	}
	return api.DestinationInfo{ServerName: n}, nil
}

func (f *fakeDestinations) ResetDestination(_ context.Context, n spec.ServerName) (bool, error) {
	f.reset = append(f.reset, n)
	return true, nil
}

func TestSynapseAdminDestinations(t *testing.T) {
	fs := &fakeDestinations{
		known: []api.DestinationInfo{
			{ServerName: "b.example", FirstFailureMS: 1000, LastFailureMS: 5000, BackoffIntervalMS: 4000, FailureCount: 2},
			{ServerName: "a.example", SuccessCount: 7},
			{ServerName: "c.example", FirstFailureMS: 2000, LastFailureMS: 3000, BackoffIntervalMS: 1000, FailureCount: 1, Blacklisted: true},
		},
		dormant: map[spec.ServerName]api.DestinationInfo{"gone.example": {ServerName: "gone.example", Blacklisted: true}},
	}
	do := func(h func(*http.Request, api.ClientFederationAPI) util.JSONResponse, method, target string, vars map[string]string) (int, gjson.Result) {
		req := mux.SetURLVars(httptest.NewRequest(method, target, nil), vars)
		res := h(req, fs)
		b, err := json.Marshal(res.JSON)
		require.NoError(t, err)
		return res.Code, gjson.ParseBytes(b)
	}

	code, res := do(SynapseAdminListDestinations, http.MethodGet, "/x", nil)
	require.Equal(t, http.StatusOK, code, res.Raw)
	assert.Equal(t, int64(3), res.Get("total").Int())
	assert.Equal(t, "a.example", res.Get("destinations.0.destination").String(), "by name unless asked otherwise")
	assert.False(t, res.Get("destinations.0.failure_ts").Exists() && res.Get("destinations.0.failure_ts").Type != gjson.Null, "no failure: null")

	_, res = do(SynapseAdminListDestinations, http.MethodGet, "/x?order_by=retry_last_ts&dir=b", nil)
	assert.Equal(t, "b.example", res.Get("destinations.0.destination").String(), "the latest failure first")
	assert.Equal(t, int64(5000), res.Get("destinations.0.retry_last_ts").Int())
	assert.Equal(t, int64(4000), res.Get("destinations.0.retry_interval").Int())
	assert.Equal(t, int64(1000), res.Get("destinations.0.failure_ts").Int())
	assert.True(t, res.Get("destinations.2.destination").Exists())

	_, res = do(SynapseAdminListDestinations, http.MethodGet, "/x?limit=2", nil)
	assert.Len(t, res.Get("destinations").Array(), 2)
	assert.Equal(t, "2", res.Get("next_token").String())
	_, res = do(SynapseAdminListDestinations, http.MethodGet, "/x?limit=2&from=2", nil)
	assert.Len(t, res.Get("destinations").Array(), 1)
	assert.False(t, res.Get("next_token").Exists())
	_, res = do(SynapseAdminListDestinations, http.MethodGet, "/x?destination=B.EX", nil)
	assert.Equal(t, int64(1), res.Get("total").Int(), "a part of the name, in any case")

	for _, bad := range []string{"order_by=x", "dir=up", "from=-1", "limit=x"} {
		code, _ = do(SynapseAdminListDestinations, http.MethodGet, "/x?"+bad, nil)
		assert.Equal(t, http.StatusBadRequest, code, bad)
	}

	code, res = do(SynapseAdminGetDestination, http.MethodGet, "/x", map[string]string{"destination": "c.example"})
	require.Equal(t, http.StatusOK, code, res.Raw)
	assert.True(t, res.Get("blacklisted").Bool())
	code, _ = do(SynapseAdminGetDestination, http.MethodGet, "/x", map[string]string{"destination": "gone.example"})
	assert.Equal(t, http.StatusOK, code, "a blacklisted server that was not contacted since the start is still a destination")
	code, _ = do(SynapseAdminGetDestination, http.MethodGet, "/x", map[string]string{"destination": "never.example"})
	assert.Equal(t, http.StatusNotFound, code)

	code, _ = do(SynapseAdminResetDestination, http.MethodPost, "/x", map[string]string{"destination": "c.example"})
	assert.Equal(t, http.StatusOK, code)
	code, _ = do(SynapseAdminResetDestination, http.MethodPost, "/x", map[string]string{"destination": "never.example"})
	assert.Equal(t, http.StatusNotFound, code)
	assert.Equal(t, []spec.ServerName{"c.example"}, fs.reset, "only the destination that exists was reset")
}
