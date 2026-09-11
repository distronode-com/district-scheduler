package db_test

import (
	"database/sql"
	"testing"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/dbtest"
)

// Migration 00065 repairs the event types provisioning seeded as 'link' with no value —
// a state validateLocation rejects, so the tenant's first save from the editor failed on
// a field they had never touched. The seed fix stops new rows arriving in it; this is the
// rows already there.
//
// ⚠️ Both engines. The migration is a plain UPDATE with nothing dialect-specific in it,
// which is exactly the kind of file that gets written once and copied — so it is asserted
// on whichever engine dbtest selects rather than on SQLite alone, and CI runs both.

// replayMigrations re-applies the newest migration by forgetting it, so a data migration
// can be exercised against rows that were not there when the test database was built.
//
// dbtest hands back a database already at the target version, and 00065's whole subject is
// rows that predate it. Deleting its goose_db_version row and migrating again runs the
// real file, from the real embedded FS, through the same db.Migrate the boot path calls —
// rather than a copy of its SQL pasted into a test, which would assert that the test is
// self-consistent and nothing about the migration that ships.
func replayMigrations(t *testing.T, handle *db.DB, version int64) {
	t.Helper()
	if _, err := handle.Exec(`DELETE FROM goose_db_version WHERE version_id = ?`, version); err != nil {
		t.Fatalf("forget migration %d: %v", version, err)
	}
	if err := handle.Migrate(); err != nil {
		t.Fatalf("re-run migrations: %v", err)
	}
}

func TestMigration00065_repairsSeededLinkLocations(t *testing.T) {
	handle := dbtest.Open(t)

	if _, err := handle.Exec(
		`INSERT INTO users (id, email, name) VALUES (?, ?, ?)`,
		"u1", "owner@example.com", "Owner"); err != nil {
		t.Fatalf("insert owner: %v", err)
	}

	// The four shapes that matter. Only the first two are broken: 'link' carries its join
	// info in location_value, so without one there is nothing to join. An empty string is
	// the same absence spelled differently, and validateLocation rejects it identically.
	seed := []struct {
		id       string
		locType  string
		locValue any
	}{
		{"et-seeded", "link", nil},
		{"et-empty", "link", ""},
		{"et-chosen", "link", "https://meet.example.com/standup"},
		{"et-phone", "phone", "+14165550123"},
	}
	for _, s := range seed {
		if _, err := handle.Exec(`
			INSERT INTO event_types (id, user_id, slug, name, duration_minutes, location_type, location_value)
			VALUES (?, ?, ?, ?, 30, ?, ?)`,
			s.id, "u1", s.id, "Seeded "+s.id, s.locType, s.locValue); err != nil {
			t.Fatalf("insert %s: %v", s.id, err)
		}
	}

	replayMigrations(t, handle, 65)

	want := []struct {
		id       string
		locType  string
		locValue sql.NullString
		why      string
	}{
		{"et-seeded", "in_person", sql.NullString{}, "the shape provisioning wrote: 'link' with no URL, which the editor refuses to save"},
		{"et-empty", "in_person", sql.NullString{}, "an empty string is the same absence as NULL, and validateLocation rejects both"},
		{"et-chosen", "link", sql.NullString{String: "https://meet.example.com/standup", Valid: true}, "a link somebody chose is valid and must be left alone"},
		{"et-phone", "phone", sql.NullString{String: "+14165550123", Valid: true}, "the repair is narrowed to location_type = 'link'"},
	}
	for _, w := range want {
		var locType string
		var locValue sql.NullString
		if err := handle.QueryRow(
			`SELECT location_type, location_value FROM event_types WHERE id = ?`, w.id).
			Scan(&locType, &locValue); err != nil {
			t.Fatalf("read %s: %v", w.id, err)
		}
		if locType != w.locType || locValue != w.locValue {
			t.Errorf("%s = %q/%v after migrating; want %q/%v — %s",
				w.id, locType, locValue, w.locType, w.locValue, w.why)
		}
	}
}
