package cache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/noamsto/forge-cache/forge"
)

const snapshotVersion = 1

type snapshot struct {
	Version int          `json:"version"`
	Changes []snapChange `json:"changes"`
	Lists   []snapList   `json:"lists"`
}

type snapChange struct {
	Key       forge.Key    `json:"key"`
	Value     forge.Change `json:"value"`
	OK        bool         `json:"ok"`
	FetchedAt time.Time    `json:"fetchedAt"`
}

type snapList struct {
	Key       forge.Key         `json:"key"`
	Refs      []forge.ChangeRef `json:"refs"`
	FetchedAt time.Time         `json:"fetchedAt"`
}

// SaveSnapshot writes every fetched entry to path through a temp file and rename.
func (c *Cache) SaveSnapshot(path string) error {
	snap := snapshot{Version: snapshotVersion}
	c.mu.Lock()
	for k, e := range c.changes {
		if !e.fetchedAt.IsZero() {
			snap.Changes = append(snap.Changes, snapChange{Key: k, Value: e.val, OK: e.ok, FetchedAt: e.fetchedAt})
		}
	}
	for k, e := range c.lists {
		if !e.listFetchedAt.IsZero() {
			snap.Lists = append(snap.Lists, snapList{Key: k, Refs: e.refs, FetchedAt: e.listFetchedAt})
		}
	}
	data, err := json.Marshal(snap)
	c.mu.Unlock()
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}

// LoadSnapshot adds the entries of the snapshot at path that the cache does not
// hold yet. They keep their original fetch time and stay cold until read.
func (c *Cache) LoadSnapshot(path string) error {
	data, err := os.ReadFile(path) //nolint:gosec // path is the operator-chosen snapshot location
	if err != nil {
		return err
	}
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return fmt.Errorf("snapshot %s: %w", path, err)
	}
	if snap.Version != snapshotVersion {
		return fmt.Errorf("snapshot %s: unsupported version %d", path, snap.Version)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.o.Clock.Now()
	for _, sc := range snap.Changes {
		if c.changes[sc.Key] == nil {
			c.changes[sc.Key] = &changeEntry{val: sc.Value, ok: sc.OK, fetchedAt: sc.FetchedAt, fullAt: sc.FetchedAt, loadedAt: now}
		}
	}
	for _, sl := range snap.Lists {
		if c.lists[sl.Key] == nil {
			c.lists[sl.Key] = &listEntry{refs: sl.Refs, listFetchedAt: sl.FetchedAt, fullAt: sl.FetchedAt, loadedAt: now}
		}
	}
	return nil
}
