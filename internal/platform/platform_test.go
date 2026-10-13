package platform

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestNextMinuteTick(t *testing.T) {
	base := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		now  time.Time
		want time.Time
	}{
		{base, base.Add(5 * time.Second)},
		{base.Add(3 * time.Second), base.Add(5 * time.Second)},
		{base.Add(5 * time.Second), base.Add(time.Minute + 5*time.Second)},
		{base.Add(59 * time.Second), base.Add(time.Minute + 5*time.Second)},
	}
	for _, tt := range tests {
		if got := NextMinuteTick(tt.now, 5*time.Second); !got.Equal(tt.want) {
			t.Errorf("NextMinuteTick(%s) = %s, verwacht %s", tt.now.Format("15:04:05"), got.Format("15:04:05"), tt.want.Format("15:04:05"))
		}
	}
}

func TestSystemClockIsUTC(t *testing.T) {
	if loc := (SystemClock{}).Now().Location(); loc != time.UTC {
		t.Errorf("locatie = %s, verwacht UTC", loc)
	}
}

func TestLoadOrCreateToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "api_token")

	first, created, err := LoadOrCreateToken(path)
	if err != nil || !created || len(first) != 64 {
		t.Fatalf("aanmaken: %q, %v, %v", first, created, err)
	}
	if info, _ := os.Stat(path); runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("rechten = %v, verwacht 0600", info.Mode().Perm())
	}

	second, created, err := LoadOrCreateToken(path)
	if err != nil || created || second != first {
		t.Errorf("tweede keer: %q, %v, %v; verwacht hetzelfde token", second, created, err)
	}

	os.WriteFile(path, []byte("kort\n"), 0o600)
	if _, _, err := LoadOrCreateToken(path); err == nil {
		t.Error("verwacht fout bij een te kort token")
	}
}
