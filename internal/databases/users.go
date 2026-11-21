package databases

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/monirujjaman-dev/docklet/internal/store/db"
)

// Permission levels of extra database users.
const (
	RoleReadOnly  = "readonly"  // read data
	RoleReadWrite = "readwrite" // read and change data, no schema changes
	RoleAdmin     = "admin"     // everything inside this database, incl. creating tables
)

// Roles lists the valid roles.
var Roles = []string{RoleReadOnly, RoleReadWrite, RoleAdmin}

var userNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,30}$`)

// Names that belong to the engines themselves.
var reservedUsers = map[string]bool{
	"root": true, "postgres": true, "mysql": true, "mariadb": true, "default": true, "public": true,
	"replicator": true, "repl": true, "admin_root": true,
}

// ValidateUserName checks a new user name for database d.
func ValidateUserName(d db.Database, name string) error {
	if !userNameRe.MatchString(name) {
		return errors.New("user name must be 1-31 characters: lowercase letters, digits and '_', starting with a letter")
	}
	if reservedUsers[name] || strings.HasPrefix(name, "pg_") || strings.HasPrefix(name, "mysql") || strings.EqualFold(name, d.Username) {
		return fmt.Errorf("%q is reserved", name)
	}
	return nil
}

func validRole(r string) bool { return r == RoleReadOnly || r == RoleReadWrite || r == RoleAdmin }

func userAAD(dbID int64, name string) []byte {
	return []byte(fmt.Sprintf("dbuser:%d/%s:password", dbID, name))
}

// UserPassword decrypts a database user's password.
func (s *Service) UserPassword(u db.DatabaseUser) (string, error) {
	b, err := s.box.Open(u.PasswordEnc, userAAD(u.DatabaseID, u.Username))
	return string(b), err
}

// UserURL is the connection URL for a database user inside the project.
func (s *Service) UserURL(d db.Database, u db.DatabaseUser) (string, error) {
	pw, err := s.UserPassword(u)
	if err != nil {
		return "", err
	}
	e := Engines[d.Engine]
	return e.URL(strings.ToLower(d.Name), Creds{User: u.Username, Password: pw, DBName: d.DbName, extraUser: true}), nil
}

func (s *Service) requireRunning(d db.Database) error {
	if d.ContainerID == "" || d.RuntimeStatus != StatusRunning {
		return errors.New("start the database first: users are changed inside the running database")
	}
	return nil
}

// CreateUser adds a user with a generated password and applies it.
func (s *Service) CreateUser(ctx context.Context, dbID int64, name, role string) (db.DatabaseUser, error) {
	defer s.lock(dbID)()
	d, err := s.st.Q.GetDatabase(ctx, dbID)
	if err != nil {
		return db.DatabaseUser{}, err
	}
	if err := ValidateUserName(d, name); err != nil {
		return db.DatabaseUser{}, err
	}
	if !validRole(role) {
		return db.DatabaseUser{}, fmt.Errorf("role must be one of %s", strings.Join(Roles, ", "))
	}
	if err := s.requireRunning(d); err != nil {
		return db.DatabaseUser{}, err
	}
	pw, err := GeneratePassword()
	if err != nil {
		return db.DatabaseUser{}, err
	}
	enc, err := s.box.Seal([]byte(pw), userAAD(dbID, name))
	if err != nil {
		return db.DatabaseUser{}, err
	}
	now := s.now().Unix()
	u, err := s.st.Q.CreateDatabaseUser(ctx, db.CreateDatabaseUserParams{DatabaseID: dbID, Username: name, PasswordEnc: enc, Role: role, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return u, fmt.Errorf("user %q already exists", name)
		}
		return u, err
	}
	if err := s.syncUsers(ctx, d); err != nil {
		_ = s.st.Q.DeleteDatabaseUser(context.WithoutCancel(ctx), u.ID)
		_ = s.dropUser(context.WithoutCancel(ctx), d, name)
		return u, err
	}
	return u, nil
}

// UpdateUser changes a user's role and/or generates a new password.
func (s *Service) UpdateUser(ctx context.Context, userID int64, role string, newPassword bool) (db.DatabaseUser, error) {
	u, err := s.st.Q.GetDatabaseUser(ctx, userID)
	if err != nil {
		return u, err
	}
	defer s.lock(u.DatabaseID)()
	d, err := s.st.Q.GetDatabase(ctx, u.DatabaseID)
	if err != nil {
		return u, err
	}
	if err := s.requireRunning(d); err != nil {
		return u, err
	}
	before := u
	if role != "" {
		if !validRole(role) {
			return u, fmt.Errorf("role must be one of %s", strings.Join(Roles, ", "))
		}
		u.Role = role
	}
	if newPassword {
		pw, err := GeneratePassword()
		if err != nil {
			return u, err
		}
		if u.PasswordEnc, err = s.box.Seal([]byte(pw), userAAD(u.DatabaseID, u.Username)); err != nil {
			return u, err
		}
	}
	if err := s.st.Q.UpdateDatabaseUser(ctx, db.UpdateDatabaseUserParams{PasswordEnc: u.PasswordEnc, Role: u.Role, UpdatedAt: s.now().Unix(), ID: u.ID}); err != nil {
		return u, err
	}
	if err := s.syncUsers(ctx, d); err != nil {
		_ = s.st.Q.UpdateDatabaseUser(context.WithoutCancel(ctx), db.UpdateDatabaseUserParams{PasswordEnc: before.PasswordEnc, Role: before.Role, UpdatedAt: before.UpdatedAt, ID: u.ID})
		_ = s.syncUsers(context.WithoutCancel(ctx), d)
		return before, err
	}
	return u, nil
}

// DeleteUser removes a user from the database engine and from Docklet.
func (s *Service) DeleteUser(ctx context.Context, userID int64) (db.DatabaseUser, error) {
	u, err := s.st.Q.GetDatabaseUser(ctx, userID)
	if err != nil {
		return u, err
	}
	defer s.lock(u.DatabaseID)()
	d, err := s.st.Q.GetDatabase(ctx, u.DatabaseID)
	if err != nil {
		return u, err
	}
	if err := s.requireRunning(d); err != nil {
		return u, err
	}
	if err := s.dropUser(ctx, d, u.Username); err != nil {
		return u, err
	}
	return u, s.st.Q.DeleteDatabaseUser(ctx, u.ID)
}

// --- Engine commands --------------------------------------------------------

func pgIdent(s string) string   { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
func sqlString(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
func myUser(s string) string    { return "'" + strings.ReplaceAll(s, "'", "''") + "'@'%'" }
func myIdent(s string) string   { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }

// adminExec runs a client command as the database's administrator and
// returns its output; errors printed by the client fail the call.
func (s *Service) adminExec(ctx context.Context, d db.Database, args ...string) (string, error) {
	c, err := s.Creds(d)
	if err != nil {
		return "", err
	}
	var cmd, env []string
	switch d.Engine {
	case "postgres":
		cmd = append([]string{"psql", "-v", "ON_ERROR_STOP=1", "-X", "-q", "-t", "-A", "-h", "127.0.0.1", "-U", c.User, "-d", c.DBName}, args...)
		env = []string{"PGPASSWORD=" + c.Password}
	case "mysql", "mariadb":
		cmd = append([]string{mysqlClient(d.Engine), "-h", "127.0.0.1", "-uroot", "-N", "-B"}, args...)
		env = []string{"MYSQL_PWD=" + c.RootPassword}
	case "redis":
		cmd = append([]string{"redis-cli", "--no-auth-warning"}, args...)
		env = []string{"REDISCLI_AUTH=" + c.Password}
	default:
		return "", fmt.Errorf("engine %s has no users", d.Engine)
	}
	var out bytes.Buffer
	if err := s.execTo(ctx, d.ContainerID, cmd, env, &out); err != nil {
		return out.String(), err
	}
	if d.Engine == "redis" && (strings.HasPrefix(out.String(), "ERR") || strings.HasPrefix(out.String(), "(error)") || strings.Contains(out.String(), "\nERR")) {
		return out.String(), errors.New(strings.TrimSpace(out.String()))
	}
	return out.String(), nil
}

func mysqlClient(engine string) string {
	if engine == "mariadb" {
		return "mariadb"
	}
	return "mysql"
}

type dbUser struct {
	name, password, role string
}

func (s *Service) loadUsers(ctx context.Context, d db.Database) ([]dbUser, error) {
	rows, err := s.st.Q.ListDatabaseUsers(ctx, d.ID)
	if err != nil {
		return nil, err
	}
	out := make([]dbUser, 0, len(rows))
	for _, r := range rows {
		pw, err := s.UserPassword(r)
		if err != nil {
			return nil, fmt.Errorf("decrypt password of %s: %w", r.Username, err)
		}
		out = append(out, dbUser{r.Username, pw, r.Role})
	}
	return out, nil
}

// syncUsers makes the engine's users and privileges match Docklet's list. It
// is idempotent, so it also repairs users after a container is recreated
// (and restores Redis ACL users, which Redis keeps only in memory).
func (s *Service) syncUsers(ctx context.Context, d db.Database) error {
	users, err := s.loadUsers(ctx, d)
	if err != nil || len(users) == 0 {
		return err
	}
	switch d.Engine {
	case "postgres":
		_, err = s.adminExec(ctx, d, "-c", PostgresSyncSQL(d.Username, d.DbName, users))
	case "mysql", "mariadb":
		_, err = s.adminExec(ctx, d, "-e", MySQLSyncSQL(d.DbName, users))
	case "redis":
		for _, u := range users {
			if _, err = s.adminExec(ctx, d, RedisACL(u)...); err != nil {
				break
			}
		}
	}
	if err != nil {
		return fmt.Errorf("apply database users: %w", err)
	}
	return nil
}

func (s *Service) dropUser(ctx context.Context, d db.Database, name string) error {
	var err error
	switch d.Engine {
	case "postgres":
		_, err = s.adminExec(ctx, d, "-c", fmt.Sprintf(`DO $$BEGIN IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = %s) THEN
  EXECUTE 'REASSIGN OWNED BY %s TO %s'; EXECUTE 'DROP OWNED BY %s'; EXECUTE 'DROP ROLE %s';
END IF; END$$;`, sqlString(name), pgIdent(name), pgIdent(d.Username), pgIdent(name), pgIdent(name)))
	case "mysql", "mariadb":
		_, err = s.adminExec(ctx, d, "-e", "DROP USER IF EXISTS "+myUser(name)+";")
	case "redis":
		_, err = s.adminExec(ctx, d, "ACL", "DELUSER", name)
	}
	if err != nil {
		return fmt.Errorf("remove user %s: %w", name, err)
	}
	return nil
}

// PostgresSyncSQL builds the statements that (re)create users and set their
// privileges on the public schema. Objects can be created by the owner or by
// admin users, so default privileges are set for each of them.
func PostgresSyncSQL(owner, database string, users []dbUser) string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	// Before PostgreSQL 15 everyone may create tables in public.
	w(`REVOKE CREATE ON SCHEMA public FROM PUBLIC;`)
	grantors := []string{owner}
	for _, u := range users {
		if u.role == RoleAdmin {
			grantors = append(grantors, u.name)
		}
	}
	for _, u := range users {
		w(`DO $$BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = %s) THEN CREATE ROLE %s LOGIN; END IF; END$$;`, sqlString(u.name), pgIdent(u.name))
		w(`ALTER ROLE %s WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD %s;`, pgIdent(u.name), sqlString(u.password))
	}
	for _, u := range users {
		n := pgIdent(u.name)
		w(`REVOKE ALL ON DATABASE %s FROM %s;`, pgIdent(database), n)
		w(`REVOKE ALL ON SCHEMA public FROM %s;`, n)
		w(`REVOKE ALL ON ALL TABLES IN SCHEMA public FROM %s;`, n)
		w(`REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM %s;`, n)
		w(`REVOKE ALL ON ALL FUNCTIONS IN SCHEMA public FROM %s;`, n)
		for _, g := range grantors {
			if g == u.name {
				continue
			}
			for _, kind := range []string{"TABLES", "SEQUENCES", "FUNCTIONS"} {
				w(`ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA public REVOKE ALL ON %s FROM %s;`, pgIdent(g), kind, n)
			}
		}
		var dbPriv, schemaPriv, tablePriv, seqPriv, fnPriv string
		switch u.role {
		case RoleReadOnly:
			dbPriv, schemaPriv, tablePriv, seqPriv = "CONNECT", "USAGE", "SELECT", "SELECT"
		case RoleReadWrite:
			dbPriv, schemaPriv, tablePriv, seqPriv, fnPriv = "CONNECT, TEMPORARY", "USAGE", "SELECT, INSERT, UPDATE, DELETE", "USAGE, SELECT, UPDATE", "EXECUTE"
		case RoleAdmin:
			dbPriv, schemaPriv, tablePriv, seqPriv, fnPriv = "ALL", "ALL", "ALL", "ALL", "ALL"
		}
		w(`GRANT %s ON DATABASE %s TO %s;`, dbPriv, pgIdent(database), n)
		w(`GRANT %s ON SCHEMA public TO %s;`, schemaPriv, n)
		w(`GRANT %s ON ALL TABLES IN SCHEMA public TO %s;`, tablePriv, n)
		w(`GRANT %s ON ALL SEQUENCES IN SCHEMA public TO %s;`, seqPriv, n)
		if fnPriv != "" {
			w(`GRANT %s ON ALL FUNCTIONS IN SCHEMA public TO %s;`, fnPriv, n)
		}
		for _, g := range grantors {
			if g == u.name {
				continue
			}
			w(`ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA public GRANT %s ON TABLES TO %s;`, pgIdent(g), tablePriv, n)
			w(`ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA public GRANT %s ON SEQUENCES TO %s;`, pgIdent(g), seqPriv, n)
			if fnPriv != "" {
				w(`ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA public GRANT %s ON FUNCTIONS TO %s;`, pgIdent(g), fnPriv, n)
			}
		}
	}
	return b.String()
}

// MySQLSyncSQL (re)creates users with privileges on one database only.
func MySQLSyncSQL(database string, users []dbUser) string {
	var b strings.Builder
	for _, u := range users {
		priv := map[string]string{
			RoleReadOnly:  "SELECT, SHOW VIEW",
			RoleReadWrite: "SELECT, INSERT, UPDATE, DELETE, EXECUTE, SHOW VIEW, CREATE TEMPORARY TABLES, LOCK TABLES",
			RoleAdmin:     "ALL PRIVILEGES",
		}[u.role]
		fmt.Fprintf(&b, "CREATE USER IF NOT EXISTS %s IDENTIFIED BY %s;\n", myUser(u.name), sqlString(u.password))
		fmt.Fprintf(&b, "ALTER USER %s IDENTIFIED BY %s;\n", myUser(u.name), sqlString(u.password))
		fmt.Fprintf(&b, "REVOKE ALL PRIVILEGES, GRANT OPTION FROM %s;\n", myUser(u.name))
		fmt.Fprintf(&b, "GRANT %s ON %s.* TO %s;\n", priv, myIdent(database), myUser(u.name))
	}
	return b.String()
}

// RedisACL is the ACL SETUSER command for a user (all keys and channels).
func RedisACL(u dbUser) []string {
	perms := map[string][]string{
		RoleReadOnly:  {"+@read", "+@connection", "-@dangerous"},
		RoleReadWrite: {"+@all", "-@admin", "-@dangerous"},
		RoleAdmin:     {"+@all", "-@admin"}, // no CONFIG, ACL, SHUTDOWN, DEBUG
	}[u.role]
	return append([]string{"ACL", "SETUSER", u.name, "reset", "on", ">" + u.password, "~*", "&*"}, perms...)
}

// repairRedisUsers re-applies ACL users that a restarted Redis has lost.
func (s *Service) repairRedisUsers(ctx context.Context, d db.Database) {
	users, err := s.st.Q.ListDatabaseUsers(ctx, d.ID)
	if err != nil || len(users) == 0 {
		return
	}
	out, err := s.adminExec(ctx, d, "ACL", "USERS")
	if err != nil {
		return
	}
	have := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		have[strings.TrimSpace(l)] = true
	}
	for _, u := range users {
		if !have[u.Username] {
			if err := s.syncUsers(ctx, d); err != nil {
				s.log.Warn("restore redis users", "database", d.Name, "err", err)
			}
			return
		}
	}
}
