package account

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// Handler is the sign-in surface. It is bearer-only: the JSON that verify
// returns carries the session token, and the client sends it back as
// `Authorization: Bearer <token>` on everything that needs a person. A cookie
// would decide CSRF and Secure for a page that does not exist yet; the page
// that sets one can set it when it arrives.
//
//	POST /auth/request  {email}  → 202 {}         (the same answer for every address)
//	POST /auth/verify   {token}  → 200 {session, expires, user}
//	GET  /auth/me                → 200 {id, email, wallet, created, balance, waiting, paid}
//	POST /auth/signout           → 204
//	POST /waitlist      {reason} → 201 {}         reasons: arena, model:<id>
func (s *Store) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /auth/request", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Email string `json:"email"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		if err := s.Request(r.Context(), in.Email); err != nil {
			if errors.Is(err, ErrBadEmail) {
				httpError(w, http.StatusBadRequest, err.Error())
				return
			}
			s.cfg.Log.Error("sign-in request failed", "err", err)
			httpError(w, http.StatusInternalServerError, "could not send a link")
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{})
	})
	mux.HandleFunc("POST /auth/verify", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Token string `json:"token"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		sess, err := s.Verify(r.Context(), in.Token)
		if err != nil {
			if errors.Is(err, ErrBadToken) {
				httpError(w, http.StatusUnauthorized, err.Error())
				return
			}
			s.cfg.Log.Error("sign-in failed", "err", err)
			httpError(w, http.StatusInternalServerError, "could not sign in")
			return
		}
		writeJSON(w, http.StatusOK, sess)
	})
	mux.HandleFunc("GET /auth/me", func(w http.ResponseWriter, r *http.Request) {
		u, ok := s.require(w, r)
		if !ok {
			return
		}
		bal, err := s.cfg.Ledger.Balance(r.Context(), u.Wallet)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "could not read the wallet")
			return
		}
		waiting, err := s.Waiting(r.Context(), u.ID)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "could not read the waitlist")
			return
		}
		paid, err := s.Paid(r.Context(), u.ID)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "could not read the topups")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": u.ID, "email": u.Email, "wallet": u.Wallet, "created": u.Created,
			"balance": bal, "waiting": waiting, "paid": paid,
		})
	})
	mux.HandleFunc("POST /auth/signout", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.require(w, r); !ok {
			return
		}
		if err := s.SignOut(r.Context(), BearerToken(r)); err != nil {
			httpError(w, http.StatusInternalServerError, "could not sign out")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /waitlist", func(w http.ResponseWriter, r *http.Request) {
		u, ok := s.require(w, r)
		if !ok {
			return
		}
		var in struct {
			Reason string `json:"reason"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		if err := s.Waitlist(r.Context(), u.ID, in.Reason); err != nil {
			if errors.Is(err, ErrBadReason) {
				httpError(w, http.StatusBadRequest, err.Error())
				return
			}
			httpError(w, http.StatusInternalServerError, "could not join the waitlist")
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{})
	})
	return mux
}

// FromRequest is the request-side of Authenticate: the user behind the
// bearer token, or ErrNoSession. Other surfaces mount it in front of
// themselves.
func (s *Store) FromRequest(r *http.Request) (User, error) {
	return s.Authenticate(r.Context(), BearerToken(r))
}

// BearerToken reads the Authorization header, or returns "".
func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func (s *Store) require(w http.ResponseWriter, r *http.Request) (User, bool) {
	u, err := s.FromRequest(r)
	if errors.Is(err, ErrNoSession) {
		httpError(w, http.StatusUnauthorized, err.Error())
		return User{}, false
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, "could not read the session")
		return User{}, false
	}
	return u, true
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(v); err != nil {
		httpError(w, http.StatusBadRequest, "body must be JSON")
		return false
	}
	return true
}

func httpError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
