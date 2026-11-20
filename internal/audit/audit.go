// Package audit writes and verifies the append-only, hash-chained audit log.
//
// Every entry stores hash = SHA-256(prev_hash || canonical(entry)). Editing,
// inserting or removing any row in the middle of the log breaks the chain,
// which Verify reports. SQLite triggers additionally block UPDATE/DELETE.
// Limitation: truncating the newest entries is not detectable from the
// database alone; anchoring the head hash externally is a later step.
package audit

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/store/db"
)

// GenesisHash is the prev_hash of the first entry.
const GenesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

// Actions recorded by M1. Keep names stable: they are part of the log.
const (
	ActionSetupCompleted  = "setup.completed"
	ActionLoginSucceeded  = "auth.login"
	ActionLoginFailed     = "auth.login_failed"
	ActionAccountLocked   = "auth.locked"
	ActionLogout          = "auth.logout"
	ActionPasswordChanged = "account.password_changed"
	ActionTOTPEnabled     = "account.totp_enabled"
	ActionTOTPDisabled    = "account.totp_disabled"
	ActionPasswordReset   = "admin.password_reset"
)

// Entry is what callers provide; ID, Ts and hashes are filled by Append.
type Entry struct {
	ActorID *int64
	Actor   string
	Action  string
	Target  string
	IP      string
	Details map[string]any
}

// Append writes an entry using q. Call it inside the same transaction as the
// action it records so both commit or neither does.
func Append(ctx context.Context, q *db.Queries, e Entry, now time.Time) (db.AuditLog, error) {
	if e.Actor == "" || e.Action == "" {
		return db.AuditLog{}, errors.New("audit: actor and action are required")
	}
	details := "{}"
	if len(e.Details) > 0 {
		b, err := json.Marshal(e.Details)
		if err != nil {
			return db.AuditLog{}, fmt.Errorf("audit: details: %w", err)
		}
		details = string(b)
	}

	prevID, prevHash := int64(0), GenesisHash
	last, err := q.LastAuditEntry(ctx)
	switch {
	case err == nil:
		prevID, prevHash = last.ID, last.Hash
	case errors.Is(err, sql.ErrNoRows):
	default:
		return db.AuditLog{}, err
	}

	row := db.AuditLog{
		ID:       prevID + 1,
		Ts:       now.UnixMilli(),
		Actor:    e.Actor,
		Action:   e.Action,
		Target:   e.Target,
		Ip:       e.IP,
		Details:  details,
		PrevHash: prevHash,
	}
	if e.ActorID != nil {
		row.ActorID = sql.NullInt64{Int64: *e.ActorID, Valid: true}
	}
	row.Hash = Hash(row)

	err = q.InsertAuditEntry(ctx, db.InsertAuditEntryParams(row))
	return row, err
}

// Hash computes the chained hash of a row (its Hash field is ignored).
func Hash(r db.AuditLog) string {
	var actorID any
	if r.ActorID.Valid {
		actorID = r.ActorID.Int64
	}
	// JSON array encoding gives unambiguous field boundaries.
	canon, _ := json.Marshal([]any{r.ID, r.Ts, actorID, r.Actor, r.Action, r.Target, r.Ip, r.Details})
	h := sha256.New()
	h.Write([]byte(r.PrevHash))
	h.Write([]byte{'\n'})
	h.Write(canon)
	return hex.EncodeToString(h.Sum(nil))
}

// Result is the outcome of Verify.
type Result struct {
	OK       bool   `json:"ok"`
	Checked  int64  `json:"checked"`
	BrokenAt int64  `json:"brokenAt,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// Verify walks the whole log in id order and checks every link.
func Verify(ctx context.Context, q *db.Queries) (Result, error) {
	const batch = 1000
	res := Result{OK: true}
	prevID, prevHash := int64(0), GenesisHash
	for {
		rows, err := q.AuditEntriesFrom(ctx, db.AuditEntriesFromParams{AfterID: prevID, Limit: batch})
		if err != nil {
			return res, err
		}
		for _, r := range rows {
			res.Checked++
			switch {
			case r.ID != prevID+1:
				return broken(res, r.ID, fmt.Sprintf("gap: expected id %d", prevID+1)), nil
			case r.PrevHash != prevHash:
				return broken(res, r.ID, "prev_hash does not match previous entry"), nil
			case Hash(r) != r.Hash:
				return broken(res, r.ID, "entry content does not match its hash"), nil
			}
			prevID, prevHash = r.ID, r.Hash
		}
		if len(rows) < batch {
			return res, nil
		}
	}
}

func broken(res Result, id int64, reason string) Result {
	res.OK, res.BrokenAt, res.Reason = false, id, reason
	return res
}
