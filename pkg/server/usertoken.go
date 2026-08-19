// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.graveland.dev/rafiki/pkg/users"
)

// DefaultAuthCacheTTL bounds how long a verified token is trusted without
// re-checking the store. It is also exactly the revocation lag: `user rm`
// takes effect on the face within this window.
const DefaultAuthCacheTTL = 5 * time.Second

// UserTokenAuth authenticates proxy-face requests against the users table,
// plus the daemon's per-boot child secret.
//
// Tokens arrive via `X-Rafiki-Token`, `Authorization: Bearer` or `x-api-key`
// — Anthropic-protocol clients like Claude Code send the middle or last.
// `X-Rafiki-Token` additionally means the request's Authorization header
// belongs to the caller and is forwarded upstream (see PassthroughCredential).
//
// The cache exists because this runs PER REQUEST, unlike the control plane's
// once-per-connection handshake. It is keyed by digest rather than plaintext
// so a heap dump does not hand over live credentials.
type UserTokenAuth struct {
	store      users.Store
	childToken string
	ttl        time.Duration

	mu    sync.Mutex
	cache map[string]cachedIdentity
}

type cachedIdentity struct {
	id      Identity
	expires time.Time
}

func NewUserTokenAuth(store users.Store, childToken string, ttl time.Duration) *UserTokenAuth {
	if ttl <= 0 {
		ttl = DefaultAuthCacheTTL
	}
	return &UserTokenAuth{
		store:      store,
		childToken: childToken,
		ttl:        ttl,
		cache:      make(map[string]cachedIdentity),
	}
}

// errAuthUnavailable distinguishes "I could not check" from "invalid".
var errAuthUnavailable = errors.New("identity store unavailable")

func (a *UserTokenAuth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, passthrough := credential(r)
		if token == "" {
			http.Error(w, "missing credentials (Authorization: Bearer, x-api-key or X-Rafiki-Token)", http.StatusUnauthorized)
			return
		}
		id, err := a.resolve(r.Context(), token)
		if errors.Is(err, errAuthUnavailable) {
			// 503, never 401: a 401 tells the client its credential is bad
			// and clients respond by discarding it. A database blip must
			// not log out the whole fleet. The store's error text stays
			// here — this caller has not proved who it is.
			http.Error(w, "identity store unavailable", http.StatusServiceUnavailable)
			return
		}
		if err != nil {
			http.Error(w, "unknown token", http.StatusUnauthorized)
			return
		}

		ctx := WithIdentity(r.Context(), &id)
		if passthrough {
			// X-Rafiki-Token means "Authorization is mine, bill it
			// upstream", so both of these fail CLOSED. Proceeding would
			// charge the daemon's key — the exact outcome the caller opted
			// out of — with no error and no log to notice it by.
			cred := r.Header.Get("Authorization")
			if cred == "" {
				http.Error(w, "X-Rafiki-Token means passthrough auth, but no Authorization credential was supplied to forward upstream", http.StatusUnauthorized)
				return
			}
			// Never relay a rafiki credential to the upstream provider: a
			// client that puts the same token in both headers would ship
			// our secret to a third party and get an opaque 401 back.
			switch _, err := a.resolve(r.Context(), strings.TrimPrefix(cred, "Bearer ")); {
			case err == nil:
				http.Error(w, "Authorization carries a rafiki token, not an upstream credential; passthrough auth needs your provider credential there", http.StatusUnauthorized)
				return
			case errors.Is(err, errAuthUnavailable):
				// Could not check — which is not the same as "checked, and it
				// is not ours". This guard exists to stop rafiki's own
				// credential being shipped to a third party, so an
				// unanswerable check fails CLOSED. Letting it through would
				// mean a database blip is all it takes to leak the daemon's
				// token upstream, where it earns an opaque 401 and no log.
				http.Error(w, "identity store unavailable", http.StatusServiceUnavailable)
				return
			}
			ctx = WithPassthroughCredential(ctx, cred)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// resolve returns the identity for token, consulting the cache first.
func (a *UserTokenAuth) resolve(ctx context.Context, token string) (Identity, error) {
	// The child secret never reaches the store: it is a daemon-internal
	// credential minted per boot, and it must keep working in bootstrap mode
	// when no users exist at all. Constant-time so it is not timing-probeable.
	if a.childToken != "" &&
		subtle.ConstantTimeCompare([]byte(token), []byte(a.childToken)) == 1 {
		return Identity{}, nil
	}

	// A nil store is a supported configuration (RAFIKI_DB unset): it means
	// "no users exist", so every user token is unknown rather than a panic.
	if a.store == nil {
		return Identity{}, users.ErrNotFound
	}

	key := users.HashToken(token)
	now := time.Now()

	a.mu.Lock()
	if e, ok := a.cache[key]; ok && now.Before(e.expires) {
		a.mu.Unlock()
		return e.id, nil
	}
	a.mu.Unlock()

	uid, err := a.store.Authenticate(ctx, token)
	if errors.Is(err, users.ErrNotFound) {
		return Identity{}, users.ErrNotFound
	}
	if err != nil {
		return Identity{}, errAuthUnavailable
	}

	id := Identity{UserID: uid.UserID, Username: uid.Username}
	a.mu.Lock()
	a.cache[key] = cachedIdentity{id: id, expires: now.Add(a.ttl)}
	// Opportunistic sweep: entries are tiny and the population is the number
	// of users, so a full scan on insert is cheaper than a timer goroutine.
	for k, e := range a.cache {
		if now.After(e.expires) {
			delete(a.cache, k)
		}
	}
	a.mu.Unlock()
	return id, nil
}
