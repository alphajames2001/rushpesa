# dotPesa Backend (Go)

Implements the design in `dotpesa-backend-spec.md`. Read that first.

## What's built vs. not

**Built end to end:** signup/login (Supabase Auth), profile management,
round engine + provably-fair crash generation, bet placement + cashout (Redis
fast path, Lua-atomic), WebSocket round broadcast, deposit flow via Daraja
STK Push (full, including callback handling), admin dashboard endpoints
(stats, user/role management, transaction ledger, withdrawal review),
influencer flow (auto-credit on promotion, mock M-Pesa withdraw), round-debug
diagnostic endpoint, `/health` keep-alive endpoint.

**Deliberately NOT built:** Daraja B2C (real withdrawal payouts). Withdrawal
requests are created and can be admin-approved, but "approved" only reserves
the funds (debits the balance) — it does not send any money. Wire up the
actual B2C call in `wallet.go`/`daraja.go` when ready; nothing else in the
codebase assumes it exists.

## Setup

1. `cp .env.example .env` and fill in Supabase + Redis + Daraja credentials.
2. Run the schema from `dotpesa-backend-spec.md` §6 against your Supabase
   Postgres instance, then run `migrations/0002_schema_deltas.sql` on top of
   it (covers a few columns/tables that came up while writing the actual
   handlers — `influencer_credited`, `daraja_checkout_id`, `influencer_withdrawals`).
3. `go mod tidy` (needs normal internet access — this was written and
   syntax-checked in a sandboxed environment without full module-proxy
   access, so dependency resolution wasn't verified end-to-end here; it
   should resolve cleanly on a normal machine or on Render's build step).
4. `go run .`

## Known integration gap: round-debug.html's WebSocket client

The Go backend broadcasts round state over **plain WebSocket** at `/ws`
(JSON envelopes `{"event": "...", "data": {...}}`), not Socket.IO. The
`round-debug.html` template currently loads `socket.io-client@4`, which
speaks a different protocol (Engine.IO) and won't connect as-is. Swap:

```js
// out:
const socket = io(API_URL, { transports: ['websocket'] });

// in:
const socket = new WebSocket(API_URL.replace(/^http/, 'ws') + '/ws');
socket.onmessage = (msg) => {
  const { event, data } = JSON.parse(msg.data);
  // dispatch on `event` the same way the old socket.on(event, ...) did
};
```

Same applies anywhere else in the frontend that expects Socket.IO events
(`round:waiting`, `round:started`, `round:tick`, `round:crashed`,
`bet:placed`, `bet:cashout`) — event names are unchanged, just the transport.

## Also worth knowing

- `round-debug.html`'s auth guard currently only checks `role === 'admin'`.
  The backend already supports influencer + `can_debug` via
  `RequireDebugAccess` middleware — the template's client-side guard just
  needs the same OR condition added.
- CORS in `server.go` is wide open (`AllowedOrigins: []string{"*"}`) —
  tighten to your real frontend origin before going anywhere near production.
