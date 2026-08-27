package indexer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type fileLock struct {
	file *os.File
}

func AcquireLock(path string) (*fileLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create index lock directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open index lock: %w", err)
	}
	if err := lockFile(file); err != nil {
		file.Close()
		if isLockBusy(err) {
			return nil, fmt.Errorf("another indexing process is already running")
		}
		return nil, fmt.Errorf("lock index: %w", err)
	}
	return &fileLock{file: file}, nil
}

func (l *fileLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := unlockFile(l.file)
	closeErr := l.file.Close()
	return errors.Join(unlockErr, closeErr)
}
