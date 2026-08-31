package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func (a *App) GetAdminStats(w http.ResponseWriter, r *http.Request) {
	stats, err := a.db.GetAdminStats(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load stats")
		return
	}
	writeSuccess(w, stats)
}

func (a *App) ListUsers(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 {
		limit = l
	}
	users, err := a.db.ListUsers(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list users")
		return

	}
	// Return as object with "users" key to match dashboard expectation
	writeSuccess(w, map[string]any{"users": users})
}

type roleChangeRequest struct {
	Role string `json:"role"`
}

func (a *App) UpdateUserRole(w http.ResponseWriter, r *http.Request) {
	adminID, _ := UserIDFromContext(r.Context())
	userID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}

	var req roleChangeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	profile, credited, err := a.db.SetRole(r.Context(), adminID, userID, req.Role)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if credited > 0 {
		if err := a.rdb.IncrRealBalance(r.Context(), userID, credited); err != nil {
			log.Printf("admin: failed to sync redis balance after influencer credit for %s: %v", userID, err)
		}
	}
	writeSuccess(w, profile)
}

type debugAccessRequest struct {
	Enabled bool `json:"enabled"`
}

func (a *App) UpdateDebugAccess(w http.ResponseWriter, r *http.Request) {
	userID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}
	var req debugAccessRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := a.db.SetDebugAccess(r.Context(), userID, req.Enabled); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeSuccess(w, map[string]any{"userId": userID, "canDebug": req.Enabled})
}

func (a *App) ListPendingWithdrawals(w http.ResponseWriter, r *http.Request) {
	withdrawals, err := a.db.ListPendingWithdrawals(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list pending withdrawals")
		return
	}
	// Return as object with "withdrawals" key and ensure it's never null
	if withdrawals == nil {
		withdrawals = []DashboardWithdrawal{}
	}
	writeSuccess(w, map[string]any{"withdrawals": withdrawals})
}

type withdrawalReviewRequest struct {
	ID     string `json:"id"`
	Action string `json:"action"`
}

func (a *App) ReviewWithdrawal(w http.ResponseWriter, r *http.Request) {
	adminID, _ := UserIDFromContext(r.Context())

	var req withdrawalReviewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	txID, err := uuid.Parse(req.ID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid transaction id")
		return
	}
	if req.Action != "approve" && req.Action != "reject" {
		writeError(w, http.StatusBadRequest, "action must be 'approve' or 'reject'")
		return
	}

	withdrawUserID, debited, err := a.db.ReviewWithdrawal(r.Context(), adminID, txID, req.Action == "approve")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if debited > 0 {
		if err := a.rdb.IncrRealBalance(r.Context(), withdrawUserID, -debited); err != nil {
			log.Printf("admin: failed to sync redis balance after withdrawal approval %s: %v", txID, err)
		}
	}
	writeSuccess(w, map[string]string{"id": req.ID, "status": req.Action + "d"})
}

func (a *App) ListTransactions(w http.ResponseWriter, r *http.Request) {
	txType := r.URL.Query().Get("type")
	status := r.URL.Query().Get("status")
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	limit := 20
	offset := (page - 1) * limit

	// If status filter is set but not handled by DB, we filter in memory
	txs, total, err := a.db.ListTransactions(r.Context(), txType, limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list transactions")
		return
	}

	// Filter by status if provided
	if status != "" {
		filtered := make([]DashboardTransaction, 0)
		for _, tx := range txs {
			if tx.Status == status {
				filtered = append(filtered, tx)
			}
		}
		txs = filtered
		total = len(filtered)
	}

	if txs == nil {
		txs = []DashboardTransaction{}
	}

	totalPages := (total + limit - 1) / limit
	if totalPages < 1 {
		totalPages = 1
	}

	writeSuccess(w, map[string]any{
		"transactions": txs,
		"page":         page,
		"totalPages":   totalPages,
		"total":        total,
	})
}

func (a *App) ListInfluencerWithdrawals(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.ListInfluencerWithdrawals(r.Context(), 100)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list influencer withdrawals")
		return
	}
	if rows == nil {
		rows = []map[string]any{}
	}
	// Return as object with "withdrawals" key
	writeSuccess(w, map[string]any{"withdrawals": rows})
}

// MarkInfluencerWithdrawal handles POST /admin/influencer-withdrawals/:id/:action
func (a *App) MarkInfluencerWithdrawal(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	action := chi.URLParam(r, "action")
	status := ""
	switch action {
	case "mark-sent":
		status = "sent"
	case "mark-failed":
		status = "failed"
	default:
		writeError(w, http.StatusBadRequest, "expected an action like mark-sent or mark-failed")
		return
	}
	if err := a.db.MarkInfluencerWithdrawalStatus(r.Context(), id, status); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeSuccess(w, map[string]string{"id": id.String(), "status": status})
}

// ---- Payment channel provisioning (Palpluss channel rotation) ----
//
// Refactor: "We need to provision for multiple palpluss chanel ids, a
// chanel ID will direct deposits to a specific wallet, we have more than
// one, we need to be able to rotate them from the dashboard, so provision
// endpoints to see how many we have, and pick the preffered as an admin."
//
// The pool of available channel IDs is configured via the
// PALPLUSS_CHANNEL_IDS env var (see server.go's parsePalplussChannels);
// which one is currently "active" is stored in Redis (see redis.go's
// Get/SetActivePalplussChannel) so a rotation takes effect immediately and
// survives restarts, without needing a redeploy. This only affects the
// Palpluss provider — Daraja has no equivalent notion of channels.

type paymentChannelView struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Active bool   `json:"active"`
}

// ListPaymentChannels shows every configured Palpluss channel and which one
// is currently active, so the dashboard can render the rotation picker.
func (a *App) ListPaymentChannels(w http.ResponseWriter, r *http.Request) {
	channels := a.cfg.PalplussChannels
	if len(channels) == 0 && a.cfg.PalplussChannelID != "" {
		// No rotation pool configured — fall back to showing the single
		// legacy channel as the only (and therefore active) option.
		channels = []PalplussChannel{{ID: a.cfg.PalplussChannelID, Label: a.cfg.PalplussChannelID}}
	}

	active, err := a.rdb.GetActivePalplussChannel(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load active payment channel")
		return
	}
	if active == "" {
		if len(channels) > 0 {
			active = channels[0].ID
		} else {
			active = a.cfg.PalplussChannelID
		}
	}

	out := make([]paymentChannelView, 0, len(channels))
	for _, c := range channels {
		out = append(out, paymentChannelView{ID: c.ID, Label: c.Label, Active: c.ID == active})
	}
	writeSuccess(w, map[string]any{
		"provider": "palpluss",
		"channels": out,
		"active":   active,
	})
}

type setActiveChannelRequest struct {
	ChannelID string `json:"channelId"`
}

// SetActivePalplussChannel lets an admin pick which configured channel new
// deposits should route through. Only IDs present in PALPLUSS_CHANNEL_IDS
// (or the legacy PALPLUSS_CHANEL_ID) are accepted, so this can't be used to
// point deposits at an arbitrary, unconfigured channel.
func (a *App) SetActivePalplussChannel(w http.ResponseWriter, r *http.Request) {
	var req setActiveChannelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ChannelID == "" {
		writeError(w, http.StatusBadRequest, "channelId is required")
		return
	}

	valid := req.ChannelID == a.cfg.PalplussChannelID
	for _, c := range a.cfg.PalplussChannels {
		if c.ID == req.ChannelID {
			valid = true
			break
		}
	}
	if !valid {
		writeError(w, http.StatusBadRequest, "unknown channel id — must be one of the configured PALPLUSS_CHANNEL_IDS")
		return
	}

	if err := a.rdb.SetActivePalplussChannel(r.Context(), req.ChannelID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to set active payment channel")
		return
	}
	writeSuccess(w, map[string]any{"provider": "palpluss", "active": req.ChannelID})
}
