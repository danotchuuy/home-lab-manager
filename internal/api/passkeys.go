package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/danotchuuy/home-lab-manager/internal/auth"
	"github.com/danotchuuy/home-lab-manager/internal/webauthn"
)

// --- Passkeys ---

func decodePasskey[T any](w http.ResponseWriter, r *http.Request) (T, bool) {
	var in T
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return in, false
	}
	return in, true
}

// writePasskeyError answers a failed passkey step: 400 for passkeys that
// can't be used here (plain HTTP, an IP address).
func writePasskeyError(w http.ResponseWriter, err error) {
	if errors.Is(err, webauthn.ErrInsecureOrigin) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "passkeys_unavailable"})
		return
	}
	if errors.Is(err, auth.ErrPasskeyExpired) {
		writeError(w, http.StatusUnauthorized, err)
		return
	}
	writeLoginError(w, err)
}

// Signing in with a passkey alone.

func (s *server) beginPasskeyLogin(w http.ResponseWriter, r *http.Request) {
	if !requireCSRFHeader(w, r) {
		return
	}
	opts, err := s.Auth.BeginPasskeyLogin(w, r)
	if err != nil {
		writePasskeyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, opts)
}

func (s *server) passkeyLogin(w http.ResponseWriter, r *http.Request) {
	if !requireCSRFHeader(w, r) {
		return
	}
	in, ok := decodePasskey[auth.UsedPasskey](w, r)
	if !ok {
		return
	}
	u, err := s.Auth.PasskeyLogin(w, r, in)
	if err != nil {
		writePasskeyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

// A passkey as the second factor, after the password.

func (s *server) beginPasskeySecond(w http.ResponseWriter, r *http.Request) {
	if !requireCSRFHeader(w, r) {
		return
	}
	opts, err := s.Auth.BeginPasskeySecond(w, r)
	if err != nil {
		writePasskeyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, opts)
}

func (s *server) passkeySecond(w http.ResponseWriter, r *http.Request) {
	if !requireCSRFHeader(w, r) {
		return
	}
	in, ok := decodePasskey[auth.UsedPasskey](w, r)
	if !ok {
		return
	}
	u, err := s.Auth.PasskeySecond(w, r, in)
	if err != nil {
		writePasskeyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

// Adding a passkey during sign-in, when two-factor is required.

func (s *server) beginPasskeySetup(w http.ResponseWriter, r *http.Request) {
	if !requireCSRFHeader(w, r) {
		return
	}
	opts, err := s.Auth.BeginPasskeySetup(w, r)
	if err != nil {
		writePasskeyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, opts)
}

func (s *server) passkeySetup(w http.ResponseWriter, r *http.Request) {
	if !requireCSRFHeader(w, r) {
		return
	}
	in, ok := decodePasskey[auth.NewPasskey](w, r)
	if !ok {
		return
	}
	u, err := s.Auth.FinishPasskeySetup(w, r, in)
	if err != nil {
		writePasskeyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

// Confirming it's you with a passkey.

func (s *server) beginPasskeyConfirm(w http.ResponseWriter, r *http.Request) {
	opts, err := s.Auth.BeginPasskeyConfirm(w, r)
	if err != nil {
		writePasskeyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, opts)
}

func (s *server) passkeyConfirm(w http.ResponseWriter, r *http.Request) {
	in, ok := decodePasskey[auth.UsedPasskey](w, r)
	if !ok {
		return
	}
	u, err := s.Auth.ConfirmPasskey(w, r, in)
	if err != nil {
		writePasskeyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"confirmedUntil": u.Confirmed.Add(auth.ConfirmWindow)})
}

// An account's passkeys.

// mayManagePasskeys reports whether the caller can see and remove an
// account's passkeys: its owner at a browser, or an admin.
func (s *server) mayManagePasskeys(r *http.Request) bool {
	return s.ownAccount(r) || s.access(r).Admin()
}

func (s *server) listPasskeys(w http.ResponseWriter, r *http.Request) {
	if !s.mayManagePasskeys(r) {
		writeError(w, http.StatusForbidden, errNotAllowed)
		return
	}
	list, err := s.Auth.Passkeys(r.PathValue("username"))
	if err != nil {
		writeAccountError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *server) beginAddPasskey(w http.ResponseWriter, r *http.Request) {
	if !s.ownAccount(r) {
		writeError(w, http.StatusForbidden, errors.New("passkeys are added by the account's owner, signed in at a browser"))
		return
	}
	if !s.stepUp(w, r) {
		return
	}
	opts, err := s.Auth.BeginPasskey(w, r, r.PathValue("username"))
	if err != nil {
		writePasskeyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, opts)
}

func (s *server) addPasskey(w http.ResponseWriter, r *http.Request) {
	if !s.ownAccount(r) {
		writeError(w, http.StatusForbidden, errNotAllowed)
		return
	}
	in, ok := decodePasskey[auth.NewPasskey](w, r)
	if !ok {
		return
	}
	p, err := s.Auth.FinishPasskey(w, r, r.PathValue("username"), in)
	if err != nil {
		writePasskeyError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (s *server) renamePasskey(w http.ResponseWriter, r *http.Request) {
	if !s.ownAccount(r) {
		writeError(w, http.StatusForbidden, errNotAllowed)
		return
	}
	in, ok := decodePasskey[struct {
		Name string `json:"name"`
	}](w, r)
	if !ok {
		return
	}
	if err := s.Auth.RenamePasskey(r.PathValue("username"), r.PathValue("id"), in.Name); err != nil {
		writeAccountError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// removePasskey removes a passkey: the owner's own (keeping a required
// second factor), or an admin's removal.
func (s *server) removePasskey(w http.ResponseWriter, r *http.Request) {
	own := s.ownAccount(r)
	if !own && !s.access(r).Admin() {
		writeError(w, http.StatusForbidden, errNotAllowed)
		return
	}
	if !s.stepUp(w, r) {
		return
	}
	if err := s.Auth.RemovePasskey(r.PathValue("username"), r.PathValue("id"), !own); err != nil {
		writeAccountError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeAccountError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auth.ErrNoAccount), errors.Is(err, auth.ErrNoPasskey):
		writeError(w, http.StatusNotFound, err)
	default:
		writeError(w, http.StatusBadRequest, err)
	}
}
