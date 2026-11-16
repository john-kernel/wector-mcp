package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"wector-mcp/utils"
)

const (
	sessionPrefix = "session-"
	sessionSuffix = ".db"
)

// session owns the URL store for the lifetime of one MCP session.
type session struct {
	store *utils.SQLiteStore
	path  string
	// owned is true when the database file was created for this process and
	// is deleted at the end; false when the user chose it with $WECTOR_DB,
	// in which case it is emptied but left in place.
	owned bool
}

// openSession opens an empty URL store.
//
// By default every process gets its own database file under
// <user cache dir>/wector, so concurrent sessions cannot flush each other's
// data. Files left behind by sessions that were killed are removed here.
// With $WECTOR_DB set, that file is used instead and emptied on open.
func openSession() (*session, error) {
	if path := os.Getenv("WECTOR_DB"); path != "" {
		store, err := utils.NewSQLiteStore(path)
		if err != nil {
			return nil, err
		}
		if err := store.Flush(context.Background()); err != nil {
			store.Close()
			return nil, err
		}
		return &session{store: store, path: path}, nil
	}

	cache, err := os.UserCacheDir()
	if err != nil {
		return nil, fmt.Errorf("resolve cache dir: %w", err)
	}
	dir := filepath.Join(cache, "wector")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	removeStaleSessions(dir)

	path := filepath.Join(dir, sessionPrefix+strconv.Itoa(os.Getpid())+sessionSuffix)
	removeDB(path) // a previous process with a recycled pid
	store, err := utils.NewSQLiteStore(path)
	if err != nil {
		return nil, err
	}
	return &session{store: store, path: path, owned: true}, nil
}

// end flushes the URL store: an owned database file is deleted, a
// user-chosen one is emptied.
func (s *session) end() error {
	if s.owned {
		err := s.store.Close()
		return errors.Join(err, removeDB(s.path))
	}
	err := s.store.Flush(context.Background())
	return errors.Join(err, s.store.Close())
}

// removeDB deletes a SQLite database file and its WAL companions.
func removeDB(path string) error {
	var errs []error
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// removeStaleSessions deletes session databases in dir whose process is no
// longer running. Failures are ignored; the files are only wasted space.
func removeStaleSessions(dir string) {
	paths, _ := filepath.Glob(filepath.Join(dir, sessionPrefix+"*"+sessionSuffix))
	for _, path := range paths {
		name := filepath.Base(path)
		pid, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, sessionPrefix), sessionSuffix))
		if err != nil || processAlive(pid) {
			continue
		}
		removeDB(path)
	}
}

// processAlive reports whether a process with the given pid exists.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, os.ErrPermission)
}
