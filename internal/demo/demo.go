// Package demo seeds and resets the public demo instance's database. The
// demo runs with no persistent volume — every container boot starts from an
// empty, freshly-migrated DB — so Seed doubles as "first boot" and "after a
// restart", and Reset (wipe + re-seed) is how the demo clears visitor data
// on a schedule without waiting for the container to restart on its own.
package demo

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/uid"
)

// OwnerUserID is the fixed ID of the seeded demo owner account. Fixed (not
// randomly generated) so the one-click "enter demo" endpoint can mint a
// session for it without a password or a DB lookup.
const OwnerUserID = "demo-owner-user"

const (
	memberUserID = "demo-member-user"
	teamID       = "demo-team"
)

// Seed populates a freshly-migrated, empty database with sample data: an
// owner + one team member, three event types (two fixed, one round-robin),
// Monday-Friday availability, and a few upcoming sample bookings. Rows are
// inserted directly via SQL, bypassing the HTTP layer — the same pattern
// already used by this package's handler test fixtures.
func Seed(ctx context.Context, db *db.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("demo seed: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// ON CONFLICT DO NOTHING rather than SQLite's INSERT OR IGNORE: both engines
	// accept it. The conflict target is omitted, which covers any unique
	// constraint, exactly as OR IGNORE did.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO server_settings (id) VALUES (1) ON CONFLICT DO NOTHING`); err != nil {
		return fmt.Errorf("demo seed: server_settings: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO users (id, email, name, iana_timezone, is_admin, is_owner, email_login)
		VALUES (?, 'demo@calnode.com', 'Demo Owner', 'UTC', 1, 1, 0)`, OwnerUserID); err != nil {
		return fmt.Errorf("demo seed: owner user: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO users (id, email, name, iana_timezone, is_admin, is_owner, email_login)
		VALUES (?, 'alex@calnode.com', 'Alex Rivera', 'UTC', 0, 0, 0)`, memberUserID); err != nil {
		return fmt.Errorf("demo seed: member user: %w", err)
	}

	for _, userID := range []string{OwnerUserID, memberUserID} {
		for day := 1; day <= 5; day++ { // Monday(1)-Friday(5)
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO availability_rules (id, user_id, day_of_week, start_time, end_time)
				VALUES (?, ?, ?, '09:00', '17:00')`, uid.New(), userID, day); err != nil {
				return fmt.Errorf("demo seed: availability for %s: %w", userID, err)
			}
		}
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO teams (id, name, slug) VALUES (?, 'Demo Team', 'demo-team')`, teamID); err != nil {
		return fmt.Errorf("demo seed: team: %w", err)
	}
	teamMembers := []struct {
		userID   string
		role     string
		priority int
	}{
		{OwnerUserID, "owner", 0},
		{memberUserID, "member", 1},
	}
	for _, m := range teamMembers {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO team_members (id, team_id, user_id, role, routing_priority)
			VALUES (?, ?, ?, ?, ?)`, uid.New(), teamID, m.userID, m.role, m.priority); err != nil {
			return fmt.Errorf("demo seed: team member %s: %w", m.userID, err)
		}
	}

	eventTypes := []struct {
		id, slug, name, description string
		durationMinutes             int
		minNoticeMinutes            int
		maxFutureDays               int
		roundRobin                  bool
	}{
		{"demo-et-intro", "intro-call", "15-Minute Intro Call",
			"A quick chat to see if we're a good fit.", 15, 60, 30, false},
		{"demo-et-meeting", "30-min-meeting", "30-Minute Meeting",
			"A standard half-hour meeting slot.", 30, 60, 30, false},
		{"demo-et-teamsync", "team-sync", "Team Sync",
			"Round-robin sync across the team.", 45, 60, 30, true},
	}
	for _, et := range eventTypes {
		routingMode := "fixed"
		var teamVal any // NULL for fixed event types
		if et.roundRobin {
			routingMode = "round_robin"
			teamVal = teamID
		}
		// location_value is set, not left NULL. 'link' with no URL is a state
		// validateLocation rejects, so every seeded event type used to fail on its first
		// save from the editor - with an error about a meeting URL the visitor had not
		// touched, on their first interaction with the demo.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO event_types
			  (id, user_id, team_id, slug, name, description, duration_minutes,
			   location_type, location_value, routing_mode, min_notice_minutes,
			   max_future_days, is_active, is_public)
			VALUES (?, ?, ?, ?, ?, ?, ?, 'link', 'https://example.com/demo-meeting', ?, ?, ?, 1, 1)`,
			et.id, OwnerUserID, teamVal, et.slug, et.name, et.description, et.durationMinutes,
			routingMode, et.minNoticeMinutes, et.maxFutureDays); err != nil {
			return fmt.Errorf("demo seed: event type %s: %w", et.slug, err)
		}

		hosts := []string{OwnerUserID}
		role := "required"
		if et.roundRobin {
			hosts = []string{OwnerUserID, memberUserID}
			role = "rotation"
		}
		for _, hostID := range hosts {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO event_type_hosts (id, event_type_id, user_id, role, priority)
				VALUES (?, ?, ?, ?, 0)`, uid.New(), et.id, hostID, role); err != nil {
				return fmt.Errorf("demo seed: event type host %s/%s: %w", et.slug, hostID, err)
			}
		}
	}

	now := time.Now().UTC()
	bookings := []struct {
		id, eventTypeID, hostID           string
		attendeeName, attendeeEmail       string
		minDaysOut, hour, durationMinutes int
	}{
		{"demo-booking-1", "demo-et-intro", OwnerUserID,
			"Jordan Lee", "jordan@example.com", 1, 10, 15},
		{"demo-booking-2", "demo-et-meeting", OwnerUserID,
			"Sam Patel", "sam@example.com", 2, 14, 30},
		{"demo-booking-3", "demo-et-teamsync", memberUserID,
			"Taylor Kim", "taylor@example.com", 3, 11, 45},
	}
	for _, b := range bookings {
		startAt := nextWeekdayAt(now, b.minDaysOut, b.hour)
		endAt := startAt.Add(time.Duration(b.durationMinutes) * time.Minute)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO bookings (id, event_type_id, host_id, start_at, end_at, status)
			VALUES (?, ?, ?, ?, ?, 'confirmed')`,
			b.id, b.eventTypeID, b.hostID, startAt.Format(time.RFC3339), endAt.Format(time.RFC3339)); err != nil {
			return fmt.Errorf("demo seed: booking %s: %w", b.id, err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO booking_hosts (id, booking_id, user_id, is_primary)
			VALUES (?, ?, ?, 1)`, uid.New(), b.id, b.hostID); err != nil {
			return fmt.Errorf("demo seed: booking host %s: %w", b.id, err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO booking_attendees (id, booking_id, name, email, iana_timezone, is_organizer)
			VALUES (?, ?, ?, ?, 'UTC', 1)`, uid.New(), b.id, b.attendeeName, b.attendeeEmail); err != nil {
			return fmt.Errorf("demo seed: booking attendee %s: %w", b.id, err)
		}
	}

	return tx.Commit()
}

// nextWeekdayAt returns the next Monday-Friday date at least minDaysOut days
// after now, at the given UTC hour — so seeded sample bookings always land
// inside the seeded Mon-Fri 09:00-17:00 availability window.
func nextWeekdayAt(now time.Time, minDaysOut, hour int) time.Time {
	d := now.AddDate(0, 0, minDaysOut)
	for d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
		d = d.AddDate(0, 0, 1)
	}
	return time.Date(d.Year(), d.Month(), d.Day(), hour, 0, 0, 0, time.UTC)
}

// Reset wipes every table in db and re-seeds it. Tables are enumerated
// dynamically rather than hardcoded, since this actively deletes data —
// see docs/ARCHITECTURE.md's hardcoded-column-count incident for why a
// stale hardcoded list is worth avoiding here specifically.
func Reset(ctx context.Context, db *db.DB) (err error) {
	tables, err := listTables(ctx, db)
	if err != nil {
		return err
	}

	// The wipe order below is arbitrary relative to the schema's 46 migrations'
	// worth of foreign keys, so referential integrity has to be stood down for it.
	// The engines do that differently and neither way exists on the other:
	//
	//   SQLite — PRAGMA foreign_keys is connection-scoped and cannot be toggled
	//   mid-transaction, so it is set before BeginTx. Safe only because the pool
	//   is one persistent connection (db.SetMaxOpenConns(1), internal/db/db.go).
	//
	//   Postgres — there is no equivalent switch short of superuser rights, so a
	//   single TRUNCATE naming every table CASCADEs through the foreign keys
	//   instead. TRUNCATE is transactional there, so it stays inside the tx.
	if isSQLite(db) {
		if _, ferr := db.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); ferr != nil {
			return fmt.Errorf("demo reset: disable foreign keys: %w", ferr)
		}
		defer func() {
			if _, ferr := db.ExecContext(ctx, `PRAGMA foreign_keys = ON`); ferr != nil && err == nil {
				err = fmt.Errorf("demo reset: re-enable foreign keys: %w", ferr)
			}
		}()
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("demo reset: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if isSQLite(db) {
		for _, t := range tables {
			if _, err = tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %q`, t)); err != nil {
				return fmt.Errorf("demo reset: delete from %s: %w", t, err)
			}
		}
	} else if len(tables) > 0 {
		quoted := make([]string, len(tables))
		for i, t := range tables {
			quoted[i] = `"` + t + `"`
		}
		// #nosec G202 -- every name came from pg_tables in this schema, not from a request.
		if _, err = tx.ExecContext(ctx,
			`TRUNCATE TABLE `+strings.Join(quoted, ", ")+` CASCADE`); err != nil {
			return fmt.Errorf("demo reset: truncate: %w", err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("demo reset: commit: %w", err)
	}

	return Seed(ctx, db)
}

// isSQLite is a free function rather than an inline comparison because Reset and
// listTables both name their handle "db", which shadows the package the Dialect
// constants live in.
func isSQLite(h *db.DB) bool { return h.Dialect() == db.DialectSQLite }

func listTables(ctx context.Context, db *db.DB) ([]string, error) {
	// sqlite_master has no Postgres counterpart. pg_tables scoped to
	// current_schema() is the equivalent, and honouring the current schema is what
	// lets a test run inside its own isolated one.
	//
	// Two tables are held back. goose_db_version is migration bookkeeping, and
	// wiping it would make the next boot re-run every migration. workspaces is the
	// tenant root (migration 00060): every application table's workspace_id has a
	// foreign key to it, so truncating it takes the 'default' row with it and the
	// re-seed's first INSERT fails with SQLSTATE 23503. It holds no visitor data —
	// in demo mode there is exactly one row, and it is a constant.
	rows, err := db.QueryContext(ctx, db.Dialect().SQL(
		`SELECT name FROM sqlite_master
		 WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
		   AND name NOT IN ('goose_db_version', 'workspaces')`,
		`SELECT tablename FROM pg_tables
		 WHERE schemaname = current_schema()
		   AND tablename NOT IN ('goose_db_version', 'workspaces')`))
	if err != nil {
		return nil, fmt.Errorf("demo reset: list tables: %w", err)
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("demo reset: scan table name: %w", err)
		}
		tables = append(tables, name)
	}
	return tables, rows.Err()
}
