package utils

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func AtomicWriteFile(filename string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(filename)
	temp, err := os.CreateTemp(dir, filepath.Base(filename)+".tmp-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			temp.Close()
			os.Remove(tempName)
		}
	}()
	if err := temp.Chmod(perm); err != nil {
		return err
	}
	if _, err := temp.Write(data); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempName, filename); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// LockFile LockPidFile flock a file (usually for pid file lock)
func LockFile(filename string) (*os.File, error) {
	file, err := os.OpenFile(filename, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, fmt.Errorf("cannot open pid file: %v", err)
	}
	err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) // nonblock & exclusive lock
	if err != nil {
		return nil, fmt.Errorf("daemon is already started")
	}
	file.Truncate(0)
	return file, nil
}
