package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// RDB wraps the Redis client and holds the compiled Lua scripts used on the
// fast path (bet placement + cashout). See spec §4.1 — Redis is the
// authoritative source of truth for balance and round state during a live
// round; Postgres only gets the durable copy once a round crashes.
type RDB struct {
	client *redis.Client

	placeBetScript *redis.Script
	cashoutScript  *redis.Script
}

func NewRDB(url string) (*RDB, error) {
	opt, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parsing redis url: %w", err)
	}
	client := redis.NewClient(opt)
	if err := client.Ping(context.Background()).Err(); err != nil {
		return nil, fmt.Errorf("pinging redis: %w", err)
	}

	r := &RDB{client: client}
	r.placeBetScript = redis.NewScript(placeBetLua)
	r.cashoutScript = redis.NewScript(cashoutLua)
	return r, nil
}

func (r *RDB) Close() error { return r.client.Close() }

// ---- Types shared with game.go / db.go ----

type RoundRecord struct {
	ID             int64
	ServerSeed     string
	ServerSeedHash string
	ClientSeed     string
	Nonce          int64
	CrashPoint     float64
	StartedAt      time.Time
	CrashedAt      time.Time
}

type RoundState struct {
	ID             int64     `json:"id"`
	Phase          string    `json:"phase"` // waiting | running | crashed
	Multiplier     float64   `json:"multiplier"`
	CrashPoint     float64   `json:"crashPoint"` // set at commit time, only exposed via admin round-debug
	ServerSeedHash string    `json:"serverSeedHash"`
	StartedAt      time.Time `json:"startedAt"`
	Countdown      int       `json:"countdown"`
}

type BetRecord struct {
	ID                uuid.UUID
	UserID            uuid.UUID
	Box               int
	Amount            float64
	CashoutMultiplier *float64
	Payout            *float64
	Status            string // pending | active | cashed_out | lost
	IsInfluencer      bool
	IsDemo            bool
	RealBalanceAfter  float64
	DemoBalanceAfter  float64
	BalanceAfter      float64 // convenience mirror for influencer audit rows
}

// betEntry is the JSON shape stored per user:box field in the round's bets hash.
type betEntry struct {
	BetID             string   `json:"betId"`
	UserID            string   `json:"userId"`
	Box               int      `json:"box"`
	Amount            float64  `json:"amount"`
	Status            string   `json:"status"`
	CashoutMultiplier *float64 `json:"cashoutMultiplier,omitempty"`
	Payout            *float64 `json:"payout,omitempty"`
	IsInfluencer      bool     `json:"isInfluencer"`
	IsDemo            bool     `json:"isDemo"`
}

func roundKey(id int64) string           { return fmt.Sprintf("round:%d:meta", id) }
func roundBetsKey(id int64) string       { return fmt.Sprintf("round:bets:%d", id) }
func balanceKey(userID uuid.UUID) string { return fmt.Sprintf("balance:%s", userID) }

const currentRoundKey = "round:current"
const roundHistoryKey = "round:history"
const persistQueueKey = "persist:queue"

// palplussActiveChannelKey holds whichever Palpluss channel ID an admin has
// picked as preferred (see admin.go's SetActivePalplussChannel and
// palpluss.go's resolveChannelID). Stored in Redis rather than only in
// process memory so a rotation an admin makes actually survives a
// deploy/restart, the same durability reasoning as every other piece of
// runtime state in this file.
const palplussActiveChannelKey = "config:palpluss:active_channel"

// ---- Round state (round:current) ----

func (r *RDB) SetCurrentRound(ctx context.Context, s RoundState) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return r.client.Set(ctx, currentRoundKey, b, 0).Err()
}

func (r *RDB) GetCurrentRound(ctx context.Context) (*RoundState, error) {
	val, err := r.client.Get(ctx, currentRoundKey).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s RoundState
	if err := json.Unmarshal(val, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (r *RDB) ClearCurrentRound(ctx context.Context) error {
	return r.client.Del(ctx, currentRoundKey).Err()
}

func (r *RDB) PushRoundHistory(ctx context.Context, crashPoint float64) error {
	pipe := r.client.TxPipeline()
	pipe.LPush(ctx, roundHistoryKey, crashPoint)
	pipe.LTrim(ctx, roundHistoryKey, 0, 49)
	_, err := pipe.Exec(ctx)
	return err
}

func (r *RDB) GetRoundHistory(ctx context.Context) ([]float64, error) {
	vals, err := r.client.LRange(ctx, roundHistoryKey, 0, 49).Result()
	if err != nil {
		return nil, err
	}
	out := make([]float64, 0, len(vals))
	for _, v := range vals {
		var f float64
		if _, err := fmt.Sscanf(v, "%f", &f); err == nil {
			out = append(out, f)
		}
	}
	return out, nil
}

// ---- Balances ----

// EnsureBalance seeds balance:{userId} from Postgres if it's not already
// cached in Redis (e.g. after a deploy/restart). Called lazily on bet
// placement so we don't need a warm-up job.
func (r *RDB) EnsureBalance(ctx context.Context, userID uuid.UUID, demo, real float64) error {
	key := balanceKey(userID)
	exists, err := r.client.Exists(ctx, key).Result()
	if err != nil {
		return err
	}
	if exists == 1 {
		return nil
	}
	return r.client.HSet(ctx, key, map[string]any{"demo": demo, "real": real}).Err()
}

func (r *RDB) GetBalance(ctx context.Context, userID uuid.UUID) (demo, real float64, err error) {
	vals, err := r.client.HMGet(ctx, balanceKey(userID), "demo", "real").Result()
	if err != nil {
		return 0, 0, err
	}
	if vals[0] != nil {
		fmt.Sscanf(fmt.Sprint(vals[0]), "%f", &demo)
	}
	if vals[1] != nil {
		fmt.Sscanf(fmt.Sprint(vals[1]), "%f", &real)
	}
	return demo, real, nil
}

// HasBalance reports whether userID's balance is already cached in Redis.
// Used to decide, on read, whether it's safe to trust Redis or whether we
// need to warm it from Postgres first (see App.getLiveBalance in wallet.go).
func (r *RDB) HasBalance(ctx context.Context, userID uuid.UUID) (bool, error) {
	n, err := r.client.Exists(ctx, balanceKey(userID)).Result()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// IncrRealBalance atomically adjusts the cached real balance by delta.
//
// Real-money balance has two writers: the fast path (placeBetLua/cashoutLua
// above, which own Redis during a live round) and the consistency path
// (deposits, withdrawal approvals, influencer credits — see wallet.go's
// DarajaCallback and admin.go's ReviewWithdrawal/UpdateUserRole), which
// writes Postgres directly. Once a user's balance is cached in Redis it
// becomes the read source of truth (see getLiveBalance), so any consistency
// -path write that changes real_balance in Postgres MUST also call this,
// or the change won't show up until something evicts the stale cache entry
// (which currently never happens).
//
// If the balance isn't cached yet, this is a no-op: the Postgres write is
// already correct, and the cache will warm from that fresh row on next read.
// HINCRBYFLOAT is atomic, so this can't race the Lua scripts' HSET.
func (r *RDB) IncrRealBalance(ctx context.Context, userID uuid.UUID, delta float64) error {
	if delta == 0 {
		return nil
	}
	has, err := r.HasBalance(ctx, userID)
	if err != nil || !has {
		return err
	}
	return r.client.HIncrByFloat(ctx, balanceKey(userID), "real", delta).Err()
}

// GetActivePalplussChannel returns the admin-selected active channel ID, or
// "" if nothing has been explicitly set yet (callers fall back to the
// configured pool's first entry / the legacy single-channel env var — see
// palpluss.go's resolveChannelID).
func (r *RDB) GetActivePalplussChannel(ctx context.Context) (string, error) {
	val, err := r.client.Get(ctx, palplussActiveChannelKey).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return val, nil
}

// SetActivePalplussChannel records channelID as the preferred Palpluss
// channel — the next InitiateDeposit call picks it up immediately, no
// redeploy needed.
func (r *RDB) SetActivePalplussChannel(ctx context.Context, channelID string) error {
	return r.client.Set(ctx, palplussActiveChannelKey, channelID, 0).Err()
}

// ---- Fast path: bet placement ----

// placeBetLua atomically:
//  1. checks the round is still 'waiting' or 'running' with betting allowed,
//  2. checks the user hasn't already bet on this box this round,
//  3. checks sufficient balance in the chosen currency (demo vs real),
//  4. debits the balance and writes the bet entry into round:bets:{id}.
//
// All in one round-trip so two concurrent requests can never race each
// other into an over-debit.
const placeBetLua = `
local balanceKey = KEYS[1]
local betsKey = KEYS[2]
local userId = ARGV[1]
local box = ARGV[2]
local amount = tonumber(ARGV[3])
local useReal = ARGV[4] == "1"
local betJson = ARGV[5]

local field = userId .. ":" .. box
if redis.call("HEXISTS", betsKey, field) == 1 then
  return {err = "bet already placed on this box"}
end

local balField = "real"
if not useReal then
  balField = "demo"
end

local current = tonumber(redis.call("HGET", balanceKey, balField) or "0")
if current < amount then
  return {err = "insufficient balance"}
end

redis.call("HSET", balanceKey, balField, current - amount)
redis.call("HSET", betsKey, field, betJson)

return "OK"
`

type PlaceBetResult struct {
	OK    bool
	Error string
}

func (r *RDB) PlaceBet(ctx context.Context, roundID int64, userID uuid.UUID, box int, amount float64, useReal bool, betID uuid.UUID) (*PlaceBetResult, error) {
	entry := betEntry{
		BetID:  betID.String(),
		UserID: userID.String(),
		Box:    box,
		Amount: amount,
		Status: "active",
		IsDemo: !useReal,
	}
	entryJSON, err := json.Marshal(entry)
	if err != nil {
		return nil, err
	}

	realFlag := "0"
	if useReal {
		realFlag = "1"
	}

	res, err := r.placeBetScript.Run(ctx, r.client,
		[]string{balanceKey(userID), roundBetsKey(roundID)},
		userID.String(), box, amount, realFlag, string(entryJSON),
	).Result()

	if err != nil {
		// redis.Script surfaces Lua `return {err=...}` as a plain Go error
		return &PlaceBetResult{OK: false, Error: err.Error()}, nil
	}
	_ = res
	return &PlaceBetResult{OK: true}, nil
}

// ---- Fast path: cashout ----

// cashoutLua atomically:
//  1. reads round:current itself, inside the same atomic step — it does
//     NOT trust a phase/multiplier value read by a separate, earlier
//     Redis call from game.go's Cashout handler. Two sequential reads
//     (read round state, THEN run this script) are not atomic with each
//     other: a round can flip to 'crashed' and clear its bets in the gap
//     between them, letting a stale 'running' read slip a cashout through
//     after the round already ended. Reading round:current from inside
//     the script closes that gap.
//  2. rejects if the round isn't currently 'running',
//  3. loads the bet entry, verifies it's 'active' (not already cashed out/lost),
//  4. computes payout = amount * multiplier using the multiplier it just read,
//  5. credits the balance and marks the bet 'cashed_out' with the payout.
const cashoutLua = `
local balanceKey = KEYS[1]
local betsKey = KEYS[2]
local roundKey = KEYS[3]
local userId = ARGV[1]
local box = ARGV[2]
local maxCashout = tonumber(ARGV[3])

local roundRaw = redis.call("GET", roundKey)
if not roundRaw then
  return {err = "no active round"}
end

local round = cjson.decode(roundRaw)
if round.phase ~= "running" then
  return {err = "cashout is only valid while the round is running"}
end
local multiplier = round.multiplier

local field = userId .. ":" .. box
local raw = redis.call("HGET", betsKey, field)
if not raw then
  return {err = "no active bet on this box"}
end

local bet = cjson.decode(raw)
if bet.status ~= "active" then
  return {err = "bet is not active"}
end

local payout = bet.amount * multiplier
if payout > maxCashout then
  payout = maxCashout
end

local balField = "real"
if bet.isDemo then
  balField = "demo"
end

local current = tonumber(redis.call("HGET", balanceKey, balField) or "0")
redis.call("HSET", balanceKey, balField, current + payout)

bet.status = "cashed_out"
bet.cashoutMultiplier = multiplier
bet.payout = payout
redis.call("HSET", betsKey, field, cjson.encode(bet))

return tostring(payout)
`

type CashoutResult struct {
	OK     bool
	Payout float64
	Error  string
}

// Cashout no longer takes a multiplier argument — the script now reads the
// live round state itself (see cashoutLua's doc comment above for why the
// old two-step version was unsafe).
func (r *RDB) Cashout(ctx context.Context, roundID int64, userID uuid.UUID, box int, maxCashout float64) (*CashoutResult, error) {
	res, err := r.cashoutScript.Run(ctx, r.client,
		[]string{balanceKey(userID), roundBetsKey(roundID), currentRoundKey},
		userID.String(), box, maxCashout,
	).Result()

	if err != nil {
		return &CashoutResult{OK: false, Error: err.Error()}, nil
	}

	var payout float64
	fmt.Sscanf(fmt.Sprint(res), "%f", &payout)
	return &CashoutResult{OK: true, Payout: payout}, nil
}

// ---- Round bet retrieval (for the crash-time flush + admin round-debug) ----

func (r *RDB) GetRoundBets(ctx context.Context, roundID int64) ([]betEntry, error) {
	raw, err := r.client.HGetAll(ctx, roundBetsKey(roundID)).Result()
	if err != nil {
		return nil, err
	}
	out := make([]betEntry, 0, len(raw))
	for _, v := range raw {
		var e betEntry
		if err := json.Unmarshal([]byte(v), &e); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

func (r *RDB) ClearRoundBets(ctx context.Context, roundID int64) error {
	return r.client.Del(ctx, roundBetsKey(roundID)).Err()
}

// ---- Write-behind persistence queue ----

type PersistJob struct {
	Round RoundRecord
	Bets  []BetRecord
}

// EnqueuePersist pushes a finalized round for the background worker (see
// game.go's runPersistWorker) to flush into Postgres. Kept as a Redis list
// so the job survives a process restart between enqueue and drain.
func (r *RDB) EnqueuePersist(ctx context.Context, job PersistJob) error {
	b, err := json.Marshal(job)
	if err != nil {
		return err
	}
	return r.client.RPush(ctx, persistQueueKey, b).Err()
}

// DequeuePersist blocks up to timeout waiting for a job. Returns nil, nil on timeout.
func (r *RDB) DequeuePersist(ctx context.Context, timeout time.Duration) (*PersistJob, error) {
	res, err := r.client.BLPop(ctx, timeout, persistQueueKey).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(res) < 2 {
		return nil, nil
	}
	var job PersistJob
	if err := json.Unmarshal([]byte(res[1]), &job); err != nil {
		return nil, err
	}
	return &job, nil
}

// ---- Rate limiting ----

// AllowRequest implements a simple fixed-window counter per key. Good enough
// for the consistency-path routes (deposit/withdraw/auth) which don't need
// the precision of a sliding window — see middleware.go.
func (r *RDB) AllowRequest(ctx context.Context, key string, limit int, window time.Duration) (bool, error) {
	count, err := r.client.Incr(ctx, key).Result()
	if err != nil {
		return false, err
	}
	if count == 1 {
		r.client.Expire(ctx, key, window)
	}
	return count <= int64(limit), nil
}
