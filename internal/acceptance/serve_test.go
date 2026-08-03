package acceptance

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The v0.1 bar, reached over HTTP: a script downloaded from the API restores the
// database exactly. Everything else here is about the API refusing rather than
// answering, which is the part that only shows against a real collector.
func TestServeAnswersFromALiveStore(t *testing.T) {
	s := startMySQL(t)
	bin := buildMulligan(t)

	s.exec("CREATE DATABASE shop")
	s.exec(schema)
	s.exec(seed)

	before := s.query(snapshot)

	path := filepath.Join(t.TempDir(), "mulligan.db")
	watching := startWatch(t, bin, s, path, "4300", "30s")

	// The accident.
	s.exec("UPDATE shop.orders SET status = 'shipped'")
	s.exec("DELETE FROM shop.orders WHERE id = 3")

	damaged := s.query(snapshot)
	if reflect.DeepEqual(before, damaged) {
		t.Fatal("the workload changed nothing, so there is nothing to test")
	}
	time.Sleep(3 * time.Second)

	base, stopServing := startServe(t, bin, path, "127.0.0.1:0", "")
	defer stopServing()

	t.Run("the timeline lists the changes newest first", func(t *testing.T) {
		var page struct {
			Changes []struct {
				ID    int64  `json:"id"`
				Op    string `json:"op"`
				Table string `json:"table"`
				At    string `json:"at"`
			} `json:"changes"`
			Next *int64 `json:"next"`
		}
		getJSON(t, base+"/api/changes", http.StatusOK, &page)

		// Three, not four: the seed already has one row at 'shipped', and MySQL
		// logs nothing for an update that changes no value.
		if len(page.Changes) != 3 {
			t.Fatalf("timeline holds %d changes, want 3 (two rows updated, one deleted)", len(page.Changes))
		}
		if page.Changes[0].Op != "DELETE" {
			t.Errorf("newest change is %s, want the DELETE that happened last", page.Changes[0].Op)
		}
		for i := 1; i < len(page.Changes); i++ {
			if page.Changes[i].ID >= page.Changes[i-1].ID {
				t.Fatalf("timeline is not newest first: %v", page.Changes)
			}
		}
	})

	t.Run("paging walks every change exactly once", func(t *testing.T) {
		seen := map[int64]bool{}
		target := base + "/api/changes?limit=1"

		for round := 0; round < 10; round++ {
			var page struct {
				Changes []struct {
					ID int64 `json:"id"`
				} `json:"changes"`
				Next *int64 `json:"next"`
			}
			getJSON(t, target, http.StatusOK, &page)

			for _, c := range page.Changes {
				if seen[c.ID] {
					t.Fatalf("change %d appeared twice while paging", c.ID)
				}
				seen[c.ID] = true
			}
			if page.Next == nil {
				break
			}
			target = fmt.Sprintf("%s/api/changes?limit=1&before=%d", base, *page.Next)
		}

		if len(seen) != 3 {
			t.Errorf("paging saw %d changes, want 3", len(seen))
		}
	})

	t.Run("the detail view carries both row images", func(t *testing.T) {
		var page struct {
			Changes []struct {
				ID int64 `json:"id"`
			} `json:"changes"`
		}
		getJSON(t, base+"/api/changes?tables=shop.orders&limit=1", http.StatusOK, &page)

		var detail struct {
			Op      string `json:"op"`
			Columns []struct {
				Name       string `json:"name"`
				PrimaryKey bool   `json:"primary_key"`
			} `json:"columns"`
			Before []*string `json:"before"`
			After  []*string `json:"after"`
		}
		getJSON(t, fmt.Sprintf("%s/api/changes/%d", base, page.Changes[0].ID), http.StatusOK, &detail)

		if len(detail.Columns) != 5 {
			t.Fatalf("detail lists %d columns, want the table's 5", len(detail.Columns))
		}
		if !detail.Columns[0].PrimaryKey {
			t.Errorf("detail does not mark the primary key: %+v", detail.Columns)
		}
		if detail.Before == nil {
			t.Fatal("detail carries no before image")
		}
		// The decimal is the value a float would round, and the preview is what
		// someone approves.
		if detail.Before[3] == nil || !strings.Contains(*detail.Before[3], ".") {
			t.Errorf("before image does not carry the decimal intact: %v", detail.Before)
		}
	})

	t.Run("the downloaded script restores the database exactly", func(t *testing.T) {
		body, status := getBody(t, base+"/api/revert.sql")
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200\n%s", status, body)
		}
		if !strings.Contains(body, "-- end of script:") {
			t.Fatalf("script has no completion trailer:\n%s", body)
		}

		s.applyScript(body)

		if restored := s.query(snapshot); !reflect.DeepEqual(restored, before) {
			t.Errorf("the database was not restored\n before: %v\n  after: %v", before, restored)
		}
	})

	// The refusal that matters. A dead collector and a quiet database look the
	// same from outside the store, and an empty timeline during an incident reads
	// as "nothing happened".
	t.Run("a stalled collector turns the timeline into a refusal", func(t *testing.T) {
		if err := watching.Process.Signal(syscall.SIGKILL); err != nil {
			t.Fatalf("killing watch: %v", err)
		}
		_ = watching.Wait()

		time.Sleep(35 * time.Second)

		body, status := getBody(t, base+"/api/changes")
		if status != http.StatusConflict {
			t.Fatalf("status = %d, want 409 once the collector stopped\n%s", status, body)
		}
		if !strings.Contains(body, "stale") {
			t.Errorf("refusal does not say why:\n%s", body)
		}

		// One stored change is still complete, so it is still served.
		if _, status := getBody(t, base+"/api/status"); status != http.StatusOK {
			t.Errorf("status route = %d while stalled, want 200", status)
		}
	})
}

// Binding beyond loopback publishes an unencrypted partial copy of the tables,
// so it has to be refused rather than merely discouraged.
func TestServeRefusesToPublishWithoutAToken(t *testing.T) {
	s := startMySQL(t)
	bin := buildMulligan(t)

	s.exec("CREATE DATABASE shop")
	s.exec(schema)
	s.exec(seed)

	path := filepath.Join(t.TempDir(), "mulligan.db")
	watching := startWatch(t, bin, s, path, "4301", "5m")
	t.Cleanup(func() {
		_ = watching.Process.Signal(syscall.SIGTERM)
		_ = watching.Wait()
	})
	time.Sleep(3 * time.Second)

	cmd := exec.Command(bin, "serve", "-store", path, "-listen", "0.0.0.0:0")
	cmd.Env = append(os.Environ(), "MULLIGAN_TOKEN=")

	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("serve bound a public address with no token:\n%s", out)
	}
	if !strings.Contains(string(out), "MULLIGAN_TOKEN") {
		t.Errorf("refusal does not say how to allow it:\n%s", out)
	}
}

// startWatch runs a collector against s, writing to path.
func startWatch(t *testing.T, bin string, s *mysqlServer, path, serverID, staleness string) *exec.Cmd {
	t.Helper()

	cmd := exec.Command(bin, "watch",
		"-store", path,
		"-server-id", serverID,
		"-max-staleness", staleness)
	cmd.Env = append(os.Environ(), "MULLIGAN_DSN="+s.dsn())

	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting watch: %v", err)
	}

	// Attach before there is anything to collect, so the workload lands while it
	// is streaming rather than before it connects.
	time.Sleep(2 * time.Second)
	return cmd
}

var servingAddr = regexp.MustCompile(`on (http://[^\s]+)`)

// startServe runs the API and returns its base URL.
func startServe(t *testing.T, bin, path, listen, token string) (base string, stop func()) {
	t.Helper()

	args := []string{"serve", "-store", path, "-listen", listen}
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "MULLIGAN_TOKEN="+token)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("piping serve stdout: %v", err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("starting serve: %v", err)
	}
	stop = func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
	}

	// The listener reports the address it actually bound, which is the only way to
	// learn the port when the test asked for an ephemeral one.
	line := make([]byte, 512)
	n, err := stdout.Read(line)
	if err != nil {
		stop()
		t.Fatalf("reading the serve banner: %v", err)
	}
	found := servingAddr.FindStringSubmatch(string(line[:n]))
	if found == nil {
		stop()
		t.Fatalf("serve did not report its address: %q", string(line[:n]))
	}
	return found[1], stop
}

func getBody(t *testing.T, url string) (string, int) {
	t.Helper()

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading %s: %v", url, err)
	}
	return string(body), resp.StatusCode
}

func getJSON(t *testing.T, url string, wantStatus int, into any) {
	t.Helper()

	body, status := getBody(t, url)
	if status != wantStatus {
		t.Fatalf("GET %s = %d, want %d\n%s", url, status, wantStatus, body)
	}
	if err := json.Unmarshal([]byte(body), into); err != nil {
		t.Fatalf("GET %s did not return JSON: %v\n%s", url, err, body)
	}
}
