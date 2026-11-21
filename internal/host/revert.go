package host

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Changes that could lock the admin out (SSH port, password login, enabling
// the firewall) are applied together with a revert job scheduled on the host
// itself (systemd-run). If the admin doesn't confirm in time, the host undoes
// the change even when Docklet or the browser is gone.

// RevertUnit prefixes the transient systemd units holding a pending revert.
// Each change gets its own name: systemd keeps a stopped transient unit
// around for a while and refuses to create another with the same name.
const RevertUnit = "docklet-revert"

// DefaultConfirmWindow is how long a risky change waits for confirmation.
const DefaultConfirmWindow = 5 * time.Minute

const listRevertTimers = `systemctl list-units --all --plain --no-legend '` + RevertUnit + `-*.timer' 2>/dev/null | awk '{print $1}'`

// revertFile holds the pending revert, so it can also be run on demand.
const revertFile = "/var/lib/docklet-host/revert.sh"

// scheduleRevert replaces any pending revert with script (run by /bin/sh on
// the host with env) after window.
func (s *Service) scheduleRevert(ctx context.Context, script string, env []string, window time.Duration) error {
	s.dropRevert(ctx)
	var b strings.Builder
	b.WriteString("# Docklet: undo an unconfirmed change\n")
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		if strings.ContainsAny(v, "'\n") {
			return fmt.Errorf("revert: unsafe value for %s", k)
		}
		fmt.Fprintf(&b, "%s='%s'\n", k, v)
	}
	b.WriteString(script)
	b.WriteString("\nrm -f " + revertFile + "\n")
	if _, err := Script(ctx, s.R, `set -e; mkdir -p /var/lib/docklet-host; printf '%s' "$REVERT" > `+revertFile+`.new; mv `+revertFile+`.new `+revertFile,
		"REVERT="+b.String()); err != nil {
		return err
	}
	unit := RevertUnit + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	_, err := Check(ctx, s.R, []string{"systemd-run", "--unit=" + unit, "--on-active=" + strconv.Itoa(int(window.Seconds())),
		"--description=Docklet: undo an unconfirmed change", "--timer-property=AccuracySec=1s", "/bin/sh", revertFile})
	return err
}

// dropRevert cancels pending reverts without running them.
func (s *Service) dropRevert(ctx context.Context) {
	_, _ = Script(ctx, s.R, `for t in $(`+listRevertTimers+`); do systemctl stop "$t"; done
rm -f `+revertFile+`; systemctl reset-failed '`+RevertUnit+`-*' 2>/dev/null; true`)
}

// runRevertNow runs the pending revert immediately.
func (s *Service) runRevertNow(ctx context.Context) error {
	_, err := Script(ctx, s.R, `for t in $(`+listRevertTimers+`); do systemctl stop "$t"; done
systemctl reset-failed '`+RevertUnit+`-*' 2>/dev/null
if [ -f `+revertFile+` ]; then /bin/sh `+revertFile+`; fi
rm -f `+revertFile+`; true`)
	return err
}

// RevertPending reports whether a revert is still scheduled on the host.
func (s *Service) RevertPending(ctx context.Context) (bool, error) {
	out, err := Script(ctx, s.R, `systemctl list-units --plain --no-legend --state=active '`+RevertUnit+`-*.timer' 2>/dev/null | grep -c . ; true`)
	return strings.TrimSpace(out) != "0" && strings.TrimSpace(out) != "", err
}

// Pending describes a change waiting for confirmation (kept by Docklet).
type Pending struct {
	Kind        string    `json:"kind"` // ssh, firewall
	Description string    `json:"description"`
	Deadline    time.Time `json:"deadline"`
	// ConfirmData is what Confirm needs to finish the change.
	ConfirmData map[string]string `json:"confirmData,omitempty"`
}

// CancelRevert keeps a pending change (firewall): the revert won't run.
func (s *Service) CancelRevert(ctx context.Context) error { s.dropRevert(ctx); return nil }

// RevertNow undoes a pending change immediately.
func (s *Service) RevertNow(ctx context.Context) error { return s.runRevertNow(ctx) }
