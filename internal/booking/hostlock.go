package booking

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"slices"

	"github.com/calnode/calnode/internal/db"
)

// hostLockDomain prefixes every id fed to the hash below, so a future advisory
// lock on some other entity cannot collide with a host's key by hashing the same
// raw string.
const hostLockDomain = "calnode:booking:host:"

// lockHosts serialises the check-then-write window for a set of hosts, for the
// remainder of tx.
//
// Create, Reschedule and ReassignHost all read "is this host busy at this time?"
// and then write on the answer. On SQLite that is free of TOCTOU races without any
// locking, because the pool is a single connection (db.SetMaxOpenConns(1), see
// internal/db/db.go) and transactions therefore cannot interleave at all. A
// Postgres pool has many connections: two overlapping bookings can both read
// "free" and both insert, and the partial unique index
// idx_bookings_no_double(host_id, start_at) only catches an identical start time,
// not a partial overlap. That is exactly the gap the app-level check was never
// asked to close on its own.
//
// pg_advisory_xact_lock is held until the transaction commits or rolls back, so
// there is no unlock call to forget and no leak when a caller returns early — which
// every guard in these functions does. Locking per host rather than once globally
// keeps bookings for different hosts concurrent, which is the whole point of moving
// off the single connection.
//
// On SQLite this is a no-op and the existing guarantee stands unchanged.
func lockHosts(ctx context.Context, tx *db.Tx, hostIDs ...string) error {
	if tx.Dialect() != db.DialectPostgres {
		return nil
	}

	// Sorted and deduplicated. Two transactions that needed the same two hosts in
	// opposite orders would otherwise deadlock, and Postgres resolves a deadlock by
	// killing one of them — a 500 on a booking that should have been a 409 or a
	// success. Sorting is done on a copy: Create's HostIDs arrive in round-robin
	// priority order and that order decides who gets the booking.
	ids := slices.Clone(hostIDs)
	slices.Sort(ids)
	ids = slices.Compact(ids)

	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`SELECT pg_advisory_xact_lock(?)`, hostLockKey(id)); err != nil {
			return fmt.Errorf("booking: lock host %s: %w", id, err)
		}
	}
	return nil
}

// hostLockKey maps a host id onto the single int64 key pg_advisory_xact_lock takes.
//
// SHA-256 of the domain-separated id, with the first eight bytes read big-endian as
// a signed integer. Deriving it in Go rather than with the engine's hashtext() keeps
// the derivation readable and testable from Go, and means the value arrives as an
// ordinary bound parameter.
//
// Two distinct hosts landing on the same key would cost an unnecessary
// serialisation between two unrelated bookings, never a wrong answer, so what this
// needs is stability and a good spread rather than cryptographic strength. SHA-256
// is used because this package already imports it for manage tokens.
func hostLockKey(hostID string) int64 {
	sum := sha256.Sum256([]byte(hostLockDomain + hostID))
	// #nosec G115 -- not a narrowing conversion: uint64 -> int64 is the same 64 bits read
	// two's-complement, and pg_advisory_xact_lock's parameter is a bigint whose domain is
	// exactly that signed range, negatives included. Every one of the 2^64 hash values maps
	// to a distinct, valid key, so there is no input for which this loses information or
	// produces a wrong lock. Masking the top bit to satisfy the rule numerically would
	// change the derivation, which TestHostLockKey pins precisely because two instances
	// sharing one Postgres must agree on the key or they serialise against nothing.
	return int64(binary.BigEndian.Uint64(sum[:8]))
}
