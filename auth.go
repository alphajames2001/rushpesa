package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/google/uuid"
)

type signupRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Username string `json:"username"`
	Phone    string `json:"phone"` // 2547XXXXXXXX / 2541XXXXXXXX — see isValidKenyanPhone in wallet.go
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type supabaseAuthResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	User         struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	} `json:"user"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
	Msg              string `json:"msg"`
}

func (a *App) Signup(w http.ResponseWriter, r *http.Request) {
	var req signupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Email == "" || req.Password == "" || req.Username == "" {
		writeError(w, http.StatusBadRequest, "email, password, and username are required")
		return
	}
	if !isValidKenyanPhone(req.Phone) {
		writeError(w, http.StatusBadRequest, "enter a valid M-Pesa number (07... or 01...)")
		return
	}

	authResp, status, err := a.supabaseAuthRequest(r.Context(), "/auth/v1/signup", map[string]string{
		"email":    req.Email,
		"password": req.Password,
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, "auth provider error: "+err.Error())
		return
	}
	if status >= 400 {
		msg := firstNonEmpty(authResp.ErrorDescription, authResp.Msg, authResp.Error, "signup failed")
		writeError(w, status, msg)
		return
	}

	userID, err := uuid.Parse(authResp.User.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "auth provider returned invalid user id")
		return
	}

	profile, err := a.db.CreateProfile(r.Context(), userID, req.Username, req.Phone)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create profile: "+err.Error())
		return
	}

	writeSuccess(w, map[string]any{
		"token": authResp.AccessToken,
		"user": map[string]any{
			"id":          profile.ID,
			"username":    profile.Username,
			"displayName": profile.DisplayName,
			"phone":       profile.Phone,
			"role":        profile.Role,
		},
	})
}

func (a *App) Login(w http.ResponseWriter, r *http.Request) {
	a.doLogin(w, r, "")
}

func (a *App) AdminLogin(w http.ResponseWriter, r *http.Request) {
	a.doLogin(w, r, "portal")
}

func (a *App) doLogin(w http.ResponseWriter, r *http.Request, mode string) {
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	authResp, status, err := a.supabaseAuthRequest(r.Context(), "/auth/v1/token?grant_type=password", map[string]string{
		"email":    req.Email,
		"password": req.Password,
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, "auth provider error: "+err.Error())
		return
	}
	if status >= 400 {
		msg := firstNonEmpty(authResp.ErrorDescription, authResp.Msg, authResp.Error, "login failed")
		writeError(w, http.StatusUnauthorized, msg)
		return
	}

	userID, err := uuid.Parse(authResp.User.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "auth provider returned invalid user id")
		return
	}

	profile, err := a.db.GetProfile(r.Context(), userID)
	if err != nil {
		writeError(w, http.StatusNotFound, "profile not found for this account")
		return
	}

	if mode == "portal" && profile.Role != "admin" && profile.Role != "influencer" {
		writeError(w, http.StatusForbidden, "this login is for admin/influencer accounts only")
		return
	}

	writeSuccess(w, map[string]any{
		"token": authResp.AccessToken,
		"user": map[string]any{
			"id":          profile.ID,
			"username":    profile.Username,
			"displayName": profile.DisplayName,
			"phone":       profile.Phone,
			"role":        profile.Role,
			"canDebug":    profile.CanDebug,
		},
	})
}

func (a *App) GetOwnProfile(w http.ResponseWriter, r *http.Request) {
	userID, _ := UserIDFromContext(r.Context())
	profile, err := a.db.GetProfile(r.Context(), userID)
	if err != nil {
		writeError(w, http.StatusNotFound, "profile not found")
		return
	}

	// Balances come from Redis (via getLiveBalance, wallet.go), not the
	// profile row above — the row only reflects Postgres, which lags behind
	// a live bet/cashout until the round crashes and the persist worker
	// flushes it. Redis is updated the instant a bet/cashout happens.
	demo, real, err := a.getLiveBalance(r.Context(), userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load balance")
		return
	}

	// Return displayName for influencer page, and phone so the wallet page
	// can prefill (but not lock in) a default M-Pesa number.
	writeSuccess(w, map[string]any{
		"id":          profile.ID,
		"username":    profile.Username,
		"displayName": profile.DisplayName,
		"phone":       profile.Phone,
		"role":        profile.Role,
		"canDebug":    profile.CanDebug,
		"realBalance": real,
		"demoBalance": demo,
	})
}

// updateProfileRequest uses pointers so username and phone can each be
// updated independently — a request with only {"phone": "..."} leaves
// Username nil rather than accidentally clearing it, and vice versa.
type updateProfileRequest struct {
	Username *string `json:"username"`
	Phone    *string `json:"phone"`
}

func (a *App) UpdateOwnProfile(w http.ResponseWriter, r *http.Request) {
	userID, _ := UserIDFromContext(r.Context())
	var req updateProfileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Username == nil && req.Phone == nil {
		writeError(w, http.StatusBadRequest, "nothing to update")
		return
	}
	if req.Username != nil && *req.Username == "" {
		writeError(w, http.StatusBadRequest, "username is required")
		return
	}
	if req.Phone != nil && !isValidKenyanPhone(*req.Phone) {
		writeError(w, http.StatusBadRequest, "enter a valid M-Pesa number (07... or 01...)")
		return
	}
	if err := a.db.UpdateProfile(r.Context(), userID, req.Username, req.Phone); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update profile")
		return
	}
	resp := map[string]any{}
	if req.Username != nil {
		resp["username"] = *req.Username
	}
	if req.Phone != nil {
		resp["phone"] = *req.Phone
	}
	writeSuccess(w, resp)
}

func (a *App) RequestPasswordReset(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["email"] == "" {
		writeError(w, http.StatusBadRequest, "email is required")
		return
	}
	_, status, err := a.supabaseAuthRequest(r.Context(), "/auth/v1/recover", map[string]string{"email": body["email"]})
	if err != nil {
		writeError(w, http.StatusBadGateway, "auth provider error: "+err.Error())
		return
	}
	if status >= 400 {
		writeError(w, status, "failed to send password reset email")
		return
	}
	writeSuccess(w, map[string]string{"message": "password reset email sent"})
}

func (a *App) supabaseAuthRequest(ctx context.Context, path string, body map[string]string) (*supabaseAuthResponse, int, error) {
	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.SupabaseURL+path, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("apikey", a.cfg.SupabaseAnonKey)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}

	var parsed supabaseAuthResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("unexpected auth response: %s", string(raw))
	}
	return &parsed, resp.StatusCode, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
