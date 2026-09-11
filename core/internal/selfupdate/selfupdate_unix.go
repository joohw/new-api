//go:build !windows

package selfupdate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func installBinary(data []byte, targetPath, execPath, version string) error {
	_ = execPath
	targetPath = strings.TrimSpace(targetPath)
	if targetPath == "" {
		return fmt.Errorf("target path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(targetPath), ".clovapi-update-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, targetPath); err != nil {
		_ = os.Remove(targetPath)
		if err2 := os.Rename(tmpName, targetPath); err2 != nil {
			cleanup()
			return err
		}
	}
	return writeVersionMeta(version)
}
