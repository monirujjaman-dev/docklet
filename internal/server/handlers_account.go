package server

import (
	"fmt"
	"net/http"

	"github.com/monirujjaman-dev/docklet/internal/audit"
	"github.com/monirujjaman-dev/docklet/internal/auth"
	"github.com/monirujjaman-dev/docklet/internal/store/db"
)

// checkPassword verifies the current user's password for sensitive changes.
func checkPassword(u db.User, pw string) error {
	ok, err := auth.VerifyPassword(pw, u.PasswordHash)
	if err != nil {
		return err
	}
	if !ok {
		return errf(http.StatusForbidden, "current password is incorrect")
	}
	return nil
}

// POST /api/account/password
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	a := authFrom(r.Context())
	if err := checkPassword(a.User, in.CurrentPassword); err != nil {
		return err
	}
	if err := auth.ValidatePassword(in.NewPassword); err != nil {
		return errf(http.StatusBadRequest, "%s", err.Error())
	}
	hash, err := auth.HashPassword(in.NewPassword)
	if err != nil {
		return err
	}
	ctx := r.Context()
	now := s.now()
	err = s.st.Tx(ctx, func(q *db.Queries) error {
		if err := q.UpdatePassword(ctx, db.UpdatePasswordParams{PasswordHash: hash, UpdatedAt: now.Unix(), ID: a.User.ID}); err != nil {
			return err
		}
		// Sign out every other session; keep the current one.
		if err := q.DeleteUserSessionsExcept(ctx, db.DeleteUserSessionsExceptParams{UserID: a.User.ID, ID: a.Session.ID}); err != nil {
			return err
		}
		_, err := audit.Append(ctx, q, audit.Entry{
			ActorID: ptr(a.User.ID), Actor: a.User.Username, Action: audit.ActionPasswordChanged, IP: clientIP(r),
		}, now)
		return err
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	return nil
}

// POST /api/account/totp/setup — generates a pending secret.
func (s *Server) handleTOTPSetup(w http.ResponseWriter, r *http.Request) error {
	a := authFrom(r.Context())
	if a.User.TotpEnabled == 1 {
		return errf(http.StatusConflict, "two-factor authentication is already enabled")
	}
	secret, err := auth.NewTOTPSecret()
	if err != nil {
		return err
	}
	enc, err := s.box.Seal([]byte(secret), totpAAD(a.User.ID))
	if err != nil {
		return err
	}
	if err := s.st.Q.SetPendingTOTP(r.Context(), db.SetPendingTOTPParams{
		TotpSecretEnc: enc, UpdatedAt: s.now().Unix(), ID: a.User.ID,
	}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"secret": secret,
		"uri":    auth.TOTPURI("Docklet", a.User.Username, secret),
	})
	return nil
}

// POST /api/account/totp/enable — confirms the pending secret with a code.
func (s *Server) handleTOTPEnable(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Code string `json:"code"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	a := authFrom(r.Context())
	if a.User.TotpEnabled == 1 {
		return errf(http.StatusConflict, "two-factor authentication is already enabled")
	}
	if len(a.User.TotpSecretEnc) == 0 {
		return errf(http.StatusBadRequest, "start two-factor setup first")
	}
	secret, err := s.box.Open(a.User.TotpSecretEnc, totpAAD(a.User.ID))
	if err != nil {
		return fmt.Errorf("decrypt totp secret: %w", err)
	}
	now := s.now()
	step, ok := auth.VerifyTOTP(string(secret), in.Code, now, 0)
	if !ok {
		return errf(http.StatusBadRequest, "invalid code; check your authenticator's clock")
	}
	ctx := r.Context()
	err = s.st.Tx(ctx, func(q *db.Queries) error {
		if err := q.EnableTOTP(ctx, db.EnableTOTPParams{TotpLastStep: step, UpdatedAt: now.Unix(), ID: a.User.ID}); err != nil {
			return err
		}
		_, err := audit.Append(ctx, q, audit.Entry{
			ActorID: ptr(a.User.ID), Actor: a.User.Username, Action: audit.ActionTOTPEnabled, IP: clientIP(r),
		}, now)
		return err
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	return nil
}

// POST /api/account/totp/disable — requires the current password.
func (s *Server) handleTOTPDisable(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Password string `json:"password"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	a := authFrom(r.Context())
	if err := checkPassword(a.User, in.Password); err != nil {
		return err
	}
	ctx := r.Context()
	now := s.now()
	err := s.st.Tx(ctx, func(q *db.Queries) error {
		if err := q.DisableTOTP(ctx, db.DisableTOTPParams{UpdatedAt: now.Unix(), ID: a.User.ID}); err != nil {
			return err
		}
		_, err := audit.Append(ctx, q, audit.Entry{
			ActorID: ptr(a.User.ID), Actor: a.User.Username, Action: audit.ActionTOTPDisabled, IP: clientIP(r),
		}, now)
		return err
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	return nil
}
