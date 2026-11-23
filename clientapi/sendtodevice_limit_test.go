package clientapi

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/element-hq/dendrite/setup/config"
	"github.com/element-hq/dendrite/test"
	uapi "github.com/element-hq/dendrite/userapi/api"
)

func TestSendToDeviceLimit(t *testing.T) {
	alice := test.NewUser(t, test.WithAccountType(uapi.AccountTypeUser))
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		h, closeFn := newAdminHarness(t, dbType, func(cfg *config.Dendrite) { cfg.ClientAPI.MaxToDeviceMessages = 3 }, alice)
		defer closeFn()
		send := func(txn string, targets int) (int, string) {
			byDevice := map[string]any{}
			for i := 0; i < targets; i++ {
				byDevice["DEV"+string(rune('A'+i))] = map[string]any{"k": "v"}
			}
			code, res := h.call(h.routers.Client, alice, http.MethodPut, "/_matrix/client/v3/sendToDevice/m.room_key_request/"+txn,
				map[string]any{"messages": map[string]any{alice.ID: byDevice}})
			return code, res.Get("errcode").String()
		}
		code, _ := send("a", 3)
		assert.Equal(t, http.StatusOK, code, "at the limit")
		code, errcode := send("b", 4)
		assert.Equal(t, http.StatusRequestEntityTooLarge, code)
		assert.Equal(t, "M_TOO_LARGE", errcode)
	})
}
