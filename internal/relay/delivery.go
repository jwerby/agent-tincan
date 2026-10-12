package relay

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

type deliveryAction struct {
	Op       string          `json:"op"`
	Status   envelope.Status `json:"status,omitempty"`
	ParentID string          `json:"parent_id"`
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
	msg := &envelope.Request{From: agent, To: parent.From, Kind: parent.Kind, Body: rep.Body, Chain: []string{agent}}
	held, err := gater.HoldDelivery(ctx, msg)
	if err != nil || !held {
		return envelope.Reply{}, err != nil, err
	}
	shadow := envelope.Request{
		From: agent, To: parent.From, ParentID: parent.ID, TraceID: parent.TraceID,
		Hop: parent.Hop + 1, Chain: []string{agent}, Kind: parent.Kind, Body: rep.Body,
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
	msg := &envelope.Request{From: agent, To: parent.To, Kind: parent.Kind, Body: body, Chain: []string{agent}}
	held, err := gater.HoldDelivery(ctx, msg)
	if err != nil || !held {
		return envelope.Request{}, err != nil, err
	}
	shadow := envelope.Request{
		From: agent, To: parent.To, ParentID: parent.ID, TraceID: parent.TraceID,
		Hop: parent.Hop + 1, Chain: []string{agent}, Kind: parent.Kind, Body: body,
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
	raw, err := json.Marshal(action)
	if err != nil {
		return envelope.Reply{}, true, err
	}
	shadow, err = s.store.EnqueueDeliveryHold(ctx, shadow, string(raw), action.Op)
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
	shadow, status, err := s.store.Request(r.Context(), id)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	if status != envelope.StatusHeld {
		writeErr(w, http.StatusConflict, store.ErrWrongState)
		return
	}
	if err := s.store.UnbindAttachments(r.Context(), id); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	switch act.Op {
	case "reply":
		rep := envelope.Reply{Body: shadow.Body, Status: act.Status, Attachments: shadow.Attachments}
		if _, err := s.recordReply(r.Context(), shadow.ParentID, shadow.From, rep, "", 0, true); err != nil {
			writeErr(w, statusFor(err), err)
			return
		}
	case "answer":
		req, err := s.store.Answer(r.Context(), shadow.ParentID, shadow.From, shadow.Body)
		if err != nil {
			writeErr(w, statusFor(err), err)
			return
		}
		s.publishAnswer(r.Context(), req, shadow.From, shadow.Body)
	default:
		writeErr(w, http.StatusConflict, store.ErrWrongState)
		return
	}
	if err := s.store.FinishHeldDelivery(r.Context(), id); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	s.record(r.Context(), "approved", id, shadow.TraceID, s.remote(r), store.DetailJSON(map[string]any{"op": act.Op, "parent_id": shadow.ParentID}))
	shadow.Status = envelope.StatusAnswered
	shadow.Body = ""
	shadow.Approved = true
	writeJSON(w, http.StatusOK, shadow)
}

func (s *Server) publishAnswer(ctx context.Context, req envelope.Request, actor, body string) {
	s.record(ctx, "answered_input", req.ID, req.TraceID, actor, store.DetailJSON(map[string]any{"answer_bytes": len(body)}))
	s.hub.notify(requestKey(req.ID))
	s.hub.notify(inboxKey(req.To))
	if s.events != nil {
		s.events.Queued(ctx, req)
	}
}
