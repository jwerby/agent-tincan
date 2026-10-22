package store

import (
	"context"
	"database/sql"
	"errors"
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
