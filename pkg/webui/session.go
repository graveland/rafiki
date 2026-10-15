// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

// sessionTokenBytes is the entropy of a launch code or a session secret.
const sessionTokenBytes = 32

// maxSessionBodyBytes caps the (tiny) launch-code exchange body.
const maxSessionBodyBytes = 4 << 10

// sessionStore holds the live launch codes (single-use) and session secrets.
type sessionStore struct {
	mu      sync.Mutex
	codes   map[string]struct{}
	secrets map[string]struct{}
}

func newSessionStore() *sessionStore {
	return &sessionStore{
		codes:   make(map[string]struct{}),
		secrets: make(map[string]struct{}),
	}
}

// randomToken mints 32 bytes of crypto/rand as base64.RawURLEncoding.
func randomToken() (string, error) {
	b := make([]byte, sessionTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// LaunchCode mints a fresh single-use launch code. It is valid until exchanged
// once.
func (s *Server) LaunchCode() string {
	code, err := randomToken()
	if err != nil {
		// A failing OS CSPRNG means no session can be minted safely.
		panic(fmt.Sprintf("rafiki web: mint launch code: %v", err))
	}
	s.sessions.mu.Lock()
	s.sessions.codes[code] = struct{}{}
	s.sessions.mu.Unlock()
	return code
}

// exchange consumes code and mints a session secret, or reports ok=false when
// the code is unknown or already used.
func (st *sessionStore) exchange(code string) (string, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if _, ok := st.codes[code]; !ok {
		return "", false
	}
	delete(st.codes, code)
	secret, err := randomToken()
	if err != nil {
		panic(fmt.Sprintf("rafiki web: mint session secret: %v", err))
	}
	st.secrets[secret] = struct{}{}
	return secret, true
}

// valid reports whether secret is a live session secret, comparing in constant
// time against each member (the set is tiny).
func (st *sessionStore) valid(secret string) bool {
	if secret == "" {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	for s := range st.secrets {
		if subtle.ConstantTimeCompare([]byte(s), []byte(secret)) == 1 {
			return true
		}
	}
	return false
}

// serveSession exchanges a launch code for a session secret. It runs after the
// Host and Origin gate checks but needs no X-Rafiki-Web header.
func (s *Server) serveSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{
			Code:    "unimplemented",
			Message: "rafiki web: POST /_rafiki/session to exchange a launch code",
		})
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSessionBodyBytes)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{
			Code:    "invalid_argument",
			Message: "rafiki web: malformed session request body",
		})
		return
	}
	secret, ok := s.sessions.exchange(req.Code)
	if !ok {
		writeJSON(w, http.StatusGone, errorBody{
			Code:    "failed_precondition",
			Message: "this rafiki web link was already used or is not valid; run rafiki web again for a new one",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"secret": secret})
}
