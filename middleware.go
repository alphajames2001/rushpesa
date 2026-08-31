package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

type ctxKey string

const (
	ctxUserID ctxKey = "userID"
	ctxRole   ctxKey = "role"
)

// AuthMiddleware verifies the Supabase-issued JWT on every protected route
// and stashes the user id + role in the request context.
//
// This used to verify with a single static HS256 secret (SUPABASE_JWT_SECRET).
// That only works if your Supabase project is still on the legacy shared
// HS256 signing secret. Newer Supabase projects default to asymmetric
// signing keys (ES256/RS256) instead — a static HS256 secret can NEVER
// verify those tokens, no matter how correct the value is, because it's
// checking the wrong kind of key entirely. That mismatch is what "invalid
// token" on every authenticated request after a successful login/signup
// almost always means.
//
// Fix: verify against Supabase's public JWKS endpoint instead of a static
// secret. This works whether the project is on legacy HS256 or the newer
// asymmetric keys, without you needing to know which mode you're in.
// a.jwks is built once at startup (see server.go) and caches/refreshes the
// keys itself, so this doesn't add a network call per request.
func (a *App) AuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, "Bearer ") {
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		tokenStr := strings.TrimPrefix(header, "Bearer ")

		claims := jwt.MapClaims{}
		_, err := jwt.ParseWithClaims(tokenStr, claims, a.jwks.Keyfunc)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid token: "+err.Error())
			return
		}

		sub, _ := claims["sub"].(string)
		userID, err := uuid.Parse(sub)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid token subject")
			return
		}

		// Role is authoritative in Postgres (profiles.role), not the JWT —
		// pull it fresh so a just-promoted admin/influencer doesn't have to
		// wait for their token to refresh. This is a consistency-path read,
		// so it's fine for it to hit Postgres directly.
		profile, err := a.db.GetProfile(r.Context(), userID)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "user not found")
			return
		}

		ctx := context.WithValue(r.Context(), ctxUserID, userID)
		ctx = context.WithValue(ctx, ctxRole, profile.Role)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// newJWKS builds the JWKS-based key source used by AuthMiddleware above.
// Call once at startup (see server.go's main()) and reuse — it refreshes
// itself on its own schedule rather than fetching per request.
func newJWKS(supabaseURL string) (keyfunc.Keyfunc, error) {
	return keyfunc.NewDefault([]string{supabaseURL + "/auth/v1/.well-known/jwks.json"})
}

// RequireRole gates a route to one or more roles. Use after AuthMiddleware.
func RequireRole(roles ...string) func(http.Handler) http.Handler {
	allowed := make(map[string]bool, len(roles))
	for _, r := range roles {
		allowed[r] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			role, _ := r.Context().Value(ctxRole).(string)
			if !allowed[role] {
				writeError(w, http.StatusForbidden, "insufficient role")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireDebugAccess gates the round-debug diagnostic route: admins always
// pass, influencers only pass if profiles.can_debug is true.
func (a *App) RequireDebugAccess(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID, _ := r.Context().Value(ctxUserID).(uuid.UUID)
		role, _ := r.Context().Value(ctxRole).(string)

		if role == "admin" {
			next.ServeHTTP(w, r)
			return
		}
		if role == "influencer" {
			profile, err := a.db.GetProfile(r.Context(), userID)
			if err == nil && profile.CanDebug {
				next.ServeHTTP(w, r)
				return
			}
		}
		writeError(w, http.StatusForbidden, "not authorized for round debug")
	})
}

func UserIDFromContext(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(ctxUserID).(uuid.UUID)
	return id, ok
}

func RoleFromContext(ctx context.Context) (string, bool) {
	role, ok := ctx.Value(ctxRole).(string)
	return role, ok
}

// RateLimit is a simple per-IP-per-route fixed-window limiter backed by
// Redis (spec §7 ratelimit:{ip}:{route}). Applied to the consistency-path
// routes (auth, wallet) — the fast path has its own protection via the
// per-user Lua-script bet lock (a user physically can't out-request the
// atomic script).
func (a *App) RateLimit(limit int, window time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := clientIP(r)
			key := fmt.Sprintf("ratelimit:%s:%s", ip, r.URL.Path)

			allowed, err := a.rdb.AllowRequest(r.Context(), key, limit, window)
			if err != nil {
				// Fail open — don't let a Redis blip take down the consistency
				// path, which is supposed to prioritize availability here.
				next.ServeHTTP(w, r)
				return
			}
			if !allowed {
				writeError(w, http.StatusTooManyRequests, "rate limit exceeded, try again shortly")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		return strings.TrimSpace(strings.Split(fwd, ",")[0])
	}
	return r.RemoteAddr
}

// ---- Response helpers shared across handlers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"success": false, "error": msg})
}

func writeSuccess(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": data})
}
