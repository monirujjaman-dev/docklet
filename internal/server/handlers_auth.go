package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/audit"
	"github.com/monirujjaman-dev/docklet/internal/auth"
	"github.com/monirujjaman-dev/docklet/internal/store/db"
)

const (
	maxFailedLogins = 5
	lockoutDuration = 15 * time.Minute
)

var usernameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{2,31}$`)

type userDTO struct {
	ID          int64  `json:"id"`
	Username    string `json:"username"`
	TOTPEnabled bool   `json:"totpEnabled"`
}

type meDTO struct {
	User      userDTO `json:"user"`
	CSRFToken string  `json:"csrfToken"`
}

func toMe(u db.User, sess db.Session) meDTO {
	return meDTO{
		User:      userDTO{ID: u.ID, Username: u.Username, TOTPEnabled: u.TotpEnabled == 1},
		CSRFToken: sess.CsrfToken,
	}
}

func ptr[T any](v T) *T { return &v }

// GET /api/setup/status
func (s *Server) handleSetupStatus(w http.ResponseWriter, r *http.Request) error {
	n, err := s.st.Q.CountUsers(r.Context())
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]bool{"needsSetup": n == 0})
	return nil
}

type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
	TOTP     string `json:"totp,omitempty"`
}

// POST /api/setup — creates the first admin. Only works while no user exists.
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) error {
	if !s.loginLimiter.allow(clientIP(r)) {
		return errf(http.StatusTooManyRequests, "too many requests, slow down")
	}
	// Check before validating input so a completed install always answers 409.
	if n, err := s.st.Q.CountUsers(r.Context()); err != nil {
		return err
	} else if n > 0 {
		return errf(http.StatusConflict, "setup has already been completed")
	}
	var in credentials
	if err := readJSON(r, &in); err != nil {
		return err
	}
	in.Username = strings.TrimSpace(in.Username)
	if !usernameRe.MatchString(in.Username) {
		return errf(http.StatusBadRequest, "username must be 3-32 characters: letters, digits, '.', '_' or '-'")
	}
	if err := auth.ValidatePassword(in.Password); err != nil {
		return errf(http.StatusBadRequest, "%s", err.Error())
	}
	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		return err
	}

	ctx := r.Context()
	var me meDTO
	err = s.st.Tx(ctx, func(q *db.Queries) error {
		n, err := q.CountUsers(ctx)
		if err != nil {
			return err
		}
		if n > 0 {
			return errf(http.StatusConflict, "setup has already been completed")
		}
		now := s.now()
		u, err := q.CreateUser(ctx, db.CreateUserParams{
			Username: in.Username, PasswordHash: hash, CreatedAt: now.Unix(), UpdatedAt: now.Unix(),
		})
		if err != nil {
			return err
		}
		if _, err := audit.Append(ctx, q, audit.Entry{
			ActorID: ptr(u.ID), Actor: u.Username, Action: audit.ActionSetupCompleted,
			Target: "user:" + u.Username, IP: clientIP(r),
		}, now); err != nil {
			return err
		}
		sess, err := s.newSession(ctx, q, w, r, u.ID)
		if err != nil {
			return err
		}
		me = toMe(u, sess)
		return nil
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, me)
	return nil
}

// POST /api/auth/login
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) error {
	ip := clientIP(r)
	if !s.loginLimiter.allow(ip) {
		return errf(http.StatusTooManyRequests, "too many login attempts, try again in a minute")
	}
	var in credentials
	if err := readJSON(r, &in); err != nil {
		return err
	}
	in.Username = strings.TrimSpace(in.Username)
	ctx := r.Context()
	invalid := errf(http.StatusUnauthorized, "invalid username or password")

	u, err := s.st.Q.GetUserByUsername(ctx, in.Username)
	if errors.Is(err, sql.ErrNoRows) {
		auth.BurnVerify(in.Password)
		if err := s.auditFailedLogin(ctx, nil, in.Username, ip, "unknown_user"); err != nil {
			return err
		}
		return invalid
	}
	if err != nil {
		return err
	}

	now := s.now()
	if u.LockedUntil > now.Unix() {
		return lockedErr(time.Unix(u.LockedUntil, 0).Sub(now))
	}

	ok, err := auth.VerifyPassword(in.Password, u.PasswordHash)
	if err != nil {
		return err
	}
	if !ok {
		return s.failLogin(ctx, u, ip, "bad_password", invalid)
	}

	var totpStep int64
	if u.TotpEnabled == 1 {
		if strings.TrimSpace(in.TOTP) == "" {
			return &apiError{Status: http.StatusUnauthorized, Msg: "two-factor code required", Extra: map[string]any{"totpRequired": true}}
		}
		secret, err := s.box.Open(u.TotpSecretEnc, totpAAD(u.ID))
		if err != nil {
			return fmt.Errorf("decrypt totp secret: %w", err)
		}
		step, valid := auth.VerifyTOTP(string(secret), in.TOTP, now, u.TotpLastStep)
		if !valid {
			return s.failLogin(ctx, u, ip, "bad_totp",
				&apiError{Status: http.StatusUnauthorized, Msg: "invalid two-factor code", Extra: map[string]any{"totpRequired": true}})
		}
		totpStep = step
	}

	var me meDTO
	err = s.st.Tx(ctx, func(q *db.Queries) error {
		if err := q.ResetFailedLogins(ctx, db.ResetFailedLoginsParams{UpdatedAt: now.Unix(), ID: u.ID}); err != nil {
			return err
		}
		if totpStep > 0 {
			if err := q.SetTOTPLastStep(ctx, db.SetTOTPLastStepParams{TotpLastStep: totpStep, UpdatedAt: now.Unix(), ID: u.ID}); err != nil {
				return err
			}
		}
		sess, err := s.newSession(ctx, q, w, r, u.ID)
		if err != nil {
			return err
		}
		if _, err := audit.Append(ctx, q, audit.Entry{
			ActorID: ptr(u.ID), Actor: u.Username, Action: audit.ActionLoginSucceeded, IP: ip,
			Details: map[string]any{"totp": totpStep > 0},
		}, now); err != nil {
			return err
		}
		me = toMe(u, sess)
		return nil
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, me)
	return nil
}

func lockedErr(remaining time.Duration) error {
	mins := int(remaining.Minutes()) + 1
	return &apiError{
		Status: http.StatusLocked,
		Msg:    fmt.Sprintf("too many failed attempts; account locked for %d more minute(s)", mins),
		Extra:  map[string]any{"locked": true},
	}
}

// failLogin counts a failed attempt, locks the account at the threshold and
// audits both events.
func (s *Server) failLogin(ctx context.Context, u db.User, ip, reason string, resp error) error {
	now := s.now()
	lockUntil := now.Add(lockoutDuration)
	locked := false
	err := s.st.Tx(ctx, func(q *db.Queries) error {
		count, err := q.RecordFailedLogin(ctx, db.RecordFailedLoginParams{
			MaxAttempts: maxFailedLogins, LockUntil: lockUntil.Unix(), Now: now.Unix(), ID: u.ID,
		})
		if err != nil {
			return err
		}
		locked = count >= maxFailedLogins
		if _, err := audit.Append(ctx, q, audit.Entry{
			ActorID: ptr(u.ID), Actor: u.Username, Action: audit.ActionLoginFailed, IP: ip,
			Details: map[string]any{"reason": reason},
		}, now); err != nil {
			return err
		}
		if locked {
			_, err := audit.Append(ctx, q, audit.Entry{
				ActorID: ptr(u.ID), Actor: u.Username, Action: audit.ActionAccountLocked, IP: ip,
				Details: map[string]any{"until": lockUntil.UTC().Format(time.RFC3339)},
			}, now)
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	if locked {
		return lockedErr(lockoutDuration)
	}
	return resp
}

func (s *Server) auditFailedLogin(ctx context.Context, actorID *int64, username, ip, reason string) error {
	if len(username) > 64 {
		username = username[:64]
	}
	return s.st.Tx(ctx, func(q *db.Queries) error {
		_, err := audit.Append(ctx, q, audit.Entry{
			ActorID: actorID, Actor: "anonymous", Action: audit.ActionLoginFailed,
			Target: "user:" + username, IP: ip, Details: map[string]any{"reason": reason},
		}, s.now())
		return err
	})
}

// POST /api/auth/logout
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) error {
	a := authFrom(r.Context())
	ctx := r.Context()
	err := s.st.Tx(ctx, func(q *db.Queries) error {
		if err := q.DeleteSession(ctx, a.Session.ID); err != nil {
			return err
		}
		_, err := audit.Append(ctx, q, audit.Entry{
			ActorID: ptr(a.User.ID), Actor: a.User.Username, Action: audit.ActionLogout, IP: clientIP(r),
		}, s.now())
		return err
	})
	if err != nil {
		return err
	}
	clearSessionCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	return nil
}

// GET /api/auth/me
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) error {
	a := authFrom(r.Context())
	writeJSON(w, http.StatusOK, toMe(a.User, a.Session))
	return nil
}

func totpAAD(userID int64) []byte {
	return []byte(fmt.Sprintf("user:%d:totp", userID))
}
