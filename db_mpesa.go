package main

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// db_mpesa.go: storage for FCM device tokens and the mock M-PESA message
// inbox (mpesa-messages.html). Tables: device_tokens, mpesa_messages — see
// mpesa_messages_migration.sql.

// MpesaMessage is one mock "You have received Ksh..." SMS.
type MpesaMessage struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	Amount    float64
	Sender    string
	Body      string
	CreatedAt time.Time
}

// UpsertDeviceToken registers (or re-assigns) an FCM token to a user. The app
// calls this on every page load, so it must be idempotent.
func (d *DB) UpsertDeviceToken(ctx context.Context, userID uuid.UUID, token string) error {
	_, err := d.pool.Exec(ctx, `
		insert into device_tokens (token, user_id) values ($1, $2)
		on conflict (token) do update set user_id = excluded.user_id, updated_at = now()`,
		token, userID)
	return err
}

func (d *DB) ListDeviceTokens(ctx context.Context, userID uuid.UUID) ([]string, error) {
	rows, err := d.pool.Query(ctx, `select token from device_tokens where user_id = $1`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (d *DB) DeleteDeviceToken(ctx context.Context, token string) error {
	_, err := d.pool.Exec(ctx, `delete from device_tokens where token = $1`, token)
	return err
}

// InsertMpesaMessage stores a message. withdrawalID is optional; when set, a
// second insert for the same withdrawal is a no-op and (nil, nil) is returned,
// so marking a withdrawal "sent" twice can't produce two messages.
func (d *DB) InsertMpesaMessage(ctx context.Context, userID uuid.UUID, amount float64, sender, body string, withdrawalID *uuid.UUID) (*MpesaMessage, error) {
	var wid any
	if withdrawalID != nil {
		wid = *withdrawalID
	}
	m := &MpesaMessage{UserID: userID, Amount: amount, Sender: sender, Body: body}
	err := d.pool.QueryRow(ctx, `
		insert into mpesa_messages (user_id, amount, sender, body, withdrawal_id)
		values ($1, $2, $3, $4, $5)
		on conflict (withdrawal_id) where withdrawal_id is not null do nothing
		returning id, created_at`,
		userID, amount, sender, body, wid).Scan(&m.ID, &m.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return m, nil
}

// ListMpesaMessages returns the user's most recent messages, oldest first
// (the page appends them in order).
func (d *DB) ListMpesaMessages(ctx context.Context, userID uuid.UUID, limit int) ([]MpesaMessage, error) {
	rows, err := d.pool.Query(ctx, `
		select id, coalesce(amount, 0), coalesce(sender, ''), body, created_at from (
			select id, amount, sender, body, created_at from mpesa_messages
			where user_id = $1 order by created_at desc limit $2
		) recent order by created_at asc`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MpesaMessage
	for rows.Next() {
		m := MpesaMessage{UserID: userID}
		if err := rows.Scan(&m.ID, &m.Amount, &m.Sender, &m.Body, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetInfluencerWithdrawal returns who a withdrawal belongs to and its amount.
func (d *DB) GetInfluencerWithdrawal(ctx context.Context, id uuid.UUID) (uuid.UUID, float64, error) {
	var userID uuid.UUID
	var amount float64
	err := d.pool.QueryRow(ctx,
		`select user_id, amount from influencer_withdrawals where id = $1`, id).Scan(&userID, &amount)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, 0, ErrNotFound
	}
	return userID, amount, err
}
