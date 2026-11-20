package server

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/auth"
	"github.com/monirujjaman-dev/docklet/internal/store/db"
)

const (
	sessionCookie      = "docklet_session"
	sessionMaxAge      = 7 * 24 * time.Hour
	sessionIdleTimeout = 24 * time.Hour
	sessionTouchEvery  = time.Minute
	csrfHeader         = "X-CSRF-Token"
)

type ctxKey int

const authKey ctxKey = 1

type authInfo struct {
	User    db.User
	Session db.Session
}

func authFrom(ctx context.Context) *authInfo {
	a, _ := ctx.Value(authKey).(*authInfo)
	return a
}

// newSession creates a session row with q and sets the cookie.
func (s *Server) newSession(ctx context.Context, q *db.Queries, w http.ResponseWriter, r *http.Request, userID int64) (db.Session, error) {
	token, err := auth.NewToken()
	if err != nil {
		return db.Session{}, err
	}
	csrf, err := auth.NewToken()
	if err != nil {
		return db.Session{}, err
	}
	now := s.now()
	ua := r.UserAgent()
	if len(ua) > 256 {
		ua = ua[:256]
	}
	sess := db.Session{
		ID: auth.HashToken(token), UserID: userID, CsrfToken: csrf,
		Ip: clientIP(r), UserAgent: ua,
		CreatedAt: now.Unix(), LastSeenAt: now.Unix(), ExpiresAt: now.Add(sessionMaxAge).Unix(),
	}
	if err := q.CreateSession(ctx, db.CreateSessionParams(sess)); err != nil {
		return db.Session{}, err
	}
	// Secure is set whenever the request arrived over HTTPS. Before a panel
	// domain is configured (M2) the dashboard is reached over plain HTTP.
	http.SetCookie(w, &http.Cookie{ // #nosec G124 -- Secure is conditional on HTTPS, see above
		Name: sessionCookie, Value: token, Path: "/",
		MaxAge: int(sessionMaxAge.Seconds()), HttpOnly: true, Secure: isHTTPS(r), SameSite: http.SameSiteStrictMode,
	})
	return sess, nil
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{ // #nosec G124 -- Secure is conditional on HTTPS
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: isHTTPS(r), SameSite: http.SameSiteStrictMode,
	})
}

// loadSession resolves the cookie to a valid session and user, or nil.
func (s *Server) loadSession(r *http.Request) (*authInfo, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil, nil
	}
	ctx := r.Context()
	id := auth.HashToken(c.Value)
	sess, err := s.st.Q.GetSession(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	now := s.now()
	if now.Unix() >= sess.ExpiresAt || now.Sub(time.Unix(sess.LastSeenAt, 0)) >= sessionIdleTimeout {
		_ = s.st.Q.DeleteSession(ctx, id)
		return nil, nil
	}
	user, err := s.st.Q.GetUserByID(ctx, sess.UserID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if now.Sub(time.Unix(sess.LastSeenAt, 0)) >= sessionTouchEvery {
		if err := s.st.Q.TouchSession(ctx, db.TouchSessionParams{LastSeenAt: now.Unix(), ID: id}); err != nil {
			return nil, err
		}
		sess.LastSeenAt = now.Unix()
	}
	return &authInfo{User: user, Session: sess}, nil
}

// requireAuth rejects requests without a valid session and enforces the
// CSRF token on state-changing methods.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a, err := s.loadSession(r)
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		if a == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
			return
		}
		if !isSafeMethod(r.Method) {
			got := r.Header.Get(csrfHeader)
			if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(a.Session.CsrfToken)) != 1 {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "invalid CSRF token"})
				return
			}
		}
		next(w, r.WithContext(context.WithValue(r.Context(), authKey, a)))
	}
}

func isSafeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// sameOrigin blocks cross-site state-changing requests to the API, including
// the unauthenticated setup and login endpoints.
func sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isSafeMethod(r.Method) {
			if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-site request blocked"})
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				if err != nil || u.Host != r.Host {
					writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin request blocked"})
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}
