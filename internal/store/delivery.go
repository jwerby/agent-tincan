package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

const deliveryApplied = "applied"

// Unexpired reports whether request id is still inside its expiry.
func (s *Store) Unexpired(ctx context.Context, id string) (bool, error) {
	var open bool
	err := s.db.QueryRowContext(ctx, `SELECT expires_at > ? FROM requests WHERE id = ?`, s.now().UnixMilli(), id).Scan(&open)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	return open, err
}

// EnqueueDeliveryHold stores req as a held shadow for op (reply or answer)
// and records action on it. A second unexpired hold for the same parent and
// op is ErrPendingHold. req.Status must already be held.
func (s *Store) EnqueueDeliveryHold(ctx context.Context, req envelope.Request, action, op string) (envelope.Request, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return envelope.Request{}, err
	}
	defer tx.Rollback()
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT id FROM requests WHERE parent_id = ? AND status = ? AND json_extract(gate_action, '$.op') = ? AND expires_at > ?`,
		req.ParentID, string(envelope.StatusHeld), op, s.now().UnixMilli()).Scan(&existing)
	if err == nil {
		return envelope.Request{}, ErrPendingHold
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return envelope.Request{}, err
	}
	if req, err = s.enqueueTx(ctx, tx, req, req.HoldTTL); err != nil {
		return envelope.Request{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE requests SET gate_action = ? WHERE id = ?`, action, req.ID); err != nil {
		return envelope.Request{}, err
	}
	return req, tx.Commit()
}

// GateAction returns the stored delivery action for id, or "" when the row
// is an ordinary request.
func (s *Store) GateAction(ctx context.Context, id string) (string, error) {
	var action string
	err := s.db.QueryRowContext(ctx, `SELECT gate_action FROM requests WHERE id = ?`, id).Scan(&action)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return action, err
}

// UnbindAttachments clears request_id on every attachment bound to requestID.
func (s *Store) UnbindAttachments(ctx context.Context, requestID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE attachments SET request_id = '' WHERE request_id = ?`, requestID)
	return err
}

// FinishHeldDelivery marks a held delivery shadow done without queueing it.
func (s *Store) FinishHeldDelivery(ctx context.Context, id string) error {
	now := s.now().UnixMilli()
	res, err := s.db.ExecContext(ctx, `UPDATE requests SET status = ?, approved = 1, body = '', updated_at = ? WHERE id = ? AND status = ?`,
		deliveryApplied, now, id, string(envelope.StatusHeld))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrWrongState
	}
	_, err = s.db.ExecContext(ctx, `UPDATE requests_fts SET body = '' WHERE rowid = (SELECT rowid FROM requests WHERE id = ?)`, id)
	return err
}
