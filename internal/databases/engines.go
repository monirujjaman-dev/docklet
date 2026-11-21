// Package databases provisions managed PostgreSQL, MySQL, MariaDB and Redis
// containers with persistent volumes, connection details and backups.
package databases

import (
	"fmt"
	"net/url"
	"strings"
)

// Engine describes how to run, probe, back up and restore one database type.
type Engine struct {
	Name          string
	Label         string
	Repo          string
	Versions      []string // allowed image tags, first is the default
	Port          int
	DefaultEnvKey string // env var name used when linking to an app
	HasUser       bool   // creates a database + user (not Redis)
	BackupExt     string
	dataPath      func(version string) string
	containerEnv  func(c Creds) []string
	cmd           func(c Creds) []string
	readyCmd      func(c Creds) ([]string, []string)
	scheme        string
	readyStdout   string // required substring of stdout, if any
}

// Creds are the decrypted credentials of a database.
type Creds struct {
	DBName, User, Password, RootPassword string
	extraUser                            bool // a database user, not the built-in owner
}

// Engines lists supported engines by name.
var Engines = map[string]*Engine{
	"postgres": {
		Name: "postgres", Label: "PostgreSQL", Repo: "postgres", Port: 5432,
		Versions:      []string{"17-alpine", "18-alpine", "16-alpine", "15-alpine"},
		DefaultEnvKey: "DATABASE_URL", HasUser: true, BackupExt: ".dump", scheme: "postgres",
		dataPath: func(v string) string {
			// The official image moved PGDATA's parent for 18+.
			if strings.HasPrefix(v, "18") {
				return "/var/lib/postgresql"
			}
			return "/var/lib/postgresql/data"
		},
		containerEnv: func(c Creds) []string {
			return []string{"POSTGRES_USER=" + c.User, "POSTGRES_PASSWORD=" + c.Password, "POSTGRES_DB=" + c.DBName}
		},
		readyCmd: func(c Creds) ([]string, []string) {
			// TCP (-h) so the init-time socket-only server isn't mistaken for ready.
			return []string{"pg_isready", "-h", "127.0.0.1", "-U", c.User, "-d", c.DBName}, nil
		},
	},
	"mysql": {
		Name: "mysql", Label: "MySQL", Repo: "mysql", Port: 3306,
		Versions:      []string{"8.4", "9.4", "8.0"},
		DefaultEnvKey: "DATABASE_URL", HasUser: true, BackupExt: ".sql.gz", scheme: "mysql",
		dataPath: func(string) string { return "/var/lib/mysql" },
		containerEnv: func(c Creds) []string {
			return []string{"MYSQL_ROOT_PASSWORD=" + c.RootPassword, "MYSQL_DATABASE=" + c.DBName,
				"MYSQL_USER=" + c.User, "MYSQL_PASSWORD=" + c.Password}
		},
		readyCmd: func(c Creds) ([]string, []string) {
			return []string{"mysqladmin", "ping", "-h", "127.0.0.1", "-uroot", "--silent"}, []string{"MYSQL_PWD=" + c.RootPassword}
		},
	},
	"mariadb": {
		Name: "mariadb", Label: "MariaDB", Repo: "mariadb", Port: 3306,
		Versions:      []string{"11.4", "11.8", "10.11"},
		DefaultEnvKey: "DATABASE_URL", HasUser: true, BackupExt: ".sql.gz", scheme: "mysql",
		dataPath: func(string) string { return "/var/lib/mysql" },
		containerEnv: func(c Creds) []string {
			return []string{"MARIADB_ROOT_PASSWORD=" + c.RootPassword, "MARIADB_DATABASE=" + c.DBName,
				"MARIADB_USER=" + c.User, "MARIADB_PASSWORD=" + c.Password}
		},
		readyCmd: func(c Creds) ([]string, []string) {
			return []string{"mariadb-admin", "ping", "-h", "127.0.0.1", "-uroot", "--silent"}, []string{"MYSQL_PWD=" + c.RootPassword}
		},
	},
	"redis": {
		Name: "redis", Label: "Redis", Repo: "redis", Port: 6379,
		Versions:      []string{"7.4-alpine", "7.2-alpine"},
		DefaultEnvKey: "REDIS_URL", BackupExt: ".rdb.gz", scheme: "redis",
		dataPath:     func(string) string { return "/data" },
		containerEnv: func(Creds) []string { return nil },
		cmd: func(c Creds) []string {
			// AOF for durability; DEBUG (loopback only) lets a restore reload an RDB in place.
			return []string{"redis-server", "--appendonly", "yes", "--requirepass", c.Password, "--enable-debug-command", "local"}
		},
		readyCmd: func(c Creds) ([]string, []string) {
			return []string{"redis-cli", "--no-auth-warning", "ping"}, []string{"REDISCLI_AUTH=" + c.Password}
		},
		readyStdout: "PONG",
	},
}

// Image returns the image reference for a version.
func (e *Engine) Image(version string) string { return e.Repo + ":" + version }

// ValidVersion reports whether version is offered for this engine.
func (e *Engine) ValidVersion(v string) bool {
	for _, x := range e.Versions {
		if x == v {
			return true
		}
	}
	return false
}

// URL is the connection string apps use on the project network.
func (e *Engine) URL(host string, c Creds) string {
	u := url.URL{Scheme: e.scheme, Host: fmt.Sprintf("%s:%d", host, e.Port)}
	switch e.Name {
	case "redis":
		// "default" is Redis's built-in ACL user; an empty username makes
		// some clients (redis-cli among them) send AUTH "" <pw>, which fails.
		user := "default"
		if c.extraUser {
			user = c.User
		}
		u.User = url.UserPassword(user, c.Password)
		u.Path = "/0"
	case "postgres":
		u.User = url.UserPassword(c.User, c.Password)
		u.Path = "/" + c.DBName
		u.RawQuery = "sslmode=disable"
	default:
		u.User = url.UserPassword(c.User, c.Password)
		u.Path = "/" + c.DBName
	}
	return u.String()
}
