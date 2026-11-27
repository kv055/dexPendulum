// Package session provides the shared cookie-session layer for the dexPendulum
// dashboard login. A single scs session manager, backed by the Postgres
// `sessions` table, is constructed here so every service validates the same
// `dex_session` cookie identically (DRY).
//
// dexMonitor owns login (it creates/destroys sessions and holds the user table);
// other services only validate. Build a Manager with NewManager, wrap the handler
// chain with Manager.LoadAndSave, and gate routes with Manager.Gate.
package session

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"time"

	"github.com/alexedwards/scs/postgresstore"
	"github.com/alexedwards/scs/v2"
)

// Session data keys. Exported so the login service can populate them and any
// service can read them via the embedded scs manager.
const (
	KeyUserID   = "userID"
	KeyUsername = "username"
)

// Config configures the session cookie.
type Config struct {
	// CookieSecure sets the Secure attribute on the session cookie. Keep true in
	// prod (HTTPS); set false only for local HTTP dev, where browsers drop Secure
	// cookies served over http:// and the session would never persist.
	CookieSecure bool

	// DisableCleanup stops the store's background cleanup goroutine, which
	// otherwise runs DELETE FROM sessions every 5 minutes. Set it for
	// deployments whose DB role cannot write the sessions table (e.g. the
	// read-only public monitor): they never create sessions, and the instance
	// that owns the sessions table runs its own cleanup.
	DisableCleanup bool
}

// Manager is the shared session manager. It embeds *scs.SessionManager, so
// callers get LoadAndSave, GetInt, Put, RenewToken, Destroy, etc. directly.
type Manager struct {
	*scs.SessionManager
}

// NewManager builds the Postgres-backed session manager with an effectively
// unlimited lifetime. postgresstore expects the `sessions` table to already
// exist (dexUnifiedApi migration 026).
func NewManager(db *sql.DB, cfg Config) *Manager {
	sm := scs.New()
	if cfg.DisableCleanup {
		// 0 interval = no background cleanup goroutine.
		sm.Store = postgresstore.NewWithCleanupInterval(db, 0)
	} else {
		sm.Store = postgresstore.New(db)
	}
	sm.Lifetime = 100 * 365 * 24 * time.Hour // effectively forever
	sm.Cookie.Name = "dex_session"
	sm.Cookie.HttpOnly = true
	sm.Cookie.SameSite = http.SameSiteLaxMode
	sm.Cookie.Persist = true
	sm.Cookie.Secure = cfg.CookieSecure
	return &Manager{SessionManager: sm}
}

// Gate returns middleware that rejects a request with 401 when isProtected
// reports the path requires a login and the request carries no valid session.
// The predicate is caller-supplied so each service controls its own exemptions
// (e.g. dexMonitor exempts /api/auth/*; unified-api gates only its browser-facing
// routes). Must be wrapped by Manager.LoadAndSave so the session is in context.
func (m *Manager) Gate(isProtected func(path string) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isProtected(r.URL.Path) && m.GetInt(r.Context(), KeyUserID) == 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
