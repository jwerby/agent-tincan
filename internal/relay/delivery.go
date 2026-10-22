package relay

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

type deliveryAction struct {
	Op       string          `json:"op"`
	Status   envelope.Status `json:"status,omitempty"`
	ParentID string          `json:"parent_id"`
	Email    bool            `json:"email,omitempty"`
}

func (s *Server) holdReply(ctx context.Context, id, agent string, rep envelope.Reply, emailMessageID string, round int) (envelope.Reply, bool, error) {
	parent, status, err := s.store.Request(ctx, id)
	if err != nil {
		return envelope.Reply{}, true, err
	}
	if err := s.replyRefusal(ctx, parent, status, agent, rep, emailMessageID != "", round); err != nil {
		return envelope.Reply{}, true, err
	}
	gater, ok := s.preparer().(DeliveryGater)
	if !ok {
		return envelope.Reply{}, false, nil
	}
	// The gate sees the whole chain, not only the replier: a name earlier in
	// the parent is enough to hold the reply.
	chain := append(slices.Clone(parent.Chain), agent)
	msg := &envelope.Request{From: agent, To: parent.From, Kind: parent.Kind, Body: rep.Body, Chain: chain}
	held, err := gater.HoldDelivery(ctx, msg)
	if err != nil || !held {
		return envelope.Reply{}, err != nil, err
	}
	shadow := envelope.Request{
		From: agent, To: parent.From, ParentID: parent.ID, TraceID: parent.TraceID,
		Hop: parent.Hop + 1, Chain: chain, Kind: parent.Kind, Body: rep.Body,
		Status: envelope.StatusHeld, HoldTTL: msg.HoldTTL, ApprovalNotify: msg.ApprovalNotify,
	}
	if shadow.Hop < 1 {
		shadow.Hop = 1
	}
	if rep.Status != envelope.StatusNeedsInput {
		shadow.Attachments = rep.Attachments
	}
	return s.enqueueHold(ctx, shadow, deliveryAction{Op: "reply", Status: rep.Status, ParentID: parent.ID}, agent, emailMessageID, parent.ID)
}

func (s *Server) holdAnswer(ctx context.Context, id, agent, body string) (envelope.Request, bool, error) {
	parent, status, err := s.store.Request(ctx, id)
	if err != nil {
		return envelope.Request{}, true, err
	}
	if err := s.answerRefusal(ctx, parent, status, agent); err != nil {
		return envelope.Request{}, true, err
	}
	gater, ok := s.preparer().(DeliveryGater)
	if !ok {
		return envelope.Request{}, false, nil
	}
	chain := append(slices.Clone(parent.Chain), agent)
	msg := &envelope.Request{From: agent, To: parent.To, Kind: parent.Kind, Body: body, Chain: chain}
	held, err := gater.HoldDelivery(ctx, msg)
	if err != nil || !held {
		return envelope.Request{}, err != nil, err
	}
	shadow := envelope.Request{
		From: agent, To: parent.To, ParentID: parent.ID, TraceID: parent.TraceID,
		Hop: parent.Hop + 1, Chain: chain, Kind: parent.Kind, Body: body,
		Status: envelope.StatusHeld, HoldTTL: msg.HoldTTL, ApprovalNotify: msg.ApprovalNotify,
	}
	if shadow.Hop < 1 {
		shadow.Hop = 1
	}
	rep, _, err := s.enqueueHold(ctx, shadow, deliveryAction{Op: "answer", ParentID: parent.ID}, agent, "", parent.ID)
	if err != nil {
		return envelope.Request{}, true, err
	}
	shadow.ID = rep.RequestID
	shadow.CreatedAt = rep.CreatedAt
	return shadow, true, nil
}

func (s *Server) enqueueHold(ctx context.Context, shadow envelope.Request, action deliveryAction, agent, emailMessageID, emailRequestID string) (envelope.Reply, bool, error) {
	action.Email = emailMessageID != ""
	raw, err := json.Marshal(action)
	if err != nil {
		return envelope.Reply{}, true, err
	}
	pauseLease := action.Op == "reply" && action.Status == envelope.StatusNeedsInput
	shadow, err = s.store.EnqueueDeliveryHold(ctx, shadow, string(raw), action.Op, pauseLease)
	if err != nil {
		return envelope.Reply{}, true, err
	}
	if emailMessageID != "" {
		if err := s.store.DecideEmail(ctx, emailMessageID, agent, emailRequestID, emailHeld, true); err != nil {
			_ = s.store.Cancel(ctx, shadow.ID, agent)
			return envelope.Reply{}, true, err
		}
	}
	s.record(ctx, "held", shadow.ID, shadow.TraceID, agent, store.DetailJSON(map[string]any{"op": action.Op, "parent_id": action.ParentID, "to": shadow.To}))
	s.notifyApproval(ctx, shadow)
	return envelope.Reply{Status: envelope.StatusHeld, RequestID: shadow.ID, From: agent, CreatedAt: shadow.CreatedAt}, true, nil
}

func (s *Server) replyRefusal(ctx context.Context, parent envelope.Request, status envelope.Status, agent string, rep envelope.Reply, email bool, round int) error {
	if parent.To != agent {
		return store.ErrForbidden
	}
	if email && (!rep.Status.Terminal() || len(rep.Attachments) > 0) {
		return store.ErrWrongState
	}
	if rep.Status == envelope.StatusNeedsInput {
		if len(rep.Attachments) != 0 || status != envelope.StatusClaimed || parent.Kind != envelope.KindAsk || len(parent.Exchanges) >= envelope.MaxExchanges {
			return store.ErrWrongState
		}
		return s.liveClaim(ctx, parent.ID)
	}
	if !rep.Status.Terminal() {
		return store.ErrWrongState
	}
	switch status {
	case envelope.StatusQueued, envelope.StatusDelivered, envelope.StatusClaimed:
	default:
		return store.ErrWrongState
	}
	if !email {
		return nil
	}
	live, err := s.store.ClaimLive(ctx, parent.ID)
	if err != nil {
		return err
	}
	if live {
		return store.ErrLiveClaim
	}
	if parent.WasHeld && !parent.Approved {
		return store.ErrWrongState
	}
	if parent.AnsweredExchanges() != round {
		return store.ErrStaleRound
	}
	return nil
}

func (s *Server) liveClaim(ctx context.Context, id string) error {
	live, err := s.store.ClaimLive(ctx, id)
	if err != nil {
		return err
	}
	if !live {
		return store.ErrWrongState
	}
	open, err := s.store.Unexpired(ctx, id)
	if err != nil {
		return err
	}
	if !open {
		return store.ErrWrongState
	}
	return nil
}

func (s *Server) answerRefusal(ctx context.Context, parent envelope.Request, status envelope.Status, agent string) error {
	if parent.From != agent {
		return store.ErrForbidden
	}
	if status != envelope.StatusNeedsInput || len(parent.Exchanges) == 0 {
		return store.ErrWrongState
	}
	open, err := s.store.Unexpired(ctx, parent.ID)
	if err != nil {
		return err
	}
	if !open {
		return store.ErrWrongState
	}
	return nil
}

func (s *Server) approveDelivery(w http.ResponseWriter, r *http.Request, id, action string) {
	var act deliveryAction
	if err := json.Unmarshal([]byte(action), &act); err != nil || act.ParentID == "" {
		writeErr(w, http.StatusConflict, store.ErrWrongState)
		return
	}
	// Unbind, the parent write, and marking the shadow applied commit together.
	// A failure in the parent write leaves the parent unchanged and the shadow held.
	applied, err := s.store.ApplyHeldDelivery(r.Context(), id)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	switch applied.Op {
	case "reply":
		s.announceReply(r.Context(), applied.Shadow.ParentID, applied.Shadow.From, applied.Reply, false)
	case "answer":
		s.publishAnswer(r.Context(), applied.Parent, applied.Shadow.From, applied.Shadow.Body)
	default:
		writeErr(w, http.StatusConflict, store.ErrWrongState)
		return
	}
	s.record(r.Context(), "approved", id, applied.Shadow.TraceID, s.remote(r), store.DetailJSON(map[string]any{"op": applied.Op, "parent_id": applied.Shadow.ParentID}))
	applied.Shadow.Status = envelope.StatusAnswered
	applied.Shadow.Body = ""
	applied.Shadow.Approved = true
	writeJSON(w, http.StatusOK, applied.Shadow)
}

func (s *Server) publishAnswer(ctx context.Context, req envelope.Request, actor, body string) {
	s.record(ctx, "answered_input", req.ID, req.TraceID, actor, store.DetailJSON(map[string]any{"answer_bytes": len(body)}))
	s.hub.notify(requestKey(req.ID))
	s.hub.notify(inboxKey(req.To))
	if s.events != nil {
		s.events.Queued(ctx, req)
	}
}
