package main

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/audit"
	"github.com/monirujjaman-dev/docklet/internal/auth"
	"github.com/monirujjaman-dev/docklet/internal/store"
	"github.com/monirujjaman-dev/docklet/internal/store/db"
)

// openExisting opens the database without creating a new data directory by
// accident (a typo in -data-dir should fail, not produce an empty install).
func openExisting(ctx context.Context, dataDir string) (*store.Store, error) {
	p := dbPath(dataDir)
	if _, err := os.Stat(p); err != nil {
		return nil, fmt.Errorf("no database at %s (wrong -data-dir?)", p)
	}
	return store.Open(ctx, p)
}

func cmdAuditVerify(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("audit verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := dataDirFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx := context.Background()
	st, err := openExisting(ctx, *dataDir)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	defer func() { _ = st.Close() }()

	res, err := audit.Verify(ctx, st.Q)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if !res.OK {
		fmt.Fprintf(stdout, "BROKEN: audit log integrity check failed at entry %d (%s); %d entries checked\n",
			res.BrokenAt, res.Reason, res.Checked)
		return 1
	}
	fmt.Fprintf(stdout, "OK: %d audit entries verified\n", res.Checked)
	return 0
}

func cmdResetPassword(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("admin reset-password", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := dataDirFlag(fs)
	username := fs.String("username", "", "user to reset (required)")
	fromStdin := fs.Bool("password-stdin", false, "read the new password from stdin instead of generating one")
	disable2FA := fs.Bool("disable-2fa", false, "also disable two-factor authentication")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *username == "" {
		fmt.Fprintln(stderr, "error: -username is required")
		return 2
	}

	var password string
	generated := false
	if *fromStdin {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		password = strings.TrimRight(line, "\r\n")
		if err := auth.ValidatePassword(password); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
	} else {
		p, err := auth.RandomPassword()
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		password, generated = p, true
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}

	ctx := context.Background()
	st, err := openExisting(ctx, *dataDir)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	defer func() { _ = st.Close() }()

	now := time.Now()
	err = st.Tx(ctx, func(q *db.Queries) error {
		u, err := q.GetUserByUsername(ctx, *username)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("user %q not found", *username)
		}
		if err != nil {
			return err
		}
		if err := q.UpdatePassword(ctx, db.UpdatePasswordParams{PasswordHash: hash, UpdatedAt: now.Unix(), ID: u.ID}); err != nil {
			return err
		}
		if err := q.DeleteUserSessions(ctx, u.ID); err != nil {
			return err
		}
		if *disable2FA {
			if err := q.DisableTOTP(ctx, db.DisableTOTPParams{UpdatedAt: now.Unix(), ID: u.ID}); err != nil {
				return err
			}
		}
		_, err = audit.Append(ctx, q, audit.Entry{
			Actor: "cli", Action: audit.ActionPasswordReset, Target: "user:" + u.Username,
			Details: map[string]any{"disable2fa": *disable2FA},
		}, now)
		return err
	})
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}

	fmt.Fprintf(stdout, "Password for %q reset; all sessions signed out.\n", *username)
	if generated {
		fmt.Fprintf(stdout, "New password: %s\n", password)
	}
	return 0
}
