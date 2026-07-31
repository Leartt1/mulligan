package acceptance

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/learttytyri/mulligan/internal/change"
	"github.com/learttytyri/mulligan/internal/cli"
	"github.com/learttytyri/mulligan/internal/reverse"
)

const (
	rootPass = "mulligan-acceptance"
	bootWait = 90 * time.Second
)

// flavor is one server Mulligan claims to support. MariaDB is a fork with the
// same binlog settings but its own client binary and its own name for the
// statement that reports the current log.
type flavor struct {
	name   string
	image  string
	client string
}

var (
	mysql8  = flavor{name: "mysql", image: "mysql:8.0", client: "mysql"}
	mariadb = flavor{name: "mariadb", image: "mariadb:11.4", client: "mariadb"}
)

// mysqlServer is a throwaway server running in a container, configured to log
// exactly what Mulligan needs to reverse a change.
type mysqlServer struct {
	t         *testing.T
	flavor    flavor
	container string
}

// startMySQL boots the default flavor.
func startMySQL(t *testing.T) *mysqlServer {
	t.Helper()
	return startServer(t, mysql8)
}

// startServer boots a container and returns once the server answers.
//
// The three binlog settings are the ones Mulligan documents as required: ROW
// format for the row images, FULL row image so an update carries the values it
// overwrote, and FULL row metadata so the log names its own columns. MariaDB
// spells all three the same way from 10.5 onward.
func startServer(t *testing.T, fl flavor) *mysqlServer {
	t.Helper()
	requireDocker(t)

	cmd := exec.Command("docker", "run", "--detach", "--rm",
		"--env", "MYSQL_ROOT_PASSWORD="+rootPass,
		"--env", "MARIADB_ROOT_PASSWORD="+rootPass,
		// Publish on an ephemeral host port: the collector connects as a replica
		// over TCP from the test process, not through docker exec.
		"--publish", "0:3306",
		fl.image,
		"--log-bin=binlog",
		"--server-id=1",
		"--binlog-format=ROW",
		"--binlog-row-image=FULL",
		"--binlog-row-metadata=FULL",
	)

	// Only stdout carries the container ID. Reading combined output instead would
	// fold in whatever docker writes to stderr — pull progress, most of all, when
	// the image is not cached yet — and every later exec would address a name
	// made of that noise.
	var stderr strings.Builder
	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("starting %s: %v\n%s", fl.image, err, stderr.String())
	}

	s := &mysqlServer{t: t, flavor: fl, container: strings.TrimSpace(string(out))}
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "--force", s.container).Run()
	})

	s.waitReady()
	return s
}

// waitReady polls until the server accepts connections. A fresh container
// initializes its data directory first, which takes appreciably longer than the
// process taking to start.
func (s *mysqlServer) waitReady() {
	s.t.Helper()

	deadline := time.Now().Add(bootWait)
	for time.Now().Before(deadline) {
		if _, err := s.run("SELECT 1"); err == nil {
			return
		}
		time.Sleep(time.Second)
	}

	// Report what the container was actually doing. A bare timeout here says
	// nothing about whether the server was slow, dead, or never addressed at all.
	status, _ := exec.Command("docker", "ps", "--all", "--filter", "id="+s.container, "--format", "{{.Status}}").Output()
	logs, _ := exec.Command("docker", "logs", "--tail", "20", s.container).CombinedOutput()
	s.t.Fatalf("%s did not accept connections within %s\ncontainer %q status: %s\nlogs:\n%s",
		s.flavor.image, bootWait, s.container, strings.TrimSpace(string(status)), logs)
}

// with returns the same server bound to a subtest, so a failure inside one is
// reported against that subtest rather than its parent.
func (s *mysqlServer) with(t *testing.T) *mysqlServer {
	return &mysqlServer{t: t, flavor: s.flavor, container: s.container}
}

// exec runs SQL and fails the test if the server rejects it.
func (s *mysqlServer) exec(sql string) {
	s.t.Helper()
	if out, err := s.run(sql); err != nil {
		s.t.Fatalf("running %q: %v\n%s", sql, err, out)
	}
}

// query runs SQL and returns the result as rows of tab-separated columns, with
// no header.
func (s *mysqlServer) query(sql string) []string {
	s.t.Helper()

	out, err := s.run("--silent", "--batch", sql)
	if err != nil {
		s.t.Fatalf("querying %q: %v\n%s", sql, err, out)
	}
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

// run executes SQL and returns what the client wrote to stdout.
//
// Only stdout is returned. MariaDB's client warns on stderr about the
// connection being unverified, and folding that in would make every parsed
// result start with a warning line.
func (s *mysqlServer) run(args ...string) (string, error) {
	sql := args[len(args)-1]
	flags := args[:len(args)-1]

	// Connect over TCP rather than the unix socket. While the image initializes
	// its data directory it runs a temporary server that listens on the socket
	// only, and a readiness probe that reaches that one reports success moments
	// before it is shut down to make way for the real server.
	full := append([]string{
		"exec", "--env", "MYSQL_PWD=" + rootPass, s.container,
		s.flavor.client, "--user=root", "--protocol=TCP", "--host=127.0.0.1",
		"--default-character-set=utf8mb4",
	}, flags...)
	full = append(full, "--execute="+sql)

	cmd := exec.Command("docker", full...)
	var stderr strings.Builder
	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		return stderr.String(), err
	}
	return string(out), nil
}

// dockerExecScript builds the command that pipes a script into the server.
func dockerExecScript(s *mysqlServer, script string) *exec.Cmd {
	cmd := exec.Command("docker", "exec", "--interactive",
		"--env", "MYSQL_PWD="+rootPass, s.container,
		s.flavor.client, "--user=root", "--protocol=TCP", "--host=127.0.0.1",
		"--default-character-set=utf8mb4")
	cmd.Stdin = strings.NewReader(script)
	return cmd
}

// applyScript pipes a whole generated script into the server, the way an
// operator runs one after reading it. Feeding it through stdin rather than
// applying statements one by one is what exercises the session settings the
// script sets for itself.
func (s *mysqlServer) applyScript(script string) {
	s.t.Helper()

	cmd := dockerExecScript(s, script)

	if out, err := cmd.CombinedOutput(); err != nil {
		s.t.Fatalf("applying script: %v\n%s\nscript was:\n%s", err, out, script)
	}
}

// revert generates the script that undoes events and applies it.
func (s *mysqlServer) revert(t *testing.T, events []change.Event, source string) []reverse.Reversal {
	t.Helper()

	plan, err := reverse.Plan(events)
	if err != nil {
		t.Fatalf("generating the reversal: %v", err)
	}

	var script strings.Builder
	if err := cli.Render(&script, source, change.Filter{}, nil, plan); err != nil {
		t.Fatalf("rendering the script: %v", err)
	}
	t.Logf("generated script:\n%s", script.String())

	s.applyScript(script.String())
	return plan
}

// currentBinlog returns the file the server is writing to right now.
//
// MariaDB renamed the statement that reports this; MySQL 8.0 only knows the old
// spelling, so both are tried.
func (s *mysqlServer) currentBinlog() string {
	s.t.Helper()

	out, err := s.run("--silent", "--batch", "SHOW BINLOG STATUS")
	if err != nil {
		if out, err = s.run("--silent", "--batch", "SHOW MASTER STATUS"); err != nil {
			s.t.Fatalf("asking for the current binlog: %v\n%s", err, out)
		}
	}

	line, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	name, _, _ := strings.Cut(line, "\t")
	if name == "" {
		s.t.Fatalf("could not read a binlog name out of %q; is binary logging on?", out)
	}
	return name
}

// copyBinlog rotates the log so the named file is complete, then copies it out
// of the container. Reading a file the server is still appending to would risk
// a half-written event at the end.
func (s *mysqlServer) copyBinlog(name string) string {
	s.t.Helper()

	s.exec("FLUSH BINARY LOGS")

	local := filepath.Join(s.t.TempDir(), name)
	remote := s.container + ":/var/lib/mysql/" + name
	if out, err := exec.Command("docker", "cp", remote, local).CombinedOutput(); err != nil {
		s.t.Fatalf("copying %s: %v\n%s", remote, err, out)
	}
	return local
}

func requireDocker(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping acceptance test in short mode")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("skipping acceptance test: docker is not available")
	}
}

// hostAddr is where the collector reaches this server from the test process.
func (s *mysqlServer) hostAddr() string {
	s.t.Helper()

	out, err := exec.Command("docker", "port", s.container, "3306/tcp").Output()
	if err != nil {
		s.t.Fatalf("asking docker for the published port: %v", err)
	}

	// docker prints one mapping per line, e.g. "0.0.0.0:55001".
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	_, port, ok := strings.Cut(line, ":")
	if !ok {
		s.t.Fatalf("could not read a port out of %q", line)
	}
	return "127.0.0.1:" + port
}

// dsn is the connection string a collector would be given for this server.
func (s *mysqlServer) dsn() string {
	return "root:" + rootPass + "@tcp(" + s.hostAddr() + ")/"
}
