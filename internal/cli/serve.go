package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/noamsto/forge-cache/forge"
	"github.com/noamsto/forge-cache/internal/cache"
	"github.com/noamsto/forge-cache/internal/github"
	"github.com/noamsto/forge-cache/internal/server"
)

const (
	snapshotEvery   = 60 * time.Second
	shutdownTimeout = 5 * time.Second
)

// Test seams.
var (
	newProviders = func() map[string]forge.Provider {
		return map[string]forge.Provider{"github/github.com": github.New(github.Options{})}
	}
	serveParent = context.Background()
)

func serve(args []string, stderr io.Writer) int {
	opts := cache.DefaultOptions()
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	socket := fs.String("socket", "", "unix socket `path` (default $XDG_RUNTIME_DIR/forge-cache/forge-cache.sock)")
	snapshot := fs.String("snapshot", "", "snapshot file `path` (default $XDG_STATE_HOME/forge-cache/snapshot.json)")
	fs.DurationVar(&opts.LeaseTTL, "lease-ttl", opts.LeaseTTL, "how long a read keeps an entry polled")
	fs.DurationVar(&opts.HotInterval, "hot-interval", opts.HotInterval, "poll interval for hot entries")
	fs.DurationVar(&opts.WarmInterval, "warm-interval", opts.WarmInterval, "probe interval for warm entries")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))

	ln, owned, err := listen(*socket)
	if err != nil {
		log.Error("listen", "err", err)
		return 1
	}
	snapPath := *snapshot
	if snapPath == "" {
		if snapPath, err = defaultSnapshotPath(); err != nil {
			log.Error("snapshot path", "err", err)
			return 1
		}
	}

	c := cache.New(newProviders(), opts)
	if err := c.LoadSnapshot(snapPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Warn("ignoring snapshot", "err", err)
	}

	ctx, stop := signal.NotifyContext(serveParent, os.Interrupt, syscall.SIGTERM)
	defer stop()
	go c.Run(ctx)
	go func() {
		t := time.NewTicker(snapshotEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := c.SaveSnapshot(snapPath); err != nil {
					log.Warn("save snapshot", "err", err)
				}
			}
		}
	}()

	srv := &http.Server{Handler: server.New(c, version), ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	log.Info("listening", "addr", ln.Addr().String())
	go func() { serveErr <- srv.Serve(ln) }()

	code := 0
	select {
	case <-ctx.Done():
	case err := <-serveErr:
		log.Error("serve", "err", err)
		code = 1
	}

	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		log.Warn("shutdown", "err", err)
	}
	if err := c.SaveSnapshot(snapPath); err != nil {
		log.Error("save snapshot", "err", err)
		code = 1
	}
	if owned {
		_ = os.Remove(ln.Addr().String())
	}
	return code
}

// listen prefers a socket-activated listener, then --socket, then the default
// path. owned reports whether this process created the socket file.
func listen(socket string) (ln net.Listener, owned bool, err error) {
	if ln, err = server.ListenFromEnv(); ln != nil || err != nil {
		return ln, false, err
	}
	if socket == "" {
		if socket, err = server.DefaultSocketPath(os.Getenv); err != nil {
			return nil, false, err
		}
	}
	ln, err = server.Listen(socket)
	return ln, err == nil, err
}

func defaultSnapshotPath() (string, error) {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "forge-cache", "snapshot.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("snapshot path: %w", err)
	}
	return filepath.Join(home, ".local", "state", "forge-cache", "snapshot.json"), nil
}
