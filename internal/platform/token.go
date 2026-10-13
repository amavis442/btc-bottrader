package platform

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LoadOrCreateToken leest een token uit path, of maakt een nieuw willekeurig token aan en slaat het
// daar op (alleen leesbaar voor de eigenaar). created geeft aan of het token nieuw is.
func LoadOrCreateToken(path string) (token string, created bool, err error) {
	data, err := os.ReadFile(path)
	if err == nil {
		if token = strings.TrimSpace(string(data)); len(token) >= 16 {
			return token, false, nil
		}
		return "", false, fmt.Errorf("token in %s is te kort; verwijder het bestand om een nieuw te laten maken", path)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", false, fmt.Errorf("token lezen: %w", err)
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", false, err
	}
	token = hex.EncodeToString(raw)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", false, fmt.Errorf("token opslaan: %w", err)
	}
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", false, fmt.Errorf("token opslaan: %w", err)
	}
	return token, true, nil
}
