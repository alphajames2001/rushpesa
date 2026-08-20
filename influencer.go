package main

import (
	"encoding/json"
	"log"
	"net/http"
)

func (a *App) GetInfluencerMpesaBalance(w http.ResponseWriter, r *http.Request) {
	userID, _ := UserIDFromContext(r.Context())
	amount, err := a.db.GetOrCreateMockMpesa(r.Context(), userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load mock mpesa balance")
		return
	}
	// Return as "balance" to match frontend expectation
	writeSuccess(w, map[string]any{"balance": amount})
}

type influencerWithdrawRequest struct {
	Amount float64 `json:"amount"`
}

func (a *App) InfluencerWithdraw(w http.ResponseWriter, r *http.Request) {
	userID, _ := UserIDFromContext(r.Context())

	var req influencerWithdrawRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Amount <= 0 {
		writeError(w, http.StatusBadRequest, "a positive amount is required")
		return
	}

	if err := a.db.InfluencerWithdraw(r.Context(), userID, req.Amount); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// InfluencerWithdraw only touched Postgres — once a user's balance is
	// cached in Redis it's the read source of truth (see wallet.go's
	// getLiveBalance / redis.go's IncrRealBalance comment), so without this
	// the withdrawal would be silently invisible: the live balance an
	// influencer sees wouldn't drop at all until Redis happened to go cold.
	if err := a.rdb.IncrRealBalance(r.Context(), userID, -req.Amount); err != nil {
		log.Printf("influencer: failed to sync redis balance after withdrawal for %s: %v", userID, err)
	}
	// Return "newBalance" to match frontend expectation
	balance, _ := a.db.GetOrCreateMockMpesa(r.Context(), userID)
	writeSuccess(w, map[string]any{"withdrawn": req.Amount, "newBalance": balance})
}

type mockMpesaWithdrawRequest struct {
	Amount float64 `json:"amount"`
}

func (a *App) MockMpesaWithdraw(w http.ResponseWriter, r *http.Request) {
	userID, _ := UserIDFromContext(r.Context())

	var req mockMpesaWithdrawRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Amount <= 0 {
		writeError(w, http.StatusBadRequest, "a positive amount is required")
		return
	}

	if err := a.db.MockMpesaWithdraw(r.Context(), userID, req.Amount); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	balance, _ := a.db.GetOrCreateMockMpesa(r.Context(), userID)
	writeSuccess(w, map[string]any{"withdrawn": req.Amount, "newBalance": balance})
}

func (a *App) GetInfluencerTransactions(w http.ResponseWriter, r *http.Request) {
	userID, _ := UserIDFromContext(r.Context())
	txs, err := a.db.ListInfluencerTransactions(r.Context(), userID, 50)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load transactions")
		return
	}
	writeSuccess(w, txs)
}
