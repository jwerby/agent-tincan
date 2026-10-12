package relay

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

func (s *Server) handleAnswer(w http.ResponseWriter, r *http.Request) {
	name := s.agent(w, r)
	if name == "" {
		return
	}
	var in struct {
		Body string `json:"body"`
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, err)
		return
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := envelope.ValidateInput(in.Body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if held, stop, err := s.holdAnswer(r.Context(), r.PathValue("id"), name, in.Body); stop || err != nil {
		if err != nil {
			writeErr(w, statusFor(err), err)
			return
		}
		writeJSON(w, http.StatusOK, held)
		return
	}
	req, err := s.store.Answer(r.Context(), r.PathValue("id"), name, in.Body)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	s.publishAnswer(r.Context(), req, name, in.Body)
	writeJSON(w, http.StatusOK, req)
}
