package dispatcher

import (
	"io"
	"log/slog"
)

// discardLogger returns a slog.Logger that drops everything — used to keep
// dispatcher tests quiet without changing the production default.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
