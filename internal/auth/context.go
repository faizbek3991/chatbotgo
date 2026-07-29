package auth

import (
	"context"
	"net/http"
	"time"

	"gemini-agent-go/internal/db"
)

// SessionCookieName is the cookie holding the opaque session token.
const SessionCookieName = "session_token"

type contextKey string

const userContextKey contextKey = "authenticatedUser"

// UserFromContext retrieves the logged-in user that RequireAuth attached to
// the request context.
func UserFromContext(ctx context.Context) (*db.User, bool) {
	u, ok := ctx.Value(userContextKey).(*db.User)
	return u, ok
}

// RequireAuth looks up the session named in the request cookie, attaches
// the matching user to the request context, and calls next. With no
// cookie, an unknown token, or an expired session, it redirects to /login
// instead of calling next at all.
func RequireAuth(store *db.Store, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(SessionCookieName)
		if err != nil || cookie.Value == "" {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		session, err := store.GetSession(r.Context(), cookie.Value)
		if err != nil || session.ExpiresAt.Before(time.Now()) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		user, err := store.GetUserByID(r.Context(), session.UserID)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		// Re-checked on every request (not just at login) so a block takes
		// effect immediately, and reactivating restores access immediately —
		// no session invalidation or expiry involved either way.
		if user.Status == "blocked" {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		ctx := context.WithValue(r.Context(), userContextKey, user)
		next(w, r.WithContext(ctx))
	}
}

// RequireAdmin builds on RequireAuth and additionally rejects any logged-in
// user whose role isn't "admin".
func RequireAdmin(store *db.Store, next http.HandlerFunc) http.HandlerFunc {
	return RequireAuth(store, func(w http.ResponseWriter, r *http.Request) {
		user, ok := UserFromContext(r.Context())
		if !ok || user.Role != "admin" {
			http.Error(w, "403 forbidden — admin access only", http.StatusForbidden)
			return
		}
		next(w, r)
	})
}
