//go:build windows

package main

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

var errAgentAlreadyRunning = errors.New("another Meridian Agent process already owns this state")

func acquireAgentInstanceLock(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- administrator-selected local Agent state path.
	if err != nil {
		return nil, err
	}
	var overlapped windows.Overlapped
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK | windows.LOCKFILE_FAIL_IMMEDIATELY)
	if err := windows.LockFileEx(windows.Handle(file.Fd()), flags, 0, 1, 0, &overlapped); err != nil {
		_ = file.Close()
		// Only a lock violation means another agent holds the file. With
		// LOCKFILE_FAIL_IMMEDIATELY an asynchronous completion (IO_PENDING)
		// is not expected; reporting it as "already running" would mask the
		// real failure.
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, errAgentAlreadyRunning
		}
		return nil, err
	}
	return func() {
		_ = windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &overlapped)
		_ = file.Close()
	}, nil
}
