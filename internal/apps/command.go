package apps

import (
	"errors"
	"strings"
)

// App kinds.
const (
	KindWeb    = "web"    // serves HTTP; may have domains
	KindWorker = "worker" // background process; never routed by Caddy
)

// SplitCommand splits a command line into arguments the way a POSIX shell
// would for simple cases: whitespace separates words, single quotes are
// literal, double quotes allow \" and \\, and a backslash escapes the next
// character outside quotes. No variables, globs or pipes: the command runs
// without a shell (use `sh -c '…'` for that).
func SplitCommand(s string) ([]string, error) {
	var args []string
	var cur strings.Builder
	inWord := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch ch {
		case '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, errors.New("unterminated single quote")
			}
			cur.WriteString(s[i+1 : i+1+j])
			i += j + 1
			inWord = true
		case '"':
			i++
			for ; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) && (s[i+1] == '"' || s[i+1] == '\\') {
					i++
				}
				cur.WriteByte(s[i])
			}
			if i >= len(s) {
				return nil, errors.New("unterminated double quote")
			}
			inWord = true
		case '\\':
			if i+1 >= len(s) {
				return nil, errors.New("trailing backslash")
			}
			i++
			cur.WriteByte(s[i])
			inWord = true
		case ' ', '\t', '\n', '\r':
			if inWord {
				args = append(args, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteByte(ch)
			inWord = true
		}
	}
	if inWord {
		args = append(args, cur.String())
	}
	return args, nil
}
