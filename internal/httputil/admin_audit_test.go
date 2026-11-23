package httputil

import (
	"net/http"
	"net/http/httptest"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"

	userapi "github.com/element-hq/dendrite/userapi/api"
)

func TestAuditAdminCall(t *testing.T) {
	hook := logrustest.NewGlobal()
	defer hook.Reset()
	req := httptest.NewRequest(http.MethodPost, "/_synapse/admin/v1/reset_password/@a:x?access_token=secret&password=hunter2", nil)
	req.RemoteAddr = "192.0.2.1:1234"
	before := counterValue(t, "test_handler", "200")

	auditAdminCall(req, &userapi.Device{UserID: "@admin:x", ID: "DEV"}, "test_handler", http.StatusOK)

	entry := hook.LastEntry()
	if entry == nil {
		t.Fatal("nothing was logged")
	}
	assert.Equal(t, logrus.InfoLevel, entry.Level)
	assert.Equal(t, "@admin:x", entry.Data["admin"])
	assert.Equal(t, "POST", entry.Data["method"])
	assert.Equal(t, "/_synapse/admin/v1/reset_password/@a:x", entry.Data["path"], "no query string")
	assert.Equal(t, 200, entry.Data["code"])
	for _, v := range entry.Data {
		if s, ok := v.(string); ok {
			assert.NotContains(t, s, "hunter2")
			assert.NotContains(t, s, "secret")
		}
	}
	assert.Equal(t, before+1, counterValue(t, "test_handler", "200"))
}

func counterValue(t *testing.T, handler, code string) float64 {
	t.Helper()
	var m dto.Metric
	if err := adminRequests.WithLabelValues(handler, code).Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetCounter().GetValue()
}
