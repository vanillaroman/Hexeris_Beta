package main

// The summary status of /healthz. No Postgres needed: healthChecks is a pure
// function of the counters and backup state.

import (
	"errors"
	"testing"
	"time"
)

func resetHealthState(t *testing.T) {
	t.Helper()
	statMsgBatchPanics.Store(0)
	statSaveTimeouts.Store(0)
	backupState.mu.Lock()
	backupState.lastAt = time.Time{}
	backupState.lastOK = false
	backupState.lastErr = ""
	backupState.offsiteAt = time.Time{}
	backupState.mu.Unlock()
	t.Setenv("DB_BACKUP_ENABLED", "")
}

func TestHealthDownWhenDBUnreachable(t *testing.T) {
	resetHealthState(t)
	checks, status := healthChecks(errors.New("connection refused"))
	if status != healthDown {
		t.Fatalf("status=%q, want %q", status, healthDown)
	}
	// With the database down the other checks are pointless.
	if _, ok := checks["backup"]; ok {
		t.Fatal("no component breakdown expected when the database is down")
	}
}

func TestHealthOKWhenQuiet(t *testing.T) {
	resetHealthState(t)
	_, status := healthChecks(nil)
	if status != healthOK {
		t.Fatalf("status=%q, want %q", status, healthOK)
	}
}

// The property under test: the service answers while quietly broken. Without
// a degraded state such failures are invisible — an outage is noticed at
// once, a silent backup failure on the day it is needed.
func TestHealthDegradedOnSilentFailures(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T)
		field string
	}{
		{"writer panic", func(t *testing.T) { statMsgBatchPanics.Store(1) }, "message_writer"},
		{"save timeouts", func(t *testing.T) { statSaveTimeouts.Store(5) }, "message_writer"},
		{"backup failed", func(t *testing.T) {
			t.Setenv("DB_BACKUP_ENABLED", "true")
			backupState.record("", errors.New("pg_dump not found"))
		}, "backup"},
		{"backup stale", func(t *testing.T) {
			t.Setenv("DB_BACKUP_ENABLED", "true")
			t.Setenv("DB_BACKUP_INTERVAL_HOURS", "6")
			backupState.record("/tmp/x.sql.gz", nil)
			backupState.mu.Lock()
			backupState.lastAt = time.Now().Add(-48 * time.Hour) // three times the window
			backupState.mu.Unlock()
		}, "backup"},
		{"off-site failed", func(t *testing.T) {
			t.Setenv("DB_BACKUP_ENABLED", "true")
			backupState.record("/tmp/x.sql.gz", nil)
			backupState.recordOffsite(false)
		}, "backup"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resetHealthState(t)
			c.setup(t)
			checks, status := healthChecks(nil)
			if status != healthDegraded {
				t.Fatalf("status=%q, want %q (checks=%v)", status, healthDegraded, checks)
			}
			sub, _ := checks[c.field].(map[string]any)
			if sub["status"] != healthDegraded {
				t.Fatalf("component %s not marked degraded: %v", c.field, sub)
			}
		})
	}
}
