package routing

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/element-hq/dendrite/clientapi/auth"
	"github.com/element-hq/dendrite/internal/httputil"
	"github.com/element-hq/dendrite/setup/config"
	userapi "github.com/element-hq/dendrite/userapi/api"
	"github.com/gorilla/mux"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/matrix-org/util"
)

const (
	// MSC4388 asks for 120 to 300 seconds.
	rendezvousLifetime = 300 * time.Second
	// The data field may hold at most 4096 bytes.
	rendezvousMaxData = 4096
	// Bounds the memory one request can use. Leaves room for the JSON around the data.
	rendezvousMaxBody = 16 * 1024
	// A normal exchange needs a handful of transaction IDs.
	rendezvousMaxTxns     = 64
	rendezvousMaxSessions = 1000

	errConcurrentWrite         spec.MatrixErrorCode = "M_CONCURRENT_WRITE"
	errConcurrentWriteUnstable spec.MatrixErrorCode = "IO_ELEMENT_MSC4388_CONCURRENT_WRITE"
	errTooLarge                spec.MatrixErrorCode = "M_TOO_LARGE"
)

// The opaque identifier grammar of the Matrix specification.
var opaqueIdentifier = regexp.MustCompile(`^[0-9a-zA-Z._~-]{1,255}$`)

type rendezvousSession struct {
	data    string
	seq     int64
	expires time.Time
	// The sequence token each successful send produced, by transaction ID.
	txns map[string]string
}

// rendezvousStore keeps the sessions in memory: they live for minutes and the two devices
// involved just start again if the server restarts.
type rendezvousStore struct {
	mu       sync.Mutex
	sessions map[string]*rendezvousSession
	now      func() time.Time
}

func newRendezvousStore() *rendezvousStore {
	return &rendezvousStore{sessions: map[string]*rendezvousSession{}, now: time.Now}
}

var errTooManyRendezvous = errors.New("too many rendezvous sessions")

// sweep drops expired sessions. The caller holds the lock.
func (s *rendezvousStore) sweep() {
	now := s.now()
	for id, sess := range s.sessions {
		if !now.Before(sess.expires) {
			delete(s.sessions, id)
		}
	}
}

// get returns the live session. The caller holds the lock.
func (s *rendezvousStore) get(id string) *rendezvousSession {
	sess, ok := s.sessions[id]
	if !ok {
		return nil
	}
	if !s.now().Before(sess.expires) {
		delete(s.sessions, id)
		return nil
	}
	return sess
}

func (s *rendezvousStore) create(data string) (id string, sess *rendezvousSession, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep()
	if len(s.sessions) >= rendezvousMaxSessions {
		return "", nil, errTooManyRendezvous
	}
	b := make([]byte, 16)
	if _, err = rand.Read(b); err != nil {
		return "", nil, err
	}
	id = base64.RawURLEncoding.EncodeToString(b)
	sess = &rendezvousSession{data: data, seq: 1, expires: s.now().Add(rendezvousLifetime), txns: map[string]string{}}
	s.sessions[id] = sess
	return id, sess, nil
}

type rendezvousAPI struct {
	cfg        *config.ClientAPI
	userAPI    userapi.ClientUserAPI
	limits     *httputil.RateLimits
	store      *rendezvousStore
	concurrent spec.MatrixErrorCode
}

// setupRendezvousRoutes registers the rendezvous endpoints under the given prefix of a router. Without
// the feature enabled they answer M_UNRECOGNIZED, as the specification asks.
func setupRendezvousRoutes(
	router *mux.Router, cfg *config.ClientAPI, userAPI userapi.ClientUserAPI,
	limits *httputil.RateLimits, store *rendezvousStore, unstable bool,
) {
	r := &rendezvousAPI{cfg: cfg, userAPI: userAPI, limits: limits, store: store, concurrent: errConcurrentWrite}
	if unstable {
		r.concurrent = errConcurrentWriteUnstable
	}
	handle := func(name string, f func(*http.Request) util.JSONResponse) http.Handler {
		return httputil.MakeExternalAPI(name, func(req *http.Request) util.JSONResponse {
			if !cfg.Rendezvous.Enabled {
				return util.JSONResponse{Code: http.StatusNotFound, JSON: spec.Unrecognized("The rendezvous API is not enabled")}
			}
			if limited := r.limits.Limit(req, nil); limited != nil {
				return *limited
			}
			return f(req)
		})
	}
	router.Handle("/rendezvous", handle("rendezvous_discover", r.discover)).Methods(http.MethodGet, http.MethodOptions)
	router.Handle("/rendezvous", handle("rendezvous_create", r.create)).Methods(http.MethodPost, http.MethodOptions)
	router.Handle("/rendezvous/{rendezvousId}", handle("rendezvous_get", r.get)).Methods(http.MethodGet, http.MethodOptions)
	router.Handle("/rendezvous/{rendezvousId}", handle("rendezvous_delete", r.delete)).Methods(http.MethodDelete, http.MethodOptions)
	router.Handle("/rendezvous/{rendezvousId}/{txnId}", handle("rendezvous_send", r.send)).Methods(http.MethodPut, http.MethodOptions)
}

// authenticated reports whether the request carries a valid access token. A token that is
// present but not valid is an error, not an anonymous request.
func (r *rendezvousAPI) authenticated(req *http.Request) (bool, *util.JSONResponse) {
	if req.Header.Get("Authorization") == "" && req.URL.Query().Get("access_token") == "" {
		return false, nil
	}
	if _, errRes := auth.VerifyUserFromRequest(req, r.userAPI); errRes != nil {
		return false, errRes
	}
	return true, nil
}

func (r *rendezvousAPI) mayCreate(req *http.Request) (bool, *util.JSONResponse) {
	if r.cfg.Rendezvous.AllowUnauthenticated {
		// Still refuse a bad token if one was sent.
		_, errRes := r.authenticated(req)
		return true, errRes
	}
	return r.authenticated(req)
}

func (r *rendezvousAPI) discover(req *http.Request) util.JSONResponse {
	ok, errRes := r.mayCreate(req)
	if errRes != nil {
		return *errRes
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: struct {
		CreateAvailable bool `json:"create_available"`
	}{ok}}
}

// readData reads a body of the form {"data": "..."} (plus the extra fields in dst) and checks the data size.
func readRendezvousBody(w io.Reader, dst any) *util.JSONResponse {
	body, err := io.ReadAll(io.LimitReader(w, rendezvousMaxBody+1))
	if err != nil {
		return &util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.NotJSON("Could not read the request body")}
	}
	if len(body) > rendezvousMaxBody {
		return &util.JSONResponse{Code: http.StatusRequestEntityTooLarge, JSON: spec.MatrixError{ErrCode: errTooLarge, Err: "The request body is too large"}}
	}
	if !json.Valid(body) {
		return &util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.NotJSON("The request body is not JSON")}
	}
	if err = json.Unmarshal(body, dst); err != nil {
		return &util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.BadJSON(err.Error())}
	}
	return nil
}

func tooLarge() util.JSONResponse {
	return util.JSONResponse{
		Code: http.StatusRequestEntityTooLarge,
		JSON: spec.MatrixError{ErrCode: errTooLarge, Err: "The data is larger than " + strconv.Itoa(rendezvousMaxData) + " bytes"},
	}
}

func (r *rendezvousAPI) create(req *http.Request) util.JSONResponse {
	ok, errRes := r.mayCreate(req)
	if errRes != nil {
		return *errRes
	}
	if !ok {
		return util.JSONResponse{Code: http.StatusForbidden, JSON: spec.Forbidden("You may not create a rendezvous session")}
	}
	var body struct {
		Data *string `json:"data"`
	}
	if errRes = readRendezvousBody(req.Body, &body); errRes != nil {
		return *errRes
	}
	if body.Data == nil {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.BadJSON("Missing data")}
	}
	if len(*body.Data) > rendezvousMaxData {
		return tooLarge()
	}
	id, sess, err := r.store.create(*body.Data)
	switch {
	case errors.Is(err, errTooManyRendezvous):
		return util.JSONResponse{Code: http.StatusTooManyRequests, JSON: spec.LimitExceeded("Too many rendezvous sessions", 10000)}
	case err != nil:
		return internalError(req, "creating a rendezvous session", err)
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: struct {
		ID            string `json:"id"`
		SequenceToken string `json:"sequence_token"`
		ExpiresInMS   int64  `json:"expires_in_ms"`
	}{id, strconv.FormatInt(sess.seq, 10), rendezvousLifetime.Milliseconds()}}
}

func rendezvousNotFound() util.JSONResponse {
	return util.JSONResponse{Code: http.StatusNotFound, JSON: spec.NotFound("Unknown rendezvous session")}
}

func (r *rendezvousAPI) send(req *http.Request) util.JSONResponse {
	vars := mux.Vars(req)
	id, txnID := vars["rendezvousId"], vars["txnId"]
	if !opaqueIdentifier.MatchString(txnID) {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("txnId does not match the opaque identifier grammar")}
	}

	// Read the body before taking the lock, so a slow client cannot hold it.
	var body struct {
		SequenceToken *string `json:"sequence_token"`
		Data          *string `json:"data"`
	}
	bodyErr := readRendezvousBody(req.Body, &body)

	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	sess := r.store.get(id)
	if sess == nil {
		return rendezvousNotFound()
	}
	// A repeated transaction gets the earlier answer, whatever the new request looks like.
	if token, ok := sess.txns[txnID]; ok {
		return util.JSONResponse{Code: http.StatusOK, JSON: struct {
			SequenceToken string `json:"sequence_token"`
		}{token}}
	}
	if bodyErr != nil {
		return *bodyErr
	}
	if body.SequenceToken == nil || body.Data == nil {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.BadJSON("Missing sequence_token or data")}
	}
	if len(*body.Data) > rendezvousMaxData {
		return tooLarge()
	}
	if *body.SequenceToken != strconv.FormatInt(sess.seq, 10) {
		return util.JSONResponse{Code: http.StatusConflict, JSON: spec.MatrixError{
			ErrCode: r.concurrent, Err: "The rendezvous session has changed, fetch the latest data first",
		}}
	}
	if len(sess.txns) >= rendezvousMaxTxns {
		return util.JSONResponse{Code: http.StatusTooManyRequests, JSON: spec.LimitExceeded("Too many sends to this rendezvous session", 10000)}
	}
	sess.seq++
	sess.data = *body.Data
	token := strconv.FormatInt(sess.seq, 10)
	sess.txns[txnID] = token
	return util.JSONResponse{Code: http.StatusOK, JSON: struct {
		SequenceToken string `json:"sequence_token"`
	}{token}}
}

func (r *rendezvousAPI) get(req *http.Request) util.JSONResponse {
	// A browser navigating to the URL must not be shown content somebody else put there.
	if req.Header.Get("Sec-Fetch-Mode") == "navigate" || req.Header.Get("Sec-Fetch-Dest") == "document" {
		return util.JSONResponse{Code: http.StatusForbidden, JSON: spec.Forbidden("This resource cannot be opened in a browser")}
	}
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	sess := r.store.get(mux.Vars(req)["rendezvousId"])
	if sess == nil {
		return rendezvousNotFound()
	}
	return util.JSONResponse{
		Code: http.StatusOK,
		JSON: struct {
			Data          string `json:"data"`
			SequenceToken string `json:"sequence_token"`
			ExpiresInMS   int64  `json:"expires_in_ms"`
		}{sess.data, strconv.FormatInt(sess.seq, 10), sess.expires.Sub(r.store.now()).Milliseconds()},
		Headers: map[string]string{"Cache-Control": "no-store"},
	}
}

func (r *rendezvousAPI) delete(req *http.Request) util.JSONResponse {
	id := mux.Vars(req)["rendezvousId"]
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if r.store.get(id) == nil {
		return rendezvousNotFound()
	}
	delete(r.store.sessions, id)
	return emptyOK()
}
