package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"math"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
)

// game.go is the fast path described in spec §4.1. Two rules govern
// everything in this file:
//  1. Bet placement and cashout never block on Postgres — Redis (via Lua
//     scripts in redis.go) is authoritative for balance/bet state during a
//     live round, and the HTTP response returns the instant Redis confirms.
//  2. Round state broadcast is one-way, over plain WebSocket, and carries no
//     request/response semantics — clients only ever receive from it.
//
// NOTE on the existing round-debug.html template: it loads socket.io-client
// v4, which speaks the Engine.IO protocol, not plain WebSocket frames. This
// engine broadcasts plain WS JSON envelopes {"event": ..., "data": ...} —
// simpler and dependency-free on the Go side. The admin template's socket
// setup needs a small swap from `io(API_URL, ...)` to a native
// `new WebSocket(...)` with matching event dispatch. Flagging this explicitly
// rather than silently pulling in a full Engine.IO server implementation.

const (
	minBetKES         = 10.0
	maxCashoutKES     = 1000000.0
	waitingPhaseSecs  = 5
	tickInterval      = 100 * time.Millisecond
	crashGrowthPerSec = 0.09 // tunable curve steepness
)

type GameEngine struct {
	db        *DB
	rdb       *RDB
	hub       *WSHub
	houseEdge float64

	mu           sync.RWMutex
	currentRound int64
}

func NewGameEngine(db *DB, rdb *RDB, hub *WSHub, houseEdge float64) *GameEngine {
	return &GameEngine{db: db, rdb: rdb, hub: hub, houseEdge: houseEdge}
}

// Run drives the round lifecycle forever. Call this in its own goroutine
// from server.go at startup.
func (g *GameEngine) Run(ctx context.Context) {
	roundID, err := g.seedRoundCounter(ctx)
	if err != nil {
		log.Fatalf("game: failed to seed round counter: %v", err)
	}
	g.currentRound = roundID

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		g.playRound(ctx)
	}
}

// seedRoundCounter avoids round-id collisions across restarts by starting
// one above whatever's already durably persisted in Postgres.
func (g *GameEngine) seedRoundCounter(ctx context.Context) (int64, error) {
	var maxID int64
	err := g.db.pool.QueryRow(ctx, `select coalesce(max(id), 0) from rounds`).Scan(&maxID)
	if err != nil {
		return 0, err
	}
	return maxID + 1, nil
}

func (g *GameEngine) playRound(ctx context.Context) {
	roundID := g.currentRound

	serverSeed, err := randomHex(32)
	if err != nil {
		log.Printf("game: failed to generate server seed: %v", err)
		time.Sleep(time.Second)
		return
	}
	serverSeedHash := sha256Hex(serverSeed)
	crashPoint := deriveCrashPoint(serverSeed, roundID, g.houseEdge)

	// ---- Waiting phase: betting window open, crash point committed but hidden ----
	state := RoundState{
		ID:             roundID,
		Phase:          "waiting",
		Multiplier:     1.0,
		CrashPoint:     crashPoint, // stored, never broadcast publicly this phase
		ServerSeedHash: serverSeedHash,
		Countdown:      waitingPhaseSecs,
	}
	if err := g.rdb.SetCurrentRound(ctx, state); err != nil {
		log.Printf("game: failed to set round state: %v", err)
	}
	g.hub.Broadcast("round:waiting", publicRoundView(state))

	for i := waitingPhaseSecs; i > 0; i-- {
		time.Sleep(time.Second)
		state.Countdown = i - 1
		g.rdb.SetCurrentRound(ctx, state)
	}

	// ---- Running phase ----
	state.Phase = "running"
	state.StartedAt = time.Now()
	state.Multiplier = 1.0
	g.rdb.SetCurrentRound(ctx, state)
	g.hub.Broadcast("round:started", publicRoundView(state))

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	for range ticker.C {
		elapsed := time.Since(state.StartedAt).Seconds()
		state.Multiplier = math.Exp(crashGrowthPerSec * elapsed)

		if state.Multiplier >= crashPoint {
			state.Multiplier = crashPoint
			break
		}
		g.rdb.SetCurrentRound(ctx, state)
		g.hub.Broadcast("round:tick", map[string]any{"id": roundID, "multiplier": round2(state.Multiplier)})
	}

	// ---- Crashed ----
	state.Phase = "crashed"
	crashedAt := time.Now()
	g.rdb.SetCurrentRound(ctx, state)
	g.rdb.PushRoundHistory(ctx, crashPoint)
	g.hub.Broadcast("round:crashed", map[string]any{"id": roundID, "crashPoint": round2(crashPoint)})

	g.settleRound(ctx, roundID, RoundRecord{
		ID:             roundID,
		ServerSeed:     serverSeed,
		ServerSeedHash: serverSeedHash,
		Nonce:          roundID,
		CrashPoint:     crashPoint,
		StartedAt:      state.StartedAt,
		CrashedAt:      crashedAt,
	})

	time.Sleep(2 * time.Second) // brief pause showing the crashed result before next round
	g.rdb.ClearCurrentRound(ctx)
	g.currentRound++
}

// settleRound closes out any bets still 'active' as losses, snapshots
// balances, and enqueues the whole round for the write-behind persistence
// worker (spec §4.1) rather than writing to Postgres inline here.
func (g *GameEngine) settleRound(ctx context.Context, roundID int64, round RoundRecord) {
	entries, err := g.rdb.GetRoundBets(ctx, roundID)
	if err != nil {
		log.Printf("game: failed to load round bets for settlement: %v", err)
		return
	}

	bets := make([]BetRecord, 0, len(entries))
	for _, e := range entries {
		userID, err := uuid.Parse(e.UserID)
		if err != nil {
			continue
		}
		betID, err := uuid.Parse(e.BetID)
		if err != nil {
			betID = uuid.New()
		}

		status := e.Status
		if status == "active" {
			status = "lost" // never cashed out before crash
		}

		demo, real, _ := g.rdb.GetBalance(ctx, userID)
		profile, perr := g.db.GetProfile(ctx, userID)
		isInfluencer := perr == nil && profile.Role == "influencer"

		balAfter := real
		if e.IsDemo {
			balAfter = demo
		}

		bets = append(bets, BetRecord{
			ID: betID, UserID: userID, Box: e.Box, Amount: e.Amount,
			CashoutMultiplier: e.CashoutMultiplier, Payout: e.Payout, Status: status,
			IsInfluencer: isInfluencer, IsDemo: e.IsDemo,
			RealBalanceAfter: real, DemoBalanceAfter: demo, BalanceAfter: balAfter,
		})
	}

	if err := g.rdb.EnqueuePersist(ctx, PersistJob{Round: round, Bets: bets}); err != nil {
		log.Printf("game: failed to enqueue persist job for round %d: %v", roundID, err)
	}
	if err := g.rdb.ClearRoundBets(ctx, roundID); err != nil {
		log.Printf("game: failed to clear round bets for round %d: %v", roundID, err)
	}
}

// RunPersistWorker drains persist:queue into Postgres. Run in its own
// goroutine from server.go. This is the eventually-consistent half of the
// fast path — round state is already durable in Redis/broadcast to clients
// by the time this runs.
func (g *GameEngine) RunPersistWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		job, err := g.rdb.DequeuePersist(ctx, 5*time.Second)
		if err != nil {
			log.Printf("persist worker: dequeue error: %v", err)
			time.Sleep(time.Second)
			continue
		}
		if job == nil {
			continue // timeout, nothing to do
		}
		if err := g.db.FlushRound(ctx, job.Round, job.Bets); err != nil {
			log.Printf("persist worker: failed to flush round %d: %v — re-queueing", job.Round.ID, err)
			// Re-enqueue rather than drop, so a transient Postgres blip
			// doesn't silently lose a round's worth of bets.
			_ = g.rdb.EnqueuePersist(ctx, *job)
			time.Sleep(2 * time.Second)
		}
	}
}

// ---- Provably fair crash point derivation ----

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// deriveCrashPoint implements the standard HMAC-based provably-fair crash
// formula (as used by Bustabit and similar games): take the first 52 bits
// of HMAC-SHA256(serverSeed, nonce), normalize to [0,1), then map through
// a house-edge-adjusted inverse distribution. Minimum result is 1.00x.
func deriveCrashPoint(serverSeed string, nonce int64, houseEdge float64) float64 {
	mac := hmac.New(sha256.New, []byte(serverSeed))
	mac.Write([]byte{
		byte(nonce >> 56), byte(nonce >> 48), byte(nonce >> 40), byte(nonce >> 32),
		byte(nonce >> 24), byte(nonce >> 16), byte(nonce >> 8), byte(nonce),
	})
	digest := mac.Sum(nil)

	// first 52 bits as a big int
	h := hex.EncodeToString(digest)[:13]
	n := new(big.Int)
	n.SetString(h, 16)
	maxVal := new(big.Int).Lsh(big.NewInt(1), 52)

	x := new(big.Float).Quo(new(big.Float).SetInt(n), new(big.Float).SetInt(maxVal))
	xf, _ := x.Float64()

	if xf <= 0 {
		return 1.00
	}

	crash := math.Floor((100*(1-houseEdge))/(1-xf)) / 100
	if crash < 1.00 {
		crash = 1.00
	}
	return crash
}

func round2(f float64) float64 {
	return math.Round(f*100) / 100
}

// publicRoundView strips the crash point out of round state before it goes
// to any non-admin channel — it's committed (hash-revealed) but must not be
// visible until the round actually crashes.
func publicRoundView(s RoundState) map[string]any {
	return map[string]any{
		"id": s.ID, "phase": s.Phase, "multiplier": round2(s.Multiplier),
		"serverSeedHash": s.ServerSeedHash, "countdown": s.Countdown,
	}
}

// ---- HTTP handlers: fast path ----

func (g *GameEngine) GetState(w http.ResponseWriter, r *http.Request) {
	state, err := g.rdb.GetCurrentRound(r.Context())
	if err != nil || state == nil {
		writeError(w, http.StatusServiceUnavailable, "no active round")
		return
	}
	writeSuccess(w, publicRoundView(*state))
}

func (g *GameEngine) GetHistory(w http.ResponseWriter, r *http.Request) {
	history, err := g.rdb.GetRoundHistory(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load history")
		return
	}
	writeSuccess(w, history)
}

// AdminRoundDebug exposes the pre-committed crash point — access already
// gated by RequireDebugAccess middleware (admin, or influencer with
// can_debug) before this handler ever runs.
func (g *GameEngine) AdminRoundDebug(w http.ResponseWriter, r *http.Request) {
	state, err := g.rdb.GetCurrentRound(r.Context())
	if err != nil || state == nil {
		writeError(w, http.StatusServiceUnavailable, "no active round")
		return
	}
	writeSuccess(w, map[string]any{
		"roundId": state.ID, "phase": state.Phase,
		"crashPoint": round2(state.CrashPoint), "countdown": state.Countdown,
	})
}

type placeBetRequest struct {
	Box      int     `json:"box"`
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"` // "demo" | "real"
}

func (g *GameEngine) PlaceBet(w http.ResponseWriter, r *http.Request) {
	userID, _ := UserIDFromContext(r.Context())

	var req placeBetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Box != 1 && req.Box != 2 {
		writeError(w, http.StatusBadRequest, "box must be 1 or 2")
		return
	}
	if req.Amount < minBetKES {
		writeError(w, http.StatusBadRequest, "minimum bet is KES 10")
		return
	}
	useReal := req.Currency == "real"

	state, err := g.rdb.GetCurrentRound(r.Context())
	if err != nil || state == nil || state.Phase != "waiting" {
		writeError(w, http.StatusConflict, "bets can only be placed while the round is in waiting phase")
		return
	}

	// Lazily warm the Redis balance cache from Postgres on first touch.
	profile, err := g.db.GetProfile(r.Context(), userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load profile")
		return
	}
	if err := g.rdb.EnsureBalance(r.Context(), userID, profile.DemoBalance, profile.RealBalance); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to sync balance")
		return
	}

	betID := uuid.New()
	res, err := g.rdb.PlaceBet(r.Context(), state.ID, userID, req.Box, req.Amount, useReal, betID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to place bet")
		return
	}
	if !res.OK {
		writeError(w, http.StatusBadRequest, res.Error)
		return
	}

	g.hub.Broadcast("bet:placed", map[string]any{
		"roundId": state.ID, "userId": userID, "box": req.Box, "amount": req.Amount,
	})
	writeSuccess(w, map[string]any{"betId": betID, "roundId": state.ID, "box": req.Box})
}

type cashoutRequest struct {
	Box int `json:"box"`
}

func (g *GameEngine) Cashout(w http.ResponseWriter, r *http.Request) {
	userID, _ := UserIDFromContext(r.Context())

	var req cashoutRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Box != 1 && req.Box != 2 {
		writeError(w, http.StatusBadRequest, "box must be 1 or 2")
		return
	}

	// This read is just a fast, friendly early rejection (instant error if
	// there's obviously no live round to cash out of) — it is NOT what
	// keeps cashouts safe after a crash. That guarantee now lives inside
	// cashoutLua itself (redis.go), which re-reads round:current
	// atomically alongside the payout, closing the race that existed when
	// this handler used to pass state.Multiplier through as a trusted
	// argument.
	state, err := g.rdb.GetCurrentRound(r.Context())
	if err != nil || state == nil || state.Phase != "running" {
		writeError(w, http.StatusConflict, "cashout is only valid while the round is running")
		return
	}

	res, err := g.rdb.Cashout(r.Context(), state.ID, userID, req.Box, maxCashoutKES)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to process cashout")
		return
	}
	if !res.OK {
		writeError(w, http.StatusBadRequest, res.Error)
		return
	}

	g.hub.Broadcast("bet:cashout", map[string]any{
		"roundId": state.ID, "userId": userID, "box": req.Box,
		"multiplier": round2(state.Multiplier), "payout": round2(res.Payout),
	})
	writeSuccess(w, map[string]any{"payout": round2(res.Payout), "multiplier": round2(state.Multiplier)})
}
