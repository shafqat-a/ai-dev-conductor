package session

import (
	"os"
	"path/filepath"
)

func OpenHistoryFile(dataDir, sessionID string) (*os.File, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, sessionID+".log")
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

func ReadHistory(dataDir, sessionID string) ([]byte, error) {
	path := filepath.Join(dataDir, sessionID+".log")
	return os.ReadFile(path)
}

func RemoveHistory(dataDir, sessionID string) {
	path := filepath.Join(dataDir, sessionID+".log")
	os.Remove(path)
}
