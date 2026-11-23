package server

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// DefaultSocketPath is $XDG_RUNTIME_DIR/forged/forged.sock.
func DefaultSocketPath(env func(string) string) (string, error) {
	dir := env("XDG_RUNTIME_DIR")
	if dir == "" {
		return "", errors.New("XDG_RUNTIME_DIR is not set; pass --socket")
	}
	return filepath.Join(dir, "forged", "forged.sock"), nil
}

// ListenFromEnv returns the systemd-activated listener, or nil when the
// process was not socket-activated. It unsets the LISTEN_* variables.
func ListenFromEnv() (net.Listener, error) {
	defer func() {
		_ = os.Unsetenv("LISTEN_PID")
		_ = os.Unsetenv("LISTEN_FDS")
		_ = os.Unsetenv("LISTEN_FDNAMES")
	}()
	return listenFromEnv(os.Getenv, os.Getpid())
}

func listenFromEnv(env func(string) string, pid int) (net.Listener, error) {
	if env("LISTEN_PID") != strconv.Itoa(pid) {
		return nil, nil
	}
	if n, err := strconv.Atoi(env("LISTEN_FDS")); err != nil || n < 1 {
		return nil, nil
	}
	f := os.NewFile(3, "LISTEN_FD_3")
	defer func() { _ = f.Close() }()
	return net.FileListener(f)
}

// Listen binds a private unix socket at path, replacing a stale socket file
// and refusing when another process answers on it.
func Listen(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
		_ = c.Close()
		return nil, fmt.Errorf("socket %s is already in use", path)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return net.Listen("unix", path)
}
