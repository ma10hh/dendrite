package clientapi

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/element-hq/dendrite/test"
	uapi "github.com/element-hq/dendrite/userapi/api"
)

func TestAdminServerACL(t *testing.T) {
	admin := test.NewUser(t, test.WithAccountType(uapi.AccountTypeAdmin))
	bob := test.NewUser(t, test.WithAccountType(uapi.AccountTypeUser))
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		h, closeFn := newAdminHarness(t, dbType, nil, admin, bob)
		defer closeFn()

		plain := test.NewRoom(t, admin)
		withACL := test.NewRoom(t, admin)
		withACL.CreateAndInsert(t, admin, "m.room.server_acl", map[string]any{"allow": []string{"*"}, "deny": []string{"bad.example"}}, test.WithStateKey(""))
		h.sendRoom(plain)
		h.sendRoom(withACL)

		code, _ := h.call(h.routers.DendriteAdmin, bob, http.MethodGet, "/_dendrite/admin/serverACL/"+plain.ID, nil)
		assert.Equal(t, http.StatusForbidden, code, "only admins")

		code, res := h.call(h.routers.DendriteAdmin, admin, http.MethodGet, "/_dendrite/admin/serverACL/"+plain.ID, nil)
		require.Equal(t, http.StatusOK, code, res.Raw)
		assert.Equal(t, "null", res.Get("acl").Raw, "no ACL")

		code, res = h.call(h.routers.DendriteAdmin, admin, http.MethodGet, "/_dendrite/admin/serverACL/"+withACL.ID+"?server=bad.example", nil)
		require.Equal(t, http.StatusOK, code, res.Raw)
		assert.Equal(t, "bad.example", res.Get("acl.deny.0").String())
		assert.True(t, res.Get("server.banned").Bool(), "the denied server is banned")
		code, res = h.call(h.routers.DendriteAdmin, admin, http.MethodGet, "/_dendrite/admin/serverACL/"+withACL.ID+"?server=good.example", nil)
		require.Equal(t, http.StatusOK, code)
		assert.False(t, res.Get("server.banned").Bool())

		code, _ = h.call(h.routers.DendriteAdmin, admin, http.MethodGet, "/_dendrite/admin/serverACL/not-a-room", nil)
		assert.Equal(t, http.StatusBadRequest, code)
	})
}
