-- dotPesa — full database schema
-- Consolidates 0001 base schema + 0002_schema_deltas.sql, reconciled against
-- what the current Go code (db.go / wallet.go / admin.go) actually reads and
-- writes. Run once against a fresh Supabase/Postgres database.
--
-- Fixes applied vs. the old delta script:
--   * profiles.phone added        — CreateProfile/GetProfile/ListUsers/UpdateProfile all use it; was missing entirely.
--   * transactions.phone added    — CreateDeposit/CreateWithdrawal insert it; was missing entirely.
--   * transactions.payment_provider added — CreateDeposit inserts it; was missing entirely.
--   * transactions.daraja_checkout_id -> provider_checkout_id — db.go's SetDepositCheckoutID/
--     GetDepositByCheckoutID use provider_checkout_id (works for both Daraja and Palpluss);
--     the old delta still had the Daraja-only name.

begin;

create extension if not exists pgcrypto;

-- profiles extends auth.users (Supabase Auth)
create table if not exists profiles (
  id uuid primary key references auth.users(id) on delete cascade,
  username text unique not null,
  phone text,
  role text not null default 'user' check (role in ('user','influencer','admin')),
  can_debug boolean not null default false,
  demo_balance numeric(14,2) not null default 50000,
  real_balance numeric(14,2) not null default 0,
  influencer_credited boolean not null default false, -- guards against re-firing the 2,000 auto-credit on demote→re-promote
  created_at timestamptz not null default now()
);

create table if not exists transactions (
  id uuid primary key default gen_random_uuid(),
  user_id uuid not null references profiles(id),
  type text not null check (type in ('deposit','withdrawal')),
  amount numeric(14,2) not null,
  status text not null default 'pending' check (status in ('pending','approved','rejected','completed')),
  phone text,                       -- deposit source / withdrawal destination M-Pesa number
  payment_provider text,            -- 'daraja' | 'palpluss' — set on deposits only
  provider_checkout_id text,        -- Daraja CheckoutRequestID or Palpluss transactionId
  mpesa_receipt text,
  reviewed_by uuid references profiles(id),
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now()
);

create table if not exists influencer_transactions (
  id uuid primary key default gen_random_uuid(),
  user_id uuid not null references profiles(id),
  type text not null check (type in ('bet','win','admin_credit','withdrawal')),
  amount numeric(14,2) not null,
  balance_after numeric(14,2) not null,
  created_at timestamptz not null default now()
);

create table if not exists influencer_mock_mpesa (
  user_id uuid primary key references profiles(id),
  withdrawn_amount numeric(14,2) not null default 0,
  updated_at timestamptz not null default now()
);

create table if not exists rounds (
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

create table if not exists bets (
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

create table if not exists influencer_withdrawals (
  id uuid primary key default gen_random_uuid(),
  user_id uuid not null references profiles(id),
  amount numeric(14,2) not null,
  status text not null default 'pending' check (status in ('pending','sent','failed')),
  created_at timestamptz not null default now()
);

-- indexes
create index if not exists idx_bets_round on bets(round_id);
create index if not exists idx_bets_user on bets(user_id);
create index if not exists idx_transactions_user on transactions(user_id);
create index if not exists idx_transactions_status on transactions(status);
create index if not exists idx_transactions_checkout_id on transactions(provider_checkout_id);
create index if not exists idx_influencer_withdrawals_user on influencer_withdrawals(user_id);

commit;
