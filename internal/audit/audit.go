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

	ActionPanelDomainSet     = "settings.panel_domain_set"
	ActionPanelDomainCleared = "settings.panel_domain_cleared"
	ActionPanelVerified      = "settings.panel_verified"
	ActionPanelVerifyFailed  = "settings.panel_verify_failed"

	ActionProjectCreated   = "project.created"
	ActionProjectDeleted   = "project.deleted"
	ActionAppCreated       = "app.created"
	ActionAppUpdated       = "app.updated"
	ActionAppDeleted       = "app.deleted"
	ActionAppEnvUpdated    = "app.env_updated"
	ActionAppDomains       = "app.domains_updated"
	ActionDeployRequested  = "app.deploy_requested"
	ActionDeploySucceeded  = "app.deploy_succeeded"
	ActionDeployFailed     = "app.deploy_failed"
	ActionAppStarted       = "app.started"
	ActionAppStopped       = "app.stopped"
	ActionAppRestarted     = "app.restarted"
	ActionContainerMissing = "app.container_missing"

	ActionAppGitUpdated     = "app.git_updated"
	ActionAppDeployKey      = "app.deploy_key_regenerated" // #nosec G101 -- audit action name, not a credential
	ActionAppWebhookRotated = "app.webhook_rotated"        //nolint:gosec // audit action name, not a credential
	ActionAppRollback       = "app.rollback_requested"

	ActionDatabaseCreated    = "database.created"
	ActionDatabaseReady      = "database.ready"
	ActionDatabaseFailed     = "database.failed"
	ActionDatabaseDeleted    = "database.deleted"
	ActionDatabaseStarted    = "database.started"
	ActionDatabaseStopped    = "database.stopped"
	ActionDatabaseRestarted  = "database.restarted"
	ActionDatabaseRecreated  = "database.recreated"
	ActionDatabasePublicPort = "database.public_port_changed"
	ActionDatabaseRevealed   = "database.credentials_viewed"
	ActionDatabaseLinked     = "database.linked"
	ActionDatabaseUnlinked   = "database.unlinked"
	ActionBackupPolicy       = "database.backup_policy_changed"
	ActionBackupRequested    = "database.backup_requested"
	ActionBackupSucceeded    = "database.backup_succeeded"
	ActionBackupFailed       = "database.backup_failed"
	ActionRestoreRequested   = "database.restore_requested"
	ActionRestoreSucceeded   = "database.restore_succeeded"
	ActionRestoreFailed      = "database.restore_failed"
	ActionBackupUploaded     = "database.backup_uploaded"
	ActionBackupUploadFailed = "database.backup_upload_failed"
	ActionDestinationChanged = "database.backup_destination_changed"
	ActionDBUserCreated      = "database.user_created"
	ActionDBUserUpdated      = "database.user_updated"
	ActionDBUserDeleted      = "database.user_deleted"
	ActionDBUserRevealed     = "database.user_credentials_viewed"

	ActionS3Created = "s3_destination.created"
	ActionS3Updated = "s3_destination.updated"
	ActionS3Deleted = "s3_destination.deleted"
	ActionS3Tested  = "s3_destination.tested"

	ActionSelfBackupPolicy   = "system.backup_schedule_changed"
	ActionSelfBackupUploaded = "system.backup_uploaded"
	ActionSelfBackupFailed   = "system.backup_upload_failed"

	ActionServerHostname   = "server.hostname_changed"
	ActionServerTimezone   = "server.timezone_changed"
	ActionServerSwap       = "server.swap_changed"
	ActionServerSwappiness = "server.swappiness_changed"
	ActionServerReboot     = "server.reboot_requested"
	ActionServerSignal     = "server.process_signalled"
	ActionServerService    = "server.service_action"
	ActionServerCleanup    = "server.disk_cleanup"

	ActionServerSSH                 = "server.ssh_changed"
	ActionServerSSHKeyAdded         = "server.ssh_key_added"
	ActionServerSSHKeyRemoved       = "server.ssh_key_removed"
	ActionServerChangeConfirmed     = "server.change_confirmed"
	ActionServerChangeUndone        = "server.change_undone"
	ActionServerPackageInstalled    = "server.package_installed"
	ActionServerFirewall            = "server.firewall_changed"
	ActionServerFirewallRuleAdded   = "server.firewall_rule_added"
	ActionServerFirewallRuleDeleted = "server.firewall_rule_deleted"
	ActionServerFirewallDefaults    = "server.firewall_defaults_changed"
	ActionServerFail2Ban            = "server.fail2ban_changed"
	ActionServerUnban               = "server.ip_unbanned"
	ActionDatabaseAllowlist         = "database.allowlist_changed"

	ActionTerminalStarted = "terminal.session_started"
	ActionTerminalEnded   = "terminal.session_ended"

	ActionFileWritten    = "files.written"
	ActionFileUploaded   = "files.uploaded"
	ActionFileDownloaded = "files.downloaded"
	ActionFileMkdir      = "files.folder_created"
	ActionFileRenamed    = "files.renamed"
	ActionFileMoved      = "files.moved"
	ActionFileDeleted    = "files.deleted"
	ActionFileChmod      = "files.mode_changed"
	ActionFileChown      = "files.owner_changed"

	ActionUpdateStarted = "docklet.update_started"
	ActionUpdateAuto    = "docklet.auto_update_changed"

	ActionCronCreated      = "cron.created"
	ActionCronUpdated      = "cron.updated"
	ActionCronDeleted      = "cron.deleted"
	ActionCronRunRequested = "cron.run_requested"
	ActionCronRunCancelled = "cron.run_cancelled"

	ActionSystemBackup = "system.backup_downloaded"
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
