package depengine

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func (e *Engine) publish(task *Task, outputs map[string][]byte) (map[string]string, error) {
	if len(outputs) != len(task.Outputs) {
		return nil, fmt.Errorf("returned %d outputs, expected %d", len(outputs), len(task.Outputs))
	}
	declared := make(map[string]bool, len(task.Outputs))
	for _, path := range task.Outputs {
		clean, err := cleanRootPath(path)
		if err != nil {
			return nil, err
		}
		declared[clean] = true
	}

	stage, err := os.MkdirTemp(e.stateDir, "stage-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stage)

	staged := make(map[string]string, len(task.Outputs))
	hashes := make(map[string]string, len(task.Outputs))
	for rawPath, data := range outputs {
		path, err := cleanRootPath(rawPath)
		if err != nil {
			return nil, err
		}
		if !declared[path] {
			return nil, fmt.Errorf("returned undeclared output %q", path)
		}
		stagePath := filepath.Join(stage, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(stagePath), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(stagePath, data, 0o644); err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		hashes[path] = hex.EncodeToString(sum[:])
		staged[path] = stagePath
	}
	for _, path := range task.Outputs {
		clean, _ := cleanRootPath(path)
		if _, ok := staged[clean]; !ok {
			return nil, fmt.Errorf("missing declared output %q", clean)
		}
	}

	type replacement struct {
		path       string
		backup     string
		backupUsed bool
	}
	var replacements []replacement
	rollback := func() {
		for i := len(replacements) - 1; i >= 0; i-- {
			r := replacements[i]
			if r.backupUsed {
				_ = os.Rename(r.backup, e.rootPath(r.path))
			} else {
				_ = os.Remove(e.rootPath(r.path))
				_ = os.Rename(r.backup, r.backup)
			}
		}
	}

	backupDir, err := os.MkdirTemp(e.stateDir, "backup-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(backupDir)

	for _, path := range task.Outputs {
		clean, _ := cleanRootPath(path)
		target := e.rootPath(clean)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			rollback()
			return nil, err
		}
		backup := filepath.Join(backupDir, filepath.Base(clean)+"-"+hex.EncodeToString([]byte(clean))[:12])
		r := replacement{path: clean, backup: backup}
		if _, err := os.Lstat(target); err == nil {
			if err := os.Rename(target, backup); err != nil {
				rollback()
				return nil, err
			}
			r.backupUsed = true
		} else if !os.IsNotExist(err) {
			rollback()
			return nil, err
		}
		if err := os.Rename(staged[clean], target); err != nil {
			if r.backupUsed {
				_ = os.Rename(backup, target)
			}
			rollback()
			return nil, err
		}
		replacements = append(replacements, r)
	}
	if err := syncTree(e.root); err != nil {
		rollback()
		return nil, err
	}
	return hashes, nil
}

func syncTree(root string) error {
	dir, err := os.Open(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
