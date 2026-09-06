package service

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"

	"github.com/metahumansworld/aiphylum/internal/spec"
)

// Authenticator turns a request into the owner behind it. It returns
// ErrUnauthenticated (wrapped or bare) for no session; any other error is
// the authenticator's own failure and answers as one.
type Authenticator func(*http.Request) (Owner, error)

// Control is the builder's surface: a signed-in person creates agents and
// sees their own. Every route but the catalogue needs a session, and an
// agent that is not the caller's is not there, which is a 404 and not a 403:
// the surface does not confirm what it does not show.
//
//	GET  /v1/models                              → 200 {offered: [...], locked: [...]}
//	POST /v1/agents        body: a spec          → 201 {id, owner, spec, wallet, created}
//	GET  /v1/agents                              → 200 {agents: [...]}   the caller's
//	GET  /v1/agents/{id}                         → 200 {id, owner, spec, wallet, created}
//	                                             → 401 without a session
func (s *Service) Control(auth Authenticator) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		offered, locked := s.Models()
		writeJSON(w, http.StatusOK, map[string]any{"offered": offered, "locked": locked})
	})
	mux.HandleFunc("POST /v1/agents", func(w http.ResponseWriter, r *http.Request) {
		owner, ok := s.owner(w, r, auth)
		if !ok {
			return
		}
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
		ag, err := s.Create(r.Context(), owner, a)
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
		owner, ok := s.owner(w, r, auth)
		if !ok {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"agents": s.AgentsOf(owner.ID)})
	})
	mux.HandleFunc("GET /v1/agents/{id}", func(w http.ResponseWriter, r *http.Request) {
		owner, ok := s.owner(w, r, auth)
		if !ok {
			return
		}
		ag, ok := s.Get(r.PathValue("id"))
		if !ok || ag.Owner != owner.ID {
			httpError(w, http.StatusNotFound, ErrNoAgent.Error())
			return
		}
		writeJSON(w, http.StatusOK, ag)
	})
	return mux
}

func (s *Service) owner(w http.ResponseWriter, r *http.Request, auth Authenticator) (Owner, bool) {
	owner, err := auth(r)
	switch {
	case errors.Is(err, ErrUnauthenticated):
		httpError(w, http.StatusUnauthorized, ErrUnauthenticated.Error())
		return Owner{}, false
	case err != nil:
		s.log.Error("authenticator failed", "err", err)
		httpError(w, http.StatusInternalServerError, "could not read the session")
		return Owner{}, false
	}
	return owner, true
}

// Public is the surface each agent is reachable on. It is meant for people
// who did not build the agent, so it says only what the agent would say to
// them: a name, a greeting, and replies. The card costs nothing; a message
// costs whatever the model call costs the owner's wallet, never the caller.
//
//	GET  /a/{id}                                  → 200 {id, name, greeting}
//	POST /a/{id}/messages  {conversation?, text}  → 200 {conversation, reply}
//	                                              → 402 when the owner's grant is spent
//	                                              → 429 + Retry-After when the agent is being flooded
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
			var limited *RateLimitError
			if errors.As(err, &limited) {
				w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(limited.RetryAfter.Seconds()))))
			}
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
	case errors.Is(err, ErrRateLimited):
		return http.StatusTooManyRequests
	case errors.Is(err, ErrProviderDown):
		return http.StatusBadGateway
	case errors.Is(err, ErrModelNotOffered):
		return http.StatusForbidden
	case errors.Is(err, ErrUnauthenticated):
		return http.StatusUnauthorized
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
