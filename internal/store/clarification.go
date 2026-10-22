package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

func (s *Store) migrateClarification() error {
	for _, col := range []struct{ name, definition string }{
		{"reply_generation", "INTEGER NOT NULL DEFAULT 0"},
		{"exchanges", "TEXT NOT NULL DEFAULT '[]'"},
		{"lease_paused", "INTEGER NOT NULL DEFAULT 0"},
		{"resumed", "INTEGER NOT NULL DEFAULT 0"},
	} {
		var has bool
		if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_table_info('requests') WHERE name = ?)`, col.name).Scan(&has); err != nil {
			return err
		}
		if !has {
			if _, err := s.db.Exec(`ALTER TABLE requests ADD COLUMN ` + col.name + ` ` + col.definition); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) needsInput(ctx context.Context, id, agent string, rep envelope.Reply) (envelope.Reply, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return envelope.Reply{}, err
	}
	defer tx.Rollback()
	rep, err = s.needsInputTx(ctx, tx, id, agent, rep)
	if err != nil {
		return envelope.Reply{}, err
	}
	return rep, tx.Commit()
}

// needsInputTx records a clarification on a claimed ask. A paused claim
// (a clarification already held for approval) applies even after its lease
// time has passed; a running claim still needs a live lease.
func (s *Store) needsInputTx(ctx context.Context, tx *sql.Tx, id, agent string, rep envelope.Reply) (envelope.Reply, error) {
	if err := envelope.ValidateInput(rep.Body); err != nil {
		return envelope.Reply{}, err
	}
	if len(rep.Attachments) != 0 {
		return envelope.Reply{}, ErrWrongState
	}
	now := s.now()
	req, status, err := scanRequest(tx.QueryRowContext(ctx, `SELECT `+requestCols+` FROM requests WHERE id = ?`, id))
	if err != nil {
		return envelope.Reply{}, err
	}
	if req.To != agent {
		return envelope.Reply{}, ErrForbidden
	}
	if status != envelope.StatusClaimed || req.Kind != envelope.KindAsk || len(req.Exchanges) >= envelope.MaxExchanges {
		return envelope.Reply{}, ErrWrongState
	}
	req.Exchanges = append(req.Exchanges, envelope.Exchange{Question: rep.Body, At: now.UTC().Truncate(time.Millisecond)})
	exchanges, err := json.Marshal(req.Exchanges)
	if err != nil {
		return envelope.Reply{}, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE requests SET status = ?, exchanges = ?, lease_paused = 1, lease_until = 0, updated_at = ?, reply_seen_at = 0, reply_generation = reply_generation + 1
		WHERE id = ? AND status = ? AND expires_at > ? AND (lease_until > ? OR lease_paused = 1)`, envelope.StatusNeedsInput, string(exchanges), now.UnixMilli(), id, envelope.StatusClaimed, now.UnixMilli(), now.UnixMilli())
	if err != nil {
		return envelope.Reply{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return envelope.Reply{}, ErrWrongState
	}
	if err := tx.QueryRowContext(ctx, `SELECT reply_generation FROM requests WHERE id = ?`, id).Scan(&rep.Generation); err != nil {
		return envelope.Reply{}, err
	}
	rep.RequestID, rep.From, rep.CreatedAt = id, agent, now.UTC().Truncate(time.Millisecond)
	if _, err := tx.ExecContext(ctx, `INSERT INTO replies(request_id, from_agent, status, body, created_at) VALUES (?, ?, ?, ?, ?)`, id, agent, rep.Status, rep.Body, now.UnixMilli()); err != nil {
		return envelope.Reply{}, err
	}
	return rep, nil
}

// Answer records input from the original sender and queues the same request again.
func (s *Store) Answer(ctx context.Context, id, agent, body string) (envelope.Request, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return envelope.Request{}, err
	}
	defer tx.Rollback()
	req, err := s.answerTx(ctx, tx, id, agent, body)
	if err != nil {
		return envelope.Request{}, err
	}
	return req, tx.Commit()
}

func (s *Store) answerTx(ctx context.Context, tx *sql.Tx, id, agent, body string) (envelope.Request, error) {
	if err := envelope.ValidateInput(body); err != nil {
		return envelope.Request{}, err
	}
	req, status, err := scanRequest(tx.QueryRowContext(ctx, `SELECT `+requestCols+` FROM requests WHERE id = ?`, id))
	if err != nil {
		return envelope.Request{}, err
	}
	if req.From != agent {
		return envelope.Request{}, ErrForbidden
	}
	if status != envelope.StatusNeedsInput || len(req.Exchanges) == 0 {
		return envelope.Request{}, ErrWrongState
	}
	req.Exchanges[len(req.Exchanges)-1].Answer = body
	exchanges, err := json.Marshal(req.Exchanges)
	if err != nil {
		return envelope.Request{}, err
	}
	now := s.now().UnixMilli()
	res, err := tx.ExecContext(ctx, `UPDATE requests SET status = ?, exchanges = ?, lease_paused = 0, lease_until = 0, resumed = 1, updated_at = ?, reply_seen_at = ?,
		progress_note = '', progress_at = 0
		WHERE id = ? AND status = ? AND expires_at > ?`, envelope.StatusQueued, string(exchanges), now, now, id, envelope.StatusNeedsInput, now)
	if err != nil {
		return envelope.Request{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return envelope.Request{}, ErrWrongState
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM replies WHERE request_id = ?`, id); err != nil {
		return envelope.Request{}, err
	}
	req.Resumed = true
	return req, nil
}
