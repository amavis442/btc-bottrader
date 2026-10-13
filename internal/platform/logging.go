package platform

import (
	"io"
	"log/slog"
	"os"

	"gopkg.in/natefinch/lumberjack.v2"
)

// NewLogger maakt een logger die naar de console en (als file niet leeg is) naar een roterend
// logbestand schrijft. De closer sluit het logbestand.
func NewLogger(level, file string) (*slog.Logger, io.Closer) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}

	var out io.Writer = os.Stdout
	var closer io.Closer = io.NopCloser(nil)
	if file != "" {
		rotating := &lumberjack.Logger{Filename: file, MaxSize: 20, MaxBackups: 10, MaxAge: 90}
		out = io.MultiWriter(os.Stdout, rotating)
		closer = rotating
	}
	return slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: lvl})), closer
}
