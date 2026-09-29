package depengine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const stateFileName = "state.json"

func (e *Engine) statePath() string {
	return filepath.Join(e.stateDir, stateFileName)
}

func (e *Engine) loadState() (stateFile, error) {
	state := stateFile{Version: 1, Tasks: map[string]record{}}
	data, err := os.ReadFile(e.statePath())
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return state, fmt.Errorf("read build state: %w", err)
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return stateFile{Version: 1, Tasks: map[string]record{}}, nil
	}
	if state.Version != 1 || state.Tasks == nil {
		return stateFile{Version: 1, Tasks: map[string]record{}}, nil
	}
	return state, nil
}

func (e *Engine) saveState(state stateFile) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(e.stateDir, ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("write build state: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write build state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("write build state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write build state: %w", err)
	}
	if err := os.Rename(tmpName, e.statePath()); err != nil {
		return fmt.Errorf("write build state: %w", err)
	}
	return nil
}
