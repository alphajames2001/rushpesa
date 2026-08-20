package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DB wraps the Postgres connection pool.
type DB struct {
	pool *pgxpool.Pool
}

func NewDB(ctx context.Context, connString string) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(connString)
	if err != nil {
		return nil, fmt.Errorf("parsing db config: %w", err)
	}
	cfg.MaxConns = 10
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connecting to postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("pinging postgres: %w", err)
	}
	return &DB{pool: pool}, nil
}

func (d *DB) Close() {
	d.pool.Close()
}

// ---- Profiles ----

type Profile struct {
	ID                 uuid.UUID `json:"id"`
	Username           string    `json:"username"`
	DisplayName        string    `json:"displayName"`
	Email              string    `json:"email"`
	Phone              string    `json:"phone"`
	Role               string    `json:"role"`
	CanDebug           bool      `json:"canDebug"`
	DemoBalance        float64   `json:"demoBalance"`
	RealBalance        float64   `json:"realBalance"`
	InfluencerCredited bool      `json:"influencerCredited"`
	CreatedAt          time.Time `json:"createdAt"`
}

var ErrNotFound = errors.New("not found")

// CreateProfile is called right after Supabase Auth signup succeeds.
func (d *DB) CreateProfile(ctx context.Context, id uuid.UUID, username, phone string) (*Profile, error) {
	const q = `
		insert into profiles (id, username, phone, role, demo_balance, real_balance)
		values ($1, $2, $3, 'user', 50000, 0)
		returning id, username, phone, role, can_debug, demo_balance, real_balance, influencer_credited, created_at`
	return d.scanProfile(d.pool.QueryRow(ctx, q, id, username, phone))
}

func (d *DB) GetProfile(ctx context.Context, id uuid.UUID) (*Profile, error) {
	const q = `
		select id, username, phone, role, can_debug, demo_balance, real_balance, influencer_credited, created_at
		from profiles where id = $1`
	return d.scanProfile(d.pool.QueryRow(ctx, q, id))
}

func (d *DB) scanProfile(row pgx.Row) (*Profile, error) {
	var p Profile
	var phone *string
	err := row.Scan(&p.ID, &p.Username, &phone, &p.Role, &p.CanDebug, &p.DemoBalance, &p.RealBalance, &p.InfluencerCredited, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if phone != nil {
		p.Phone = *phone
	}
	p.DisplayName = p.Username
	p.Email = ""
	return &p, nil
}

// UpdateProfile updates whichever of username/phone are non-nil. Each is
// applied independently so a request that only wants to change one of them
// (see auth.go's UpdateOwnProfile) doesn't need to resend the other.
func (d *DB) UpdateProfile(ctx context.Context, id uuid.UUID, username, phone *string) error {
	if username != nil {
		if _, err := d.pool.Exec(ctx, `update profiles set username = $2 where id = $1`, id, *username); err != nil {
			return err
		}
	}
	if phone != nil {
		if _, err := d.pool.Exec(ctx, `update profiles set phone = $2 where id = $1`, id, *phone); err != nil {
			return err
		}
	}
	return nil
}

// SetRole changes a user's role. Returns the profile and the amount (if any)
// auto-credited to real_balance as part of a fresh influencer promotion —
// callers use the latter to sync the Redis balance cache (see admin.go).
func (d *DB) SetRole(ctx context.Context, adminID, userID uuid.UUID, newRole string) (*Profile, float64, error) {
	if newRole != "user" && newRole != "influencer" && newRole != "admin" {
		return nil, 0, fmt.Errorf("invalid role %q", newRole)
	}

	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback(ctx)

	var p Profile
	var phone *string
	err = tx.QueryRow(ctx, `
		select id, username, phone, role, can_debug, demo_balance, real_balance, influencer_credited, created_at
		from profiles where id = $1 for update`, userID,
	).Scan(&p.ID, &p.Username, &phone, &p.Role, &p.CanDebug, &p.DemoBalance, &p.RealBalance, &p.InfluencerCredited, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, ErrNotFound
	}
	if phone != nil {
		p.Phone = *phone
	}
	if err != nil {
		return nil, 0, err
	}

	shouldCredit := newRole == "influencer" && !p.InfluencerCredited
	const influencerAutoCredit = 2000.00
	credited := 0.0

	if shouldCredit {
		newBalance := p.RealBalance + influencerAutoCredit
		_, err = tx.Exec(ctx, `
			update profiles set role = $2, real_balance = $3, influencer_credited = true
			where id = $1`, userID, newRole, newBalance)
		if err != nil {
			return nil, 0, err
		}
		_, err = tx.Exec(ctx, `
			insert into influencer_transactions (id, user_id, type, amount, balance_after)
			values ($1, $2, 'admin_credit', $3, $4)`,
			uuid.New(), userID, influencerAutoCredit, newBalance)
		if err != nil {
			return nil, 0, err
		}
		p.RealBalance = newBalance
		p.InfluencerCredited = true
		credited = influencerAutoCredit
	} else {
		_, err = tx.Exec(ctx, `update profiles set role = $2 where id = $1`, userID, newRole)
		if err != nil {
			return nil, 0, err
		}
	}
	p.Role = newRole
	p.DisplayName = p.Username

	if err := tx.Commit(ctx); err != nil {
		return nil, 0, err
	}
	return &p, credited, nil
}

func (d *DB) SetDebugAccess(ctx context.Context, userID uuid.UUID, enabled bool) error {
	const q = `update profiles set can_debug = $2 where id = $1 and role = 'influencer'`
	tag, err := d.pool.Exec(ctx, q, userID, enabled)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errors.New("user not found or not an influencer")
	}
	return nil
}

func (d *DB) ListUsers(ctx context.Context, limit int) ([]Profile, error) {
	const q = `
		select id, username, phone, role, can_debug, demo_balance, real_balance, influencer_credited, created_at
		from profiles order by created_at desc limit $1`
	rows, err := d.pool.Query(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Profile
	for rows.Next() {
		var p Profile
		var phone *string
		if err := rows.Scan(&p.ID, &p.Username, &phone, &p.Role, &p.CanDebug, &p.DemoBalance, &p.RealBalance, &p.InfluencerCredited, &p.CreatedAt); err != nil {
			return nil, err
		}
		if phone != nil {
			p.Phone = *phone
		}
		p.DisplayName = p.Username
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---- Transactions ----

type Transaction struct {
	ID           uuid.UUID  `json:"id"`
	UserID       uuid.UUID  `json:"userId"`
	Type         string     `json:"type"`
	Amount       float64    `json:"amount"`
	AmountKES    float64    `json:"amount_kes"`
	Status       string     `json:"status"`
	MpesaReceipt *string    `json:"mpesaReceipt,omitempty"`
	ReviewedBy   *uuid.UUID `json:"reviewedBy,omitempty"`
	CreatedAt    time.Time  `json:"createdAt"`
	UpdatedAt    time.Time  `json:"updatedAt"`
}

// DashboardTransaction extends Transaction with user details for the admin UI.
type DashboardTransaction struct {
	ID          uuid.UUID `json:"id"`
	UserID      uuid.UUID `json:"userId"`
	Type        string    `json:"type"`
	Amount      float64   `json:"amount"`
	AmountKES   float64   `json:"amount_kes"`
	Status      string    `json:"status"`
	DisplayName string    `json:"display_name"`
	Email       string    `json:"email"`
	Phone       string    `json:"phone"`
	Method      string    `json:"method"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type DashboardWithdrawal struct {
	ID          uuid.UUID `json:"id"`
	UserID      uuid.UUID `json:"userId"`
	Amount      float64   `json:"amount"`
	AmountKES   float64   `json:"amount_kes"`
	Status      string    `json:"status"`
	DisplayName string    `json:"display_name"`
	Email       string    `json:"email"`
	Phone       string    `json:"phone"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// CreateDeposit records provider alongside the pending row (payment_provider
// — see the migration in schema-updates.sql) so a webhook from one provider
// can't be misread against a deposit that was actually initiated through
// the other, and so the admin dashboard can show which one handled it.
func (d *DB) CreateDeposit(ctx context.Context, userID uuid.UUID, amount float64, phone, provider string) (uuid.UUID, error) {
	id := uuid.New()
	const q = `
		insert into transactions (id, user_id, type, amount, status, phone, payment_provider)
		values ($1, $2, 'deposit', $3, 'pending', $4, $5)`
	_, err := d.pool.Exec(ctx, q, id, userID, amount, phone, provider)
	return id, err
}

// SetDepositCheckoutID / GetDepositByCheckoutID use provider_checkout_id,
// renamed from daraja_checkout_id now that either provider's tracking ref
// can land here (see schema-updates.sql) — Daraja's CheckoutRequestID or
// Palpluss's transactionId (see daraja.go / palpluss.go InitiateDeposit).
func (d *DB) SetDepositCheckoutID(ctx context.Context, txID uuid.UUID, providerRef string) error {
	const q = `update transactions set provider_checkout_id = $2 where id = $1`
	_, err := d.pool.Exec(ctx, q, txID, providerRef)
	return err
}

func (d *DB) GetDepositByCheckoutID(ctx context.Context, providerRef string) (uuid.UUID, error) {
	var id uuid.UUID
	const q = `select id from transactions where provider_checkout_id = $1 and type = 'deposit'`
	err := d.pool.QueryRow(ctx, q, providerRef).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNotFound
	}
	return id, err
}

// CompleteDeposit returns (userID, amount, credited, err). credited is false
// when the deposit was already completed (e.g. a duplicate Daraja callback
// retry) — callers must not re-apply a Redis balance credit in that case,
// since Postgres wasn't touched either.
func (d *DB) CompleteDeposit(ctx context.Context, txID uuid.UUID, mpesaReceipt string) (userID uuid.UUID, amount float64, credited bool, err error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, 0, false, err
	}
	defer tx.Rollback(ctx)

	var status string
	err = tx.QueryRow(ctx, `
		select user_id, amount, status from transactions where id = $1 and type = 'deposit' for update`, txID,
	).Scan(&userID, &amount, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, 0, false, ErrNotFound
	}
	if err != nil {
		return uuid.Nil, 0, false, err
	}
	if status == "completed" {
		return userID, amount, false, nil
	}

	_, err = tx.Exec(ctx, `
		update transactions set status = 'completed', mpesa_receipt = $2, updated_at = now()
		where id = $1`, txID, mpesaReceipt)
	if err != nil {
		return uuid.Nil, 0, false, err
	}

	_, err = tx.Exec(ctx, `update profiles set real_balance = real_balance + $2 where id = $1`, userID, amount)
	if err != nil {
		return uuid.Nil, 0, false, err
	}

	return userID, amount, true, tx.Commit(ctx)
}

func (d *DB) FailDeposit(ctx context.Context, txID uuid.UUID, reason string) error {
	const q = `update transactions set status = 'rejected', updated_at = now() where id = $1 and type = 'deposit'`
	_, err := d.pool.Exec(ctx, q, txID)
	return err
}

func (d *DB) CreateWithdrawal(ctx context.Context, userID uuid.UUID, amount float64, phone string) (uuid.UUID, error) {
	id := uuid.New()
	const q = `
		insert into transactions (id, user_id, type, amount, status, phone)
		values ($1, $2, 'withdrawal', $3, 'pending', $4)`
	_, err := d.pool.Exec(ctx, q, id, userID, amount, phone)
	return id, err
}

func (d *DB) GetProfileBalance(ctx context.Context, userID uuid.UUID) (real float64, err error) {
	const q = `select real_balance from profiles where id = $1`
	err = d.pool.QueryRow(ctx, q, userID).Scan(&real)
	return
}

// ReviewWithdrawal returns (userID, debitedAmount, err). debitedAmount is
// only nonzero when approve is true and the debit actually happened —
// callers use it to sync the Redis balance cache (see admin.go).
func (d *DB) ReviewWithdrawal(ctx context.Context, adminID, txID uuid.UUID, approve bool) (uuid.UUID, float64, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, 0, err
	}
	defer tx.Rollback(ctx)

	var userID uuid.UUID
	var amount float64
	var status string
	err = tx.QueryRow(ctx, `
		select user_id, amount, status from transactions
		where id = $1 and type = 'withdrawal' for update`, txID,
	).Scan(&userID, &amount, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, 0, ErrNotFound
	}
	if err != nil {
		return uuid.Nil, 0, err
	}
	if status != "pending" {
		return uuid.Nil, 0, fmt.Errorf("withdrawal already %s", status)
	}

	newStatus := "rejected"
	debited := 0.0
	if approve {
		newStatus = "approved"
		var balance float64
		if err := tx.QueryRow(ctx, `select real_balance from profiles where id = $1 for update`, userID).Scan(&balance); err != nil {
			return uuid.Nil, 0, err
		}
		if balance < amount {
			return uuid.Nil, 0, errors.New("insufficient balance to approve withdrawal")
		}
		if _, err := tx.Exec(ctx, `update profiles set real_balance = real_balance - $2 where id = $1`, userID, amount); err != nil {
			return uuid.Nil, 0, err
		}
		debited = amount
	}

	_, err = tx.Exec(ctx, `
		update transactions set status = $2, reviewed_by = $3, updated_at = now() where id = $1`,
		txID, newStatus, adminID)
	if err != nil {
		return uuid.Nil, 0, err
	}

	return userID, debited, tx.Commit(ctx)
}

// ListPendingWithdrawals returns dashboard-formatted withdrawals.
func (d *DB) ListPendingWithdrawals(ctx context.Context) ([]DashboardWithdrawal, error) {
	rows, err := d.pool.Query(ctx, `
		select t.id, t.user_id, t.amount, t.status, t.created_at, t.updated_at,
		       p.username
		from transactions t
		join profiles p on p.id = t.user_id
		where t.type = 'withdrawal' and t.status = 'pending'
		order by t.created_at desc`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DashboardWithdrawal
	for rows.Next() {
		var w DashboardWithdrawal
		var username string
		if err := rows.Scan(&w.ID, &w.UserID, &w.Amount, &w.Status, &w.CreatedAt, &w.UpdatedAt, &username); err != nil {
			return nil, err
		}
		w.AmountKES = w.Amount
		w.DisplayName = username
		w.Email = ""
		w.Phone = ""
		out = append(out, w)
	}
	if out == nil {
		out = []DashboardWithdrawal{}
	}
	return out, rows.Err()
}

// ListTransactions returns dashboard-formatted transactions with pagination.
func (d *DB) ListTransactions(ctx context.Context, txType string, limit, offset int) ([]DashboardTransaction, int, error) {
	q := `select t.id, t.user_id, t.type, t.amount, t.status, t.created_at, t.updated_at, p.username
		from transactions t
		join profiles p on p.id = t.user_id
		where t.type in ('deposit','withdrawal')`
	args := []any{}
	argN := 1

	if txType != "" {
		q += fmt.Sprintf(" and t.type = $%d", argN)
		args = append(args, txType)
		argN++
	}
	q += fmt.Sprintf(" order by t.created_at desc limit $%d offset $%d", argN, argN+1)
	args = append(args, limit, offset)

	rows, err := d.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []DashboardTransaction
	for rows.Next() {
		var tx DashboardTransaction
		var username string
		if err := rows.Scan(&tx.ID, &tx.UserID, &tx.Type, &tx.Amount, &tx.Status, &tx.CreatedAt, &tx.UpdatedAt, &username); err != nil {
			return nil, 0, err
		}
		tx.AmountKES = tx.Amount
		tx.DisplayName = username
		out = append(out, tx)
	}

	// Get total count
	var total int
	countQ := `select count(*) from transactions t
		join profiles p on p.id = t.user_id
		where t.type in ('deposit','withdrawal')`
	if txType != "" {
		countQ += fmt.Sprintf(" and t.type = '%s'", txType)
	}
	err = d.pool.QueryRow(ctx, countQ).Scan(&total)
	if err != nil {
		return nil, 0, err
	}

	if out == nil {
		out = []DashboardTransaction{}
	}
	return out, total, rows.Err()
}

// ListUserTransactions returns a single user's own deposit/withdrawal
// history for the wallet page. Unlike ListTransactions (which is the
// admin-dashboard's cross-user, paginated view backed by DashboardTransaction),
// this is scoped to user_id at the SQL level and returns the plain
// Transaction shape the wallet UI actually expects (including the M-Pesa
// receipt, which DashboardTransaction doesn't carry).
func (d *DB) ListUserTransactions(ctx context.Context, userID uuid.UUID, limit int) ([]Transaction, error) {
	const q = `
		select id, user_id, type, amount, status, mpesa_receipt, reviewed_by, created_at, updated_at
		from transactions
		where user_id = $1 and type in ('deposit','withdrawal')
		order by created_at desc
		limit $2`
	rows, err := d.pool.Query(ctx, q, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Transaction
	for rows.Next() {
		var t Transaction
		if err := rows.Scan(&t.ID, &t.UserID, &t.Type, &t.Amount, &t.Status, &t.MpesaReceipt, &t.ReviewedBy, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		t.AmountKES = t.Amount
		out = append(out, t)
	}
	if out == nil {
		out = []Transaction{}
	}
	return out, rows.Err()
}

// ---- Admin stats ----

type AdminStats struct {
	TotalUsers               int     `json:"totalUsers"`
	TotalDeposited           float64 `json:"totalDeposited"`
	TotalBalanceReal         float64 `json:"totalBalanceReal"`
	TotalBalanceDemo         float64 `json:"totalBalanceDemo"`
	TotalWithdrawn           float64 `json:"totalWithdrawn"`
	TotalInfluencerWithdrawn float64 `json:"totalInfluencerWithdrawn"`
	PendingWithdrawals       int     `json:"pendingWithdrawals"`
}

func (d *DB) GetAdminStats(ctx context.Context) (*AdminStats, error) {
	var s AdminStats
	err := d.pool.QueryRow(ctx, `
		select
			coalesce(sum(real_balance), 0),
			coalesce(sum(demo_balance), 0),
			count(*)
		from profiles`).Scan(&s.TotalBalanceReal, &s.TotalBalanceDemo, &s.TotalUsers)
	if err != nil {
		return nil, err
	}

	err = d.pool.QueryRow(ctx, `
		select coalesce(sum(amount), 0) from transactions where type = 'deposit' and status = 'completed'`,
	).Scan(&s.TotalDeposited)
	if err != nil {
		return nil, err
	}

	err = d.pool.QueryRow(ctx, `
		select coalesce(sum(amount), 0) from transactions where type = 'withdrawal' and status = 'approved'`,
	).Scan(&s.TotalWithdrawn)
	if err != nil {
		return nil, err
	}

	err = d.pool.QueryRow(ctx, `
		select coalesce(sum(amount), 0) from influencer_withdrawals where status = 'sent'`,
	).Scan(&s.TotalInfluencerWithdrawn)
	if err != nil {
		return nil, err
	}

	err = d.pool.QueryRow(ctx, `
		select count(*) from transactions where type = 'withdrawal' and status = 'pending'`,
	).Scan(&s.PendingWithdrawals)
	if err != nil {
		return nil, err
	}

	return &s, nil
}

// ---- Influencer ----

func (d *DB) GetOrCreateMockMpesa(ctx context.Context, userID uuid.UUID) (float64, error) {
	var amount float64
	err := d.pool.QueryRow(ctx, `select withdrawn_amount from influencer_mock_mpesa where user_id = $1`, userID).Scan(&amount)
	if errors.Is(err, pgx.ErrNoRows) {
		_, err = d.pool.Exec(ctx, `insert into influencer_mock_mpesa (user_id, withdrawn_amount) values ($1, 0)`, userID)
		return 0, err
	}
	return amount, err
}

func (d *DB) InfluencerWithdraw(ctx context.Context, userID uuid.UUID, amount float64) error {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var balance float64
	if err := tx.QueryRow(ctx, `select real_balance from profiles where id = $1 and role = 'influencer' for update`, userID).Scan(&balance); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errors.New("not an influencer account")
		}
		return err
	}
	if balance < amount {
		return errors.New("insufficient influencer balance")
	}

	newBalance := balance - amount
	if _, err := tx.Exec(ctx, `update profiles set real_balance = $2 where id = $1`, userID, newBalance); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		insert into influencer_mock_mpesa (user_id, withdrawn_amount)
		values ($1, $2)
		on conflict (user_id) do update set withdrawn_amount = influencer_mock_mpesa.withdrawn_amount + $2, updated_at = now()`,
		userID, amount); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		insert into influencer_transactions (id, user_id, type, amount, balance_after)
		values ($1, $2, 'withdrawal', $3, $4)`,
		uuid.New(), userID, amount, newBalance); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		insert into influencer_withdrawals (id, user_id, amount, status)
		values ($1, $2, $3, 'pending')`,
		uuid.New(), userID, amount); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

func (d *DB) ListInfluencerWithdrawals(ctx context.Context, limit int) ([]map[string]any, error) {
	rows, err := d.pool.Query(ctx, `
		select iw.id, iw.user_id, p.username, iw.amount, iw.status, iw.created_at
		from influencer_withdrawals iw
		join profiles p on p.id = iw.user_id
		order by iw.created_at desc limit $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []map[string]any
	for rows.Next() {
		var id, userID uuid.UUID
		var username, status string
		var amount float64
		var createdAt time.Time
		if err := rows.Scan(&id, &userID, &username, &amount, &status, &createdAt); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"id":        id,
			"userId":    userID,
			"username":  username,
			"amount":    amount,
			"status":    status,
			"createdAt": createdAt,
		})
	}
	if out == nil {
		out = []map[string]any{}
	}
	return out, rows.Err()
}

func (d *DB) MarkInfluencerWithdrawalStatus(ctx context.Context, id uuid.UUID, status string) error {
	if status != "sent" && status != "failed" && status != "pending" {
		return fmt.Errorf("invalid status %q", status)
	}
	tag, err := d.pool.Exec(ctx, `update influencer_withdrawals set status = $2 where id = $1`, id, status)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (d *DB) MockMpesaWithdraw(ctx context.Context, userID uuid.UUID, amount float64) error {
	tag, err := d.pool.Exec(ctx, `
		update influencer_mock_mpesa set withdrawn_amount = withdrawn_amount - $2, updated_at = now()
		where user_id = $1 and withdrawn_amount >= $2`, userID, amount)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errors.New("insufficient mock mpesa balance")
	}
	return nil
}

func (d *DB) ListInfluencerTransactions(ctx context.Context, userID uuid.UUID, limit int) ([]map[string]any, error) {
	rows, err := d.pool.Query(ctx, `
		select type, amount, balance_after, created_at from influencer_transactions
		where user_id = $1 order by created_at desc limit $2`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []map[string]any
	for rows.Next() {
		var typ string
		var amount, balanceAfter float64
		var createdAt time.Time
		if err := rows.Scan(&typ, &amount, &balanceAfter, &createdAt); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"type":          typ,
			"amount":        amount,
			"balance_after": balanceAfter,
			"created_at":    createdAt,
		})
	}
	return out, rows.Err()
}

// ---- Rounds & bets ----
// Note: RoundRecord and BetRecord are defined in redis.go
// These flush methods use those types.

func (d *DB) FlushRound(ctx context.Context, r RoundRecord, bets []BetRecord) error {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx, `
		insert into rounds (id, server_seed, server_seed_hash, client_seed, nonce, crash_point, started_at, crashed_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8)
		on conflict (id) do nothing`,
		r.ID, r.ServerSeed, r.ServerSeedHash, r.ClientSeed, r.Nonce, r.CrashPoint, r.StartedAt, r.CrashedAt)
	if err != nil {
		return fmt.Errorf("inserting round: %w", err)
	}

	for _, b := range bets {
		_, err = tx.Exec(ctx, `
			insert into bets (id, round_id, user_id, box, amount, cashout_multiplier, payout, status)
			values ($1, $2, $3, $4, $5, $6, $7, $8)
			on conflict (id) do nothing`,
			b.ID, r.ID, b.UserID, b.Box, b.Amount, b.CashoutMultiplier, b.Payout, b.Status)
		if err != nil {
			return fmt.Errorf("inserting bet %s: %w", b.ID, err)
		}

		if b.IsInfluencer {
			evType := "bet"
			if b.Status == "cashed_out" {
				evType = "win"
			}
			_, err = tx.Exec(ctx, `
				insert into influencer_transactions (id, user_id, type, amount, balance_after)
				values ($1, $2, $3, $4, $5)`,
				uuid.New(), b.UserID, evType, b.Amount, b.BalanceAfter)
			if err != nil {
				return fmt.Errorf("inserting influencer bet event: %w", err)
			}
		}
		// Every bet — influencer or not — syncs profiles.real_balance /
		// demo_balance here. This used to be skipped for influencers (only
		// the influencer_transactions log was written), which left Postgres
		// permanently stale for them: Redis stayed correct through play
		// since the fast path (redis.go's Lua scripts) doesn't distinguish
		// influencers, but the instant Redis went cold — restart, evicted
		// key, fresh login — getLiveBalance/EnsureBalance (wallet.go)
		// reseeded it from this untouched Postgres row, snapping the
		// balance back to whatever it was before any bets happened.
		_, err = tx.Exec(ctx, `update profiles set real_balance = $2, demo_balance = $3 where id = $1`,
			b.UserID, b.RealBalanceAfter, b.DemoBalanceAfter)
		if err != nil {
			return fmt.Errorf("syncing balance for %s: %w", b.UserID, err)
		}
	}

	return tx.Commit(ctx)
}
