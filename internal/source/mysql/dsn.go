package mysql

import (
	"fmt"
	"strings"
)

// defaultPort is MySQL's, used when the DSN names a host without one.
const defaultPort = "3306"

// dsn is a parsed connection string.
type dsn struct {
	user     string
	password string
	addr     string
}

// parseDSN reads the connection forms an operator is likely to write:
//
//	user:password@tcp(host:port)/database
//	user:password@host:port
//
// The database part is accepted and discarded — replication is server-wide, and
// silently ignoring it is friendlier than refusing a DSN copied from somewhere
// else.
//
// No error from here carries the password. These strings reach logs, terminals
// and tickets, and a credential that travels with a complaint about a typo is the
// most ordinary way one escapes.
func parseDSN(s string) (dsn, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return dsn{}, fmt.Errorf("mysql: no connection string given; set MULLIGAN_DSN to user:password@tcp(host:3306)/")
	}

	// Split on the last @ so a password containing one still parses.
	at := strings.LastIndex(s, "@")
	if at < 0 {
		return dsn{}, fmt.Errorf("mysql: connection string is missing the @ between credentials and host; " +
			"expected user:password@tcp(host:3306)/")
	}

	credentials, hostPart := s[:at], s[at+1:]

	var d dsn
	if colon := strings.Index(credentials, ":"); colon >= 0 {
		d.user, d.password = credentials[:colon], credentials[colon+1:]
	} else {
		d.user = credentials
	}
	if d.user == "" {
		return dsn{}, fmt.Errorf("mysql: connection string names no user; expected user:password@tcp(host:3306)/")
	}

	addr, err := parseHost(hostPart)
	if err != nil {
		return dsn{}, err
	}
	d.addr = addr
	return d, nil
}

func parseHost(hostPart string) (string, error) {
	// Trim the optional /database, which replication does not use.
	if slash := strings.Index(hostPart, "/"); slash >= 0 {
		hostPart = hostPart[:slash]
	}

	if open := strings.Index(hostPart, "("); open >= 0 {
		close := strings.LastIndex(hostPart, ")")
		if close < open {
			return "", fmt.Errorf("mysql: connection string has an unclosed ( in the host; " +
				"expected user:password@tcp(host:3306)/")
		}
		hostPart = hostPart[open+1 : close]
	}

	hostPart = strings.TrimSpace(hostPart)
	if hostPart == "" {
		return "", fmt.Errorf("mysql: connection string names no host; expected user:password@tcp(host:3306)/")
	}

	if !strings.Contains(hostPart, ":") {
		hostPart += ":" + defaultPort
	}
	return hostPart, nil
}

// Redact removes the password from a connection string so it can be printed.
//
// Every message that mentions the connection goes through this, including the
// ones the replication library would otherwise be handed verbatim. The address
// is kept, because a redacted string that names nothing makes the message it
// appears in useless.
func Redact(s string) string {
	at := strings.LastIndex(s, "@")
	if at < 0 {
		return s
	}

	credentials, host := s[:at], s[at+1:]
	if colon := strings.Index(credentials, ":"); colon >= 0 {
		credentials = credentials[:colon] + ":***"
	}
	return credentials + "@" + host
}
