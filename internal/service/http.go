package service

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/metahumansworld/aiphylum/internal/spec"
)

// Control is the operator's surface: create and list agents. It carries no
// authentication of its own; like the daemon's control plane it is bound to
// loopback by whoever mounts it, and accounts will sit in front of it later.
//
//	POST /v1/agents        body: a spec          → 201 {id, spec, wallet, created}
//	GET  /v1/agents                              → 200 {agents: [...]}
//	GET  /v1/agents/{id}                         → 200 {id, spec, wallet, created}
func (s *Service) Control() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/agents", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		if err != nil {
			httpError(w, http.StatusBadRequest, "unreadable body")
			return
		}
		a, err := spec.Parse(body)
		if err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
		ag, err := s.Create(r.Context(), a)
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, ErrModelNotOffered) || errors.Is(err, spec.ErrInvalid) {
				status = http.StatusBadRequest
			}
			httpError(w, status, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, ag)
	})
	mux.HandleFunc("GET /v1/agents", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"agents": s.Agents()})
	})
	mux.HandleFunc("GET /v1/agents/{id}", func(w http.ResponseWriter, r *http.Request) {
		ag, ok := s.Get(r.PathValue("id"))
		if !ok {
			httpError(w, http.StatusNotFound, ErrNoAgent.Error())
			return
		}
		writeJSON(w, http.StatusOK, ag)
	})
	return mux
}

// Public is the surface each agent is reachable on. It is meant for people
// who did not build the agent, so it says only what the agent would say to
// them: a name, a greeting, and replies. The card costs nothing; a message
// costs whatever the model call costs the agent's wallet, never the caller.
//
//	GET  /a/{id}                                  → 200 {id, name, greeting}
//	POST /a/{id}/messages  {conversation?, text}  → 200 {conversation, reply}
//	                                              → 402 when the agent's credits are spent
//	                                              → 502 when the model is unreachable (nothing charged)
func (s *Service) Public() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /a/{id}", func(w http.ResponseWriter, r *http.Request) {
		ag, ok := s.Get(r.PathValue("id"))
		if !ok {
			httpError(w, http.StatusNotFound, ErrNoAgent.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": ag.ID, "name": ag.Spec.Name, "greeting": ag.Spec.Greeting,
		})
	})
	mux.HandleFunc("POST /a/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Conversation string `json:"conversation"`
			Text         string `json:"text"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
			httpError(w, http.StatusBadRequest, "body must be JSON with text and an optional conversation")
			return
		}
		turn, err := s.Say(r.Context(), r.PathValue("id"), in.Conversation, in.Text)
		if err != nil {
			httpError(w, statusFor(err), err.Error())
			return
		}
		// The reply and the thread, and not what it cost: a stranger who
		// messages an agent must not learn how much grant it has left.
		writeJSON(w, http.StatusOK, map[string]any{
			"conversation": turn.Conversation, "reply": turn.Reply,
		})
	})
	return mux
}

// statusFor maps the service's refusals onto the codes the SDK already
// teaches: 402 is out of credits, 502 is the provider, 4xx is the caller.
func statusFor(err error) int {
	switch {
	case errors.Is(err, ErrNoAgent), errors.Is(err, ErrNoConversation):
		return http.StatusNotFound
	case errors.Is(err, ErrEmptyMessage), errors.Is(err, ErrMessageTooLong):
		return http.StatusBadRequest
	case errors.Is(err, ErrOutOfCredits):
		return http.StatusPaymentRequired
	case errors.Is(err, ErrProviderDown):
		return http.StatusBadGateway
	case errors.Is(err, ErrModelNotOffered):
		return http.StatusForbidden
	}
	return http.StatusInternalServerError
}

func httpError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
