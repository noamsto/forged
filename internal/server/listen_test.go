package server

import (
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "fc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

func lookupFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestListenFromEnvNilWhenNotActivated(t *testing.T) {
	cases := map[string]map[string]string{
		"pid mismatch": {"LISTEN_PID": "999", "LISTEN_FDS": "1"},
		"fds unset":    {"LISTEN_PID": "42"},
		"nothing set":  {},
	}
	for name, vars := range cases {
		t.Run(name, func(t *testing.T) {
			ln, err := listenFromEnv(lookupFrom(vars), 42)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if ln != nil {
				_ = ln.Close()
				t.Fatal("listener = non-nil, want nil")
			}
		})
	}
}

func TestDefaultSocketPath(t *testing.T) {
	got, err := DefaultSocketPath(lookupFrom(map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000"}))
	if err != nil {
		t.Fatal(err)
	}
	if want := "/run/user/1000/forged/forged.sock"; got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
	if _, err := DefaultSocketPath(lookupFrom(nil)); err == nil {
		t.Fatal("err = nil with XDG_RUNTIME_DIR unset")
	}
}

func TestListenCreatesDirPrivate(t *testing.T) {
	dir := filepath.Join(shortDir(t), "sub")
	ln, err := Listen(filepath.Join(dir, "s.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Fatalf("dir perm = %o, want 700", perm)
	}
}

func TestListenRemovesStaleSocket(t *testing.T) {
	path := filepath.Join(shortDir(t), "s.sock")
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	_ = stale.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stale socket file missing: %v", err)
	}

	ln, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen over stale socket: %v", err)
	}
	_ = ln.Close()
}

func TestListenRefusesLiveSocket(t *testing.T) {
	path := filepath.Join(shortDir(t), "s.sock")
	live, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = live.Close() }()
	go func() {
		for {
			c, err := live.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	ln, err := Listen(path)
	if err == nil {
		_ = ln.Close()
		t.Fatal("second Listen succeeded against a live listener")
	}
}

func TestListenRefusesNonSocketPath(t *testing.T) {
	path := filepath.Join(shortDir(t), "s.sock")
	if err := os.WriteFile(path, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := Listen(path)
	if err == nil {
		_ = ln.Close()
		t.Fatal("Listen replaced a regular file")
	}
	if b, rerr := os.ReadFile(path); rerr != nil || string(b) != "precious" {
		t.Fatalf("file after Listen = %q, %v", b, rerr)
	}
}

// Not parallel: the process umask is global.
func TestListenSocketModeIs0600(t *testing.T) {
	old := syscall.Umask(0o002)
	t.Cleanup(func() { syscall.Umask(old) })
	path := filepath.Join(shortDir(t), "s.sock")
	ln, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket perm = %o, want 600", perm)
	}
}
