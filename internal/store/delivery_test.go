package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// A failed parent write rolls back the unbind. The parent is unchanged and
// the shadow stays held, with its attachment still bound to it.
func TestApplyHeldDeliveryRollsBackFailedReply(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	parent := ask(t, s, "muse", "grokbot", "book a table")
	if _, err := s.Claim(ctx, parent.ID, "grokbot", time.Minute); err != nil {
		t.Fatal(err)
	}
	att := upload(t, s, "grokbot", "menu.png", 12)
	action := `{"op":"reply","status":"answered","parent_id":"` + parent.ID + `"}`
	shadow, err := s.EnqueueDeliveryHold(ctx, envelope.Request{
		From: "grokbot", To: "muse", ParentID: parent.ID, Kind: envelope.KindAsk, Body: "booked",
		Status: envelope.StatusHeld, HoldTTL: time.Hour, Chain: []string{"grokbot"},
		Attachments: []envelope.Attachment{{ID: att}},
	}, action, "reply", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE requests SET status = ? WHERE id = ?`, envelope.StatusCancelled, parent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyHeldDelivery(ctx, shadow.ID); err == nil {
		t.Fatal("apply succeeded after the parent was no longer open")
	}
	got, status, err := s.Request(ctx, parent.ID)
	if err != nil || status != envelope.StatusCancelled || got.Body != "book a table" {
		t.Fatalf("parent = %+v %s %v, want it unchanged", got, status, err)
	}
	if result, err := s.Get(ctx, parent.ID, "muse"); err != nil || result.Reply != nil {
		t.Fatalf("parent reply = %+v %v, want none", result.Reply, err)
	}
	_, shadowStatus, err := s.Request(ctx, shadow.ID)
	if err != nil || shadowStatus != envelope.StatusHeld {
		t.Fatalf("shadow status = %s %v, want held", shadowStatus, err)
	}
	rec, err := s.Attachment(ctx, att)
	if err != nil || rec.RequestID != shadow.ID {
		t.Fatalf("attachment bound to %q, %v; want the shadow", rec.RequestID, err)
	}
}

// The second step is the parent write. Its error leaves the unbind uncommitted.
func TestCommitHeldDeliverySecondStepRollsBack(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	parent := ask(t, s, "muse", "grokbot", "book a table")
	att := upload(t, s, "grokbot", "menu.png", 12)
	shadow, err := s.Enqueue(ctx, envelope.Request{
		From: "grokbot", To: "muse", ParentID: parent.ID, Kind: envelope.KindAsk, Body: "booked",
		Status: envelope.StatusHeld, HoldTTL: time.Hour, Chain: []string{"grokbot"},
		Attachments: []envelope.Attachment{{ID: att}},
	}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("second step failed")
	if err := s.commitHeldDelivery(ctx, shadow.ID, func(context.Context, *sql.Tx) error {
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("commit = %v, want the second step's error", err)
	}
	got, status, err := s.Request(ctx, parent.ID)
	if err != nil || status != envelope.StatusQueued || got.Body != parent.Body {
		t.Fatalf("parent = %+v %s %v, want it unchanged", got, status, err)
	}
	if result, err := s.Get(ctx, parent.ID, "muse"); err != nil || result.Reply != nil {
		t.Fatalf("parent reply = %+v %v, want none", result.Reply, err)
	}
	_, shadowStatus, err := s.Request(ctx, shadow.ID)
	if err != nil || shadowStatus != envelope.StatusHeld {
		t.Fatalf("shadow status = %s %v, want held", shadowStatus, err)
	}
	rec, err := s.Attachment(ctx, att)
	if err != nil || rec.RequestID != shadow.ID {
		t.Fatalf("attachment bound to %q, %v; want the shadow", rec.RequestID, err)
	}
}

func holdReply(t *testing.T, s *Store, parent envelope.Request, body, action string, pause bool) envelope.Request {
	t.Helper()
	shadow, err := s.EnqueueDeliveryHold(context.Background(), envelope.Request{
		From: parent.To, To: parent.From, ParentID: parent.ID, Kind: envelope.KindAsk, Body: body,
		Status: envelope.StatusHeld, HoldTTL: time.Minute, Chain: []string{parent.To},
	}, action, "reply", pause)
	if err != nil {
		t.Fatal(err)
	}
	return shadow
}

func replyAction(parentID string, email bool) string {
	action := `{"op":"reply","status":"answered","parent_id":"` + parentID + `"`
	if email {
		action += `,"email":true`
	}
	return action + "}"
}

// A delivery hold clears the parent's progress note, rejects a new one, and
// hides any note that is still stored from get and trace. An admin read
// still receives it. Once the hold expires, progress works again.
func TestProgressBlockedWhileDeliveryHeld(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	parent := ask(t, s, "muse", "grokbot", "book a table")
	if _, err := s.db.ExecContext(ctx, `UPDATE requests SET expires_at = ? WHERE id = ?`, c.t.Add(24*time.Hour).UnixMilli(), parent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(ctx, parent.ID, "grokbot", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProgress(ctx, parent.ID, "grokbot", "calling the restaurant", time.Hour); err != nil {
		t.Fatal(err)
	}
	shadow := holdReply(t, s, parent, "booked", replyAction(parent.ID, false), false)
	got, err := s.Get(ctx, parent.ID, "muse")
	if err != nil || got.Progress != nil {
		t.Fatalf("get during hold = %+v %v, want no progress", got.Progress, err)
	}
	var note string
	var at int64
	if err := s.db.QueryRowContext(ctx, `SELECT progress_note, progress_at FROM requests WHERE id = ?`, parent.ID).Scan(&note, &at); err != nil {
		t.Fatal(err)
	}
	if note != "" || at != 0 {
		t.Fatalf("stored progress = %q at %d, want it cleared", note, at)
	}
	if err := s.SetProgress(ctx, parent.ID, "grokbot", "still calling", time.Hour); !errors.Is(err, ErrWrongState) {
		t.Fatalf("progress during hold = %v, want ErrWrongState", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE requests SET progress_note = ?, progress_at = ? WHERE id = ?`, "secret note", c.t.UnixMilli(), parent.ID); err != nil {
		t.Fatal(err)
	}
	got, err = s.Get(ctx, parent.ID, "muse")
	if err != nil || got.Progress != nil {
		t.Fatalf("asker get = %+v %v, want the note omitted", got.Progress, err)
	}
	got, err = s.Get(ctx, parent.ID, "grokbot")
	if err != nil || got.Progress != nil {
		t.Fatalf("claimer get = %+v %v, want the note omitted", got.Progress, err)
	}
	steps, err := s.Trace(ctx, parent.TraceID)
	if err != nil || len(steps) != 1 || steps[0].Progress != nil {
		t.Fatalf("trace = %+v %v, want the note omitted", steps, err)
	}
	admin, err := s.Get(WithAdminProgress(ctx), parent.ID, "muse")
	if err != nil || admin.Progress == nil || admin.Progress.Note != "secret note" {
		t.Fatalf("admin get = %+v %v, want the note", admin.Progress, err)
	}
	adminSteps, err := s.Trace(WithAdminProgress(ctx), parent.TraceID)
	if err != nil || len(adminSteps) != 1 || adminSteps[0].Progress == nil || adminSteps[0].Progress.Note != "secret note" {
		t.Fatalf("admin trace = %+v %v, want the note", adminSteps, err)
	}
	c.advance(2 * time.Minute)
	if _, err := s.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	_, shadowStatus, err := s.Request(ctx, shadow.ID)
	if err != nil || shadowStatus != envelope.StatusExpired {
		t.Fatalf("shadow after sweep = %s %v, want expired", shadowStatus, err)
	}
	if err := s.SetProgress(ctx, parent.ID, "grokbot", "calling again", time.Hour); err != nil {
		t.Fatal(err)
	}
	got, err = s.Get(ctx, parent.ID, "muse")
	if err != nil || got.Progress == nil || got.Progress.Note != "calling again" {
		t.Fatalf("get after the hold = %+v %v", got.Progress, err)
	}
}

// An overdue hold is not applied, and finish refuses it even when the row
// is still marked held. The parent and the shadow stay as they were.
func TestApplyHeldDeliveryRejectsExpiredHold(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	parent := ask(t, s, "muse", "grokbot", "book a table")
	if _, err := s.Claim(ctx, parent.ID, "grokbot", time.Hour); err != nil {
		t.Fatal(err)
	}
	shadow := holdReply(t, s, parent, "booked", replyAction(parent.ID, false), false)
	c.advance(2 * time.Minute)
	if _, err := s.ApplyHeldDelivery(ctx, shadow.ID); !errors.Is(err, ErrWrongState) {
		t.Fatalf("apply expired = %v, want ErrWrongState", err)
	}
	got, status, err := s.Request(ctx, parent.ID)
	if err != nil || status != envelope.StatusClaimed || got.Body != "book a table" {
		t.Fatalf("parent = %+v %s %v, want it unchanged", got, status, err)
	}
	_, shadowStatus, err := s.Request(ctx, shadow.ID)
	if err != nil || shadowStatus != envelope.StatusHeld {
		t.Fatalf("shadow status = %s %v, want held", shadowStatus, err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := requireHeldTx(ctx, tx, shadow.ID, s.now().UnixMilli()); !errors.Is(err, ErrWrongState) {
		t.Fatalf("requireHeldTx = %v, want ErrWrongState", err)
	}
	if err := finishHeldTx(ctx, tx, shadow.ID, s.now().UnixMilli()); !errors.Is(err, ErrWrongState) {
		t.Fatalf("finishHeldTx = %v, want ErrWrongState", err)
	}
}

// A new claim is refused while any unexpired hold exists, and accepted once
// that hold has expired.
func TestClaimBlockedWhileHoldOpen(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	parent := ask(t, s, "muse", "grokbot", "book a table")
	if _, err := s.db.ExecContext(ctx, `UPDATE requests SET expires_at = ? WHERE id = ?`, c.t.Add(24*time.Hour).UnixMilli(), parent.ID); err != nil {
		t.Fatal(err)
	}
	holdReply(t, s, parent, "booked", replyAction(parent.ID, true), false)
	if _, err := s.Claim(ctx, parent.ID, "grokbot", time.Hour); !errors.Is(err, ErrWrongState) {
		t.Fatalf("claim during hold = %v, want ErrWrongState", err)
	}
	c.advance(2 * time.Minute)
	if _, err := s.Claim(ctx, parent.ID, "grokbot", time.Hour); err != nil {
		t.Fatalf("claim after the hold expired: %v", err)
	}
}

// An email reply applies with the email guard. A live claim makes the
// transaction roll back: the shadow stays held, the parent stays claimed,
// and its attachment stays bound. Without that claim, the same reply lands.
func TestEmailHeldReplyUsesGuard(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	parent := ask(t, s, "muse", "grokbot", "book a table")
	att := upload(t, s, "grokbot", "menu.png", 12)
	shadow, err := s.EnqueueDeliveryHold(ctx, envelope.Request{
		From: "grokbot", To: "muse", ParentID: parent.ID, Kind: envelope.KindAsk, Body: "booked",
		Status: envelope.StatusHeld, HoldTTL: time.Hour, Chain: []string{"grokbot"},
		Attachments: []envelope.Attachment{{ID: att}},
	}, replyAction(parent.ID, true), "reply", false)
	if err != nil {
		t.Fatal(err)
	}
	action, err := s.GateAction(ctx, shadow.ID)
	if err != nil || !strings.Contains(action, `"email":true`) {
		t.Fatalf("gate action = %q %v, want email", action, err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE requests SET status = ?, lease_until = ? WHERE id = ?`,
		envelope.StatusClaimed, c.t.Add(time.Hour).UnixMilli(), parent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyHeldDelivery(ctx, shadow.ID); !errors.Is(err, ErrWrongState) {
		t.Fatalf("apply over a live claim = %v, want ErrWrongState", err)
	}
	got, status, err := s.Request(ctx, parent.ID)
	if err != nil || status != envelope.StatusClaimed || got.Body != "book a table" {
		t.Fatalf("parent = %+v %s %v, want the claim kept", got, status, err)
	}
	if result, err := s.Get(ctx, parent.ID, "muse"); err != nil || result.Reply != nil {
		t.Fatalf("parent reply = %+v %v, want none", result.Reply, err)
	}
	_, shadowStatus, err := s.Request(ctx, shadow.ID)
	if err != nil || shadowStatus != envelope.StatusHeld {
		t.Fatalf("shadow status = %s %v, want held", shadowStatus, err)
	}
	rec, err := s.Attachment(ctx, att)
	if err != nil || rec.RequestID != shadow.ID {
		t.Fatalf("attachment bound to %q, %v; want the shadow", rec.RequestID, err)
	}

	if _, err := s.db.ExecContext(ctx, `UPDATE requests SET status = ?, lease_until = 0 WHERE id = ?`, envelope.StatusQueued, parent.ID); err != nil {
		t.Fatal(err)
	}
	applied, err := s.ApplyHeldDelivery(ctx, shadow.ID)
	if err != nil || applied.Reply.Body != "booked" || applied.Reply.Status != envelope.StatusAnswered {
		t.Fatalf("apply = %+v %v, want the email reply", applied.Reply, err)
	}
	got, status, err = s.Request(ctx, parent.ID)
	if err != nil || status != envelope.StatusAnswered {
		t.Fatalf("parent = %+v %s %v, want answered", got, status, err)
	}
}

// A held reply that did not come from email still closes a claim the agent
// already held when the reply was queued.
func TestHeldReplyAppliesOverExistingClaim(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	parent := ask(t, s, "muse", "grokbot", "book a table")
	if _, err := s.Claim(ctx, parent.ID, "grokbot", time.Hour); err != nil {
		t.Fatal(err)
	}
	shadow := holdReply(t, s, parent, "booked", replyAction(parent.ID, false), false)
	applied, err := s.ApplyHeldDelivery(ctx, shadow.ID)
	if err != nil || applied.Reply.Body != "booked" {
		t.Fatalf("apply = %+v %v", applied.Reply, err)
	}
	_, status, err := s.Request(ctx, parent.ID)
	if err != nil || status != envelope.StatusAnswered {
		t.Fatalf("parent status = %s %v, want answered", status, err)
	}
}
