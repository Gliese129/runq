package utils

import (
	"os"
	"path/filepath"
)

func AtomicWriteFile(filename string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(filename)
	temp, err := os.CreateTemp(dir, filename+".tmp")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer func() {
		if err != nil {
			temp.Close()
			os.Remove(tempName)
		}
	}()
	if err := os.WriteFile(tempName, data, perm); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, filename)
}
