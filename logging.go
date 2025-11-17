package main

import (
	"bytes"
	"log"
	"os"

	"github.com/coreos/go-systemd/v22/journal"
)

// journalWriter sends log lines to systemd-journald using go-systemd.
// It sets SYSLOG_IDENTIFIER to help filter via journalctl.
// Logging is unconditional: messages are always sent to journald.
type journalWriter struct {
	identifier string
}

func (jw journalWriter) Write(p []byte) (int, error) {
	n := len(p)
	// Split into lines to avoid merging multiple log lines into one journal entry
	for len(p) > 0 {
		idx := bytes.IndexByte(p, '\n')
		var line []byte
		if idx == -1 {
			line = p
			p = nil
		} else {
			line = p[:idx]
			p = p[idx+1:]
		}
		if len(line) == 0 {
			continue
		}
		_ = journal.Send(string(line), journal.PriInfo, map[string]string{
			"SYSLOG_IDENTIFIER": jw.identifier,
		})
	}
	return n, nil
}

// newAppLogger creates the application logger.
// If configuration.logs.journal is set, it will be used as SYSLOG_IDENTIFIER
// to make it easy to filter via journalctl. Otherwise, the process name is used.
// Logs are emitted directly to systemd-journald with no fallback.
func newAppLogger(cfg *Config) *log.Logger {
	identifier := os.Args[0]
	if cfg.Configuration.Logs.Journal != "" {
		identifier = cfg.Configuration.Logs.Journal
	}
	jw := journalWriter{identifier: identifier}
	cfg.log = log.New(jw, "", 0)
	return cfg.log
}
