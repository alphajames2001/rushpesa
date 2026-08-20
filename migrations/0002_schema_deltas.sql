-- profiles extends auth.users
create table profiles (
  id uuid primary key references auth.users(id) on delete cascade,
  username text unique not null,
  role text not null default 'user' check (role in ('user','influencer','admin')),
  can_debug boolean not null default false,
  demo_balance numeric(14,2) not null default 50000,
  real_balance numeric(14,2) not null default 0,
  influencer_credited boolean not null default false, -- guards against re-firing the 2,000 auto-credit on demote→re-promote
  created_at timestamptz not null default now()
);

create table transactions (
  id uuid primary key default gen_random_uuid(),
  user_id uuid not null references profiles(id),
  type text not null check (type in ('deposit','withdrawal')),
  amount numeric(14,2) not null,
  status text not null default 'pending' check (status in ('pending','approved','rejected','completed')),
  mpesa_receipt text,
  reviewed_by uuid references profiles(id),
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now()
);

create table influencer_transactions (
  id uuid primary key default gen_random_uuid(),
  user_id uuid not null references profiles(id),
  type text not null check (type in ('bet','win','admin_credit','withdrawal')),
  amount numeric(14,2) not null,
  balance_after numeric(14,2) not null,
  created_at timestamptz not null default now()
);

create table influencer_mock_mpesa (
  user_id uuid primary key references profiles(id),
  withdrawn_amount numeric(14,2) not null default 0,
  updated_at timestamptz not null default now()
);

create table rounds (
  id bigserial primary key,
  server_seed text not null,
  server_seed_hash text not null,   -- revealed pre-round for provably-fair
  client_seed text,
  nonce bigint not null,
  crash_point numeric(10,2) not null,
  started_at timestamptz,
  crashed_at timestamptz,
  created_at timestamptz not null default now()
);

create table bets (
  id uuid primary key default gen_random_uuid(),
  round_id bigint not null references rounds(id),
  user_id uuid not null references profiles(id),
  box smallint not null check (box in (1,2)),
  amount numeric(14,2) not null,
  cashout_multiplier numeric(10,2),
  payout numeric(14,2),
  status text not null default 'pending' check (status in ('pending','active','cashed_out','lost')),
  created_at timestamptz not null default now()
);

create index idx_bets_round on bets(round_id);
create index idx_bets_user on bets(user_id);
create index idx_transactions_user on transactions(user_id);
create index idx_transactions_status on transactions(status);


-- Run this against the schema in dotpesa-backend-spec.md §6.
-- Covers columns/tables that came up while writing the actual Go code
-- and weren't in the original design doc.

-- 1. Idempotency guard for the influencer auto-credit (spec §3.2 / admin.go SetRole)
alter table profiles
  add column if not exists influencer_credited boolean not null default false;

-- 2. Matches Daraja's STK callback (which only carries CheckoutRequestID)
--    back to our pending deposit row (wallet.go / daraja.go).
alter table transactions
  add column if not exists daraja_checkout_id text;

create index if not exists idx_transactions_checkout_id
  on transactions(daraja_checkout_id);

-- 3. Per-withdrawal audit log for the influencer mock-M-Pesa flow, feeding
--    dashboard.html's /admin/influencer-withdrawals + mark-{status} calls.
--    Status here is bookkeeping only — see spec §10 gap note, influencer
--    withdrawals need no approval and are already final when created.
create table if not exists influencer_withdrawals (
  id uuid primary key default gen_random_uuid(),
  user_id uuid not null references profiles(id),
  amount numeric(14,2) not null,
  status text not null default 'pending' check (status in ('pending','sent','failed')),
  created_at timestamptz not null default now()
);

create index if not exists idx_influencer_withdrawals_user on influencer_withdrawals(user_id);
