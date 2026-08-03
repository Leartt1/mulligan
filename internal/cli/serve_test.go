package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

// These exercise checkServe rather than the command, deliberately. A refusal
// tested through serve itself could only be observed as a server failing to
// start, so removing the refusal would make the test hang instead of fail.

func TestServeRequiresAStore(t *testing.T) {
	err := checkServe("", defaultListen, "")
	if err == nil {
		t.Fatal("serve accepted no -store")
	}
	if !strings.Contains(err.Error(), "-store") {
		t.Errorf("error does not name the missing flag: %v", err)
	}
}

// Opening a store creates it, and serving an empty store that a typo brought
// into existence is worse here than at the command line: it stays up.
func TestServeRefusesAStoreThatDoesNotExist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.db")

	err := checkServe(path, defaultListen, "")
	if err == nil {
		t.Fatal("serve accepted a store that does not exist")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error does not name the store: %v", err)
	}
	if fileExists(path) {
		t.Error("the check created the store file")
	}
}

// The store is an unencrypted partial copy of production rows. Binding it
// somewhere reachable is a decision, not a default, and a typo in -listen must
// not be the thing that publishes them.
func TestServeRefusesANonLoopbackListenWithoutAToken(t *testing.T) {
	path, _ := seedStore(t, 0)

	for _, listen := range []string{"0.0.0.0:8080", ":8080", "192.0.2.7:8080", "db.internal:8080"} {
		err := checkServe(path, listen, "")
		if err == nil {
			t.Errorf("%s: accepted without a token", listen)
			continue
		}
		if !strings.Contains(err.Error(), "MULLIGAN_TOKEN") {
			t.Errorf("%s: error does not say how to allow it: %v", listen, err)
		}
	}
}

// With a token, the same address is allowed: that is what the token is for.
func TestServeAllowsANonLoopbackListenWithAToken(t *testing.T) {
	path, _ := seedStore(t, 0)

	if err := checkServe(path, "0.0.0.0:8080", "s3cret"); err != nil {
		t.Errorf("a token did not permit binding beyond loopback: %v", err)
	}
}

func TestServeAllowsLoopbackWithoutAToken(t *testing.T) {
	path, _ := seedStore(t, 0)

	for _, listen := range []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080"} {
		if err := checkServe(path, listen, ""); err != nil {
			t.Errorf("%s: refused a loopback listener: %v", listen, err)
		}
	}
}

func TestServeRejectsAnAddressWithoutAPort(t *testing.T) {
	path, _ := seedStore(t, 0)

	err := checkServe(path, "not-an-address", "")
	if err == nil {
		t.Fatal("serve accepted an address with no port")
	}
	if !strings.Contains(err.Error(), "not-an-address") {
		t.Errorf("error does not quote the address: %v", err)
	}
}
