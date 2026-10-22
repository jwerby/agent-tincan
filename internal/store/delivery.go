package store

import (
	"context"
	"database/sql"
	"encoding/json"
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
// op is ErrPendingHold. req.Status must already be held. pauseParentLease
// freezes the parent's claim so a held clarification is still approvable
// after the claim lease would have elapsed.
func (s *Store) EnqueueDeliveryHold(ctx context.Context, req envelope.Request, action, op string, pauseParentLease bool) (envelope.Request, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return envelope.Request{}, err
	}
	defer tx.Rollback()
	now := s.now().UnixMilli()
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT id FROM requests WHERE parent_id = ? AND status = ? AND json_extract(gate_action, '$.op') = ? AND expires_at > ?`,
		req.ParentID, string(envelope.StatusHeld), op, now).Scan(&existing)
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
	// A progress note is the claimer's text. The hold exists to keep that
	// delivery off the asker, a council, and approval.json, so the note goes
	// with it until the hold is gone.
	if req.ParentID != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE requests SET progress_note = '', progress_at = 0 WHERE id = ?`, req.ParentID); err != nil {
			return envelope.Request{}, err
		}
	}
	if pauseParentLease {
		res, err := tx.ExecContext(ctx, `UPDATE requests SET lease_paused = 1, updated_at = ? WHERE id = ? AND status = ?`,
			now, req.ParentID, string(envelope.StatusClaimed))
		if err != nil {
			return envelope.Request{}, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return envelope.Request{}, ErrWrongState
		}
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

// HeldDelivery is a held reply or answer after it has been applied to its parent.
type HeldDelivery struct {
	Op     string
	Shadow envelope.Request
	Parent envelope.Request
	Reply  envelope.Reply
}

// ApplyHeldDelivery unbinds the shadow's attachments, writes the parent reply
// or answer, and marks the shadow applied, in one transaction. If the parent
// write fails, the parent is unchanged and the shadow stays held.
func (s *Store) ApplyHeldDelivery(ctx context.Context, id string) (HeldDelivery, error) {
	var out HeldDelivery
	err := s.commitHeldDelivery(ctx, id, func(ctx context.Context, tx *sql.Tx) error {
		shadow, status, err := scanRequest(tx.QueryRowContext(ctx, `SELECT `+requestCols+` FROM requests WHERE id = ?`, id))
		if err != nil {
			return err
		}
		if status != envelope.StatusHeld {
			return ErrWrongState
		}
		var raw string
		if err := tx.QueryRowContext(ctx, `SELECT gate_action FROM requests WHERE id = ?`, id).Scan(&raw); err != nil {
			return err
		}
		var act struct {
			Op       string `json:"op"`
			Status   string `json:"status"`
			ParentID string `json:"parent_id"`
			Email    bool   `json:"email"`
		}
		if json.Unmarshal([]byte(raw), &act) != nil || act.ParentID == "" || act.ParentID != shadow.ParentID {
			return ErrWrongState
		}
		switch act.Op {
		case "reply":
			if act.Status == "" {
				return ErrWrongState
			}
			rep := envelope.Reply{Body: shadow.Body, Status: envelope.Status(act.Status), Attachments: shadow.Attachments}
			if rep.Status == envelope.StatusNeedsInput {
				rep, err = s.needsInputTx(ctx, tx, shadow.ParentID, shadow.From, rep)
			} else {
				// An email reply was gated when it was held. Apply it with
				// the same guard, in this transaction: a live claim or an
				// unapproved hold rolls the unbind and the parent write back
				// and leaves the shadow held.
				rep, err = s.replyTx(ctx, tx, shadow.ParentID, shadow.From, rep, act.Email)
			}
			if err != nil {
				return err
			}
			out.Reply = rep
		case "answer":
			parent, err := s.answerTx(ctx, tx, shadow.ParentID, shadow.From, shadow.Body)
			if err != nil {
				return err
			}
			out.Parent = parent
		default:
			return ErrWrongState
		}
		out.Op = act.Op
		out.Shadow = shadow
		return nil
	})
	if err != nil {
		return HeldDelivery{}, err
	}
	return out, nil
}

// commitHeldDelivery runs unbind, apply, and marking the shadow applied in
// one transaction. apply is the parent write; its error rolls back the unbind.
func (s *Store) commitHeldDelivery(ctx context.Context, shadowID string, apply func(context.Context, *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := requireHeldTx(ctx, tx, shadowID, s.now().UnixMilli()); err != nil {
		return err
	}
	if err := unbindAttachmentsTx(ctx, tx, shadowID); err != nil {
		return err
	}
	if err := apply(ctx, tx); err != nil {
		return err
	}
	if err := finishHeldTx(ctx, tx, shadowID, s.now().UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

func requireHeldTx(ctx context.Context, tx *sql.Tx, id string, now int64) error {
	var status string
	var expires int64
	err := tx.QueryRowContext(ctx, `SELECT status, expires_at FROM requests WHERE id = ?`, id).Scan(&status, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	// Release refuses an overdue hold. Applying one after a missed sweep
	// would deliver text the owner can no longer approve.
	if status != string(envelope.StatusHeld) || expires <= now {
		return ErrWrongState
	}
	return nil
}

// openDeliveryHold matches an unexpired held child of the outer requests row:
// a reply or answer shadow still waiting on the owner.
const openDeliveryHold = `EXISTS (SELECT 1 FROM requests AS held WHERE held.parent_id = requests.id AND held.status = ? AND held.expires_at > ?)`

type adminProgressKey struct{}

// WithAdminProgress marks ctx as an admin read. Get and Trace keep a
// progress note for that caller while a delivery hold is open; every other
// caller has the note omitted until the hold is gone.
func WithAdminProgress(ctx context.Context) context.Context {
	return context.WithValue(ctx, adminProgressKey{}, true)
}

func adminProgress(ctx context.Context) bool {
	v, _ := ctx.Value(adminProgressKey{}).(bool)
	return v
}

// hideHeldProgress drops a progress note while parent req has an unexpired
// delivery hold, except on an admin read.
func (s *Store) hideHeldProgress(ctx context.Context, req *envelope.Request) error {
	if req.Progress == nil || adminProgress(ctx) {
		return nil
	}
	open, err := s.OpenDeliveryHold(ctx, req.ID)
	if err != nil || !open {
		return err
	}
	req.Progress = nil
	return nil
}

// OverlayProgress writes the progress columns of id without the hold check
// SetProgressLeases enforces. Tests use it to leave a note in place while a
// delivery hold is open.
func (s *Store) OverlayProgress(ctx context.Context, id, note string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE requests SET progress_note = ?, progress_at = ? WHERE id = ?`, note, s.now().UnixMilli(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// OpenDeliveryHold reports whether parentID has an unexpired delivery hold.
func (s *Store) OpenDeliveryHold(ctx context.Context, parentID string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM requests WHERE parent_id = ? AND status = ? AND expires_at > ? LIMIT 1`,
		parentID, string(envelope.StatusHeld), s.now().UnixMilli()).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func unbindAttachmentsTx(ctx context.Context, tx *sql.Tx, requestID string) error {
	_, err := tx.ExecContext(ctx, `UPDATE attachments SET request_id = '' WHERE request_id = ?`, requestID)
	return err
}

// clarificationHold reports whether action is a held needs_input reply,
// the hold that pauses the parent's claim.
func clarificationHold(action string) bool {
	var act struct {
		Op     string `json:"op"`
		Status string `json:"status"`
	}
	if json.Unmarshal([]byte(action), &act) != nil {
		return false
	}
	return act.Op == "reply" && act.Status == string(envelope.StatusNeedsInput)
}

const liveClarificationHold = `held.parent_id = parent.id AND held.status = ? AND held.expires_at > ?
	AND json_extract(held.gate_action, '$.op') = 'reply'
	AND json_extract(held.gate_action, '$.status') = ?`

// resumeIdleClaims lets a claim run again when its held clarification is gone.
func (s *Store) resumeIdleClaims(ctx context.Context, now int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE requests SET lease_paused = 0 WHERE id IN (
		SELECT parent.id FROM requests AS parent
		WHERE parent.status = ? AND parent.lease_paused = 1
		AND NOT EXISTS (SELECT 1 FROM requests AS held WHERE `+liveClarificationHold+`)
	)`, string(envelope.StatusClaimed), string(envelope.StatusHeld), now, string(envelope.StatusNeedsInput))
	return err
}

type sqlExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func resumeClaimIfIdle(ctx context.Context, ex sqlExecer, parentID string, now int64) error {
	_, err := ex.ExecContext(ctx, `UPDATE requests SET lease_paused = 0 WHERE id = ? AND status = ? AND lease_paused = 1
		AND NOT EXISTS (SELECT 1 FROM requests AS held WHERE held.parent_id = requests.id AND held.status = ? AND held.expires_at > ?
			AND json_extract(held.gate_action, '$.op') = 'reply'
			AND json_extract(held.gate_action, '$.status') = ?)`,
		parentID, string(envelope.StatusClaimed), string(envelope.StatusHeld), now, string(envelope.StatusNeedsInput))
	return err
}

func finishHeldTx(ctx context.Context, tx *sql.Tx, id string, now int64) error {
	res, err := tx.ExecContext(ctx, `UPDATE requests SET status = ?, approved = 1, body = '', updated_at = ? WHERE id = ? AND status = ? AND expires_at > ?`,
		deliveryApplied, now, id, string(envelope.StatusHeld), now)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrWrongState
	}
	_, err = tx.ExecContext(ctx, `UPDATE requests_fts SET body = '' WHERE rowid = (SELECT rowid FROM requests WHERE id = ?)`, id)
	return err
}
