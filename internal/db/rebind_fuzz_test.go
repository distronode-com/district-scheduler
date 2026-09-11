package db_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/calnode/calnode/internal/db"
)

// FuzzRebind holds the placeholder lexer to its contract for arbitrary SQL-shaped input.
//
// Rebind is the one piece of this tree that rewrites every Postgres statement the server
// sends, and its whole reason for existing is that a naive strings.Replace corrupts a ?
// inside a literal or a comment. The corruption surfaces at runtime, a long way from the
// query that caused it, which is precisely the failure a table test is worst at finding:
// the rows are the cases someone already had in mind.
//
// Four properties, in package db_test because Rebind is exported and needs no internals:
//
//	(a) it never panics — every f.Fuzz body asserts this by running;
//	(b) an input with no ? comes back byte for byte;
//	(c) the number of $N tokens emitted equals the number of ? outside quotes and
//	    comments, counted by the independent scanner below;
//	(d) putting the ?s back yields the input again, for inputs that carry no literal
//	    $ followed by a digit (where the reverse direction is genuinely ambiguous).
//
// (c) is asserted through a whole-output reconstruction rather than a tally, which is
// strictly stronger: it pins WHERE each placeholder landed and that the numbering runs
// 1..N left to right, not merely how many there were.
func FuzzRebind(f *testing.F) {
	seeds := []string{
		// Every query in TestRebind and TestRebind_manyPlaceholders.
		`SELECT id FROM users`,
		`SELECT id FROM users WHERE email = ?`,
		`UPDATE users SET name = ?, email = ? WHERE id = ?`,
		`SELECT id FROM event_types WHERE name = 'why?' AND slug = ?`,
		`SELECT ? WHERE reason = 'it''s a ?' AND id = ?`,
		`SELECT "odd?column" FROM t WHERE id = ?`,
		`SELECT "a""?b" FROM t WHERE id = ?`,
		"SELECT `weird?` FROM t WHERE id = ?",
		"SELECT id -- what about ?\nFROM t WHERE id = ?",
		`SELECT id FROM t WHERE id = ? -- trailing ?`,
		`SELECT /* ? not a param ? */ id FROM t WHERE id = ?`,
		`SELECT /* outer /* inner ? */ still ? */ id FROM t WHERE id = ?`,
		`SELECT price_cents / 100 FROM event_types WHERE id = ?`,
		`SELECT id FROM t WHERE n = -1 AND id = ?`,
		`SELECT id FROM t WHERE s = 'oops ? and id = ?`,
		`SELECT id FROM t /* oops ? and id = ?`,
		`SELECT id FROM t WHERE id = $1`,
		"-- +goose Up\nINSERT INTO t (a, b) VALUES (?, ?)\n  -- ? in a comment\n  ON CONFLICT DO NOTHING",
		`INSERT INTO t VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,

		// The bare edge shapes, each one byte away from a different branch.
		"",
		"?",
		"??",
		`'?'`,
		`"?"`,
		"`?`",
		"-- ?\n?",
		"/* ? */ ?",
		"/* /* ? */ */ ?",
		"'?",
		"/* ?",
		"*/ ?",
		"?/*?*/?",
		"$?",
		"?0",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, query string) {
		got := db.Rebind(query)

		if !strings.Contains(query, "?") {
			if got != query {
				t.Fatalf("Rebind(%q) = %q; a query with no ? must come back unchanged", query, got)
			}
			return
		}

		at := placeholderOffsets(query)

		// (c), as a reconstruction: the input with each placeholder ? replaced by $k.
		var want strings.Builder
		want.Grow(len(query) + 8)
		prev := 0
		for k, off := range at {
			want.WriteString(query[prev:off])
			want.WriteByte('$')
			want.WriteString(strconv.Itoa(k + 1))
			prev = off + 1
		}
		want.WriteString(query[prev:])
		if got != want.String() {
			t.Fatalf("Rebind(%q)\n got %q\nwant %q\n(placeholders at %v)", query, got, want.String(), at)
		}

		// Both remaining properties read the output without reference to the input's
		// structure, so both need the input to carry no $ before a digit: there an emitted
		// placeholder is indistinguishable from the caller's own text, and the ambiguity is
		// the input's rather than Rebind's. The reconstruction above has no such blind spot
		// and has already covered this input.
		if hasDollarDigit(query) {
			return
		}

		// (c) said as a count, because that is the one a caller depends on: the argument
		// list is bound positionally, so N arguments must meet N placeholders.
		if n := countEmitted(got); n != len(at) {
			t.Fatalf("Rebind(%q) = %q emitted %d $N tokens; want %d", query, got, n, len(at))
		}

		// (d) round trip.
		if back := unrebind(got); back != query {
			t.Fatalf("round trip of %q through %q gave %q", query, got, back)
		}
	})
}

// placeholderOffsets returns the byte offsets of the ?s that are placeholders — those
// outside string literals, quoted identifiers and comments.
//
// It is a flat state machine written from Rebind's documented contract rather than from
// its code: an oracle that shared the implementation would agree with a bug. The rules it
// encodes are SQL's, and all four are ones Rebind's comments name: a quote run ends at an
// undoubled matching quote or at end of input; -- runs to the newline, which is not part of
// the comment; /* */ nests; and nothing else is special.
func placeholderOffsets(q string) []int {
	const (
		normal = iota
		quoted
		lineComment
		blockComment
	)

	var (
		at    []int
		state = normal
		quote byte
		depth int
	)
	for i := 0; i < len(q); {
		c := q[i]
		next := byte(0)
		hasNext := i+1 < len(q)
		if hasNext {
			next = q[i+1]
		}

		switch state {
		case normal:
			switch {
			case c == '\'' || c == '"' || c == '`':
				state, quote = quoted, c
				i++
			case c == '-' && hasNext && next == '-':
				state = lineComment
				i += 2
			case c == '/' && hasNext && next == '*':
				state, depth = blockComment, 1
				i += 2
			case c == '?':
				at = append(at, i)
				i++
			default:
				i++
			}
		case quoted:
			switch {
			case c != quote:
				i++
			case hasNext && next == quote:
				i += 2 // a doubled quote is an escaped one and does not close the run
			default:
				state = normal
				i++
			}
		case lineComment:
			if c == '\n' {
				state = normal
			}
			i++
		case blockComment:
			switch {
			case c == '/' && hasNext && next == '*':
				depth++
				i += 2
			case c == '*' && hasNext && next == '/':
				depth--
				i += 2
				if depth == 0 {
					state = normal
				}
			default:
				i++
			}
		}
	}
	return at
}

// countEmitted counts the $1, $2, … tokens in out, matched in order. Matching the exact
// expected number rather than "$ followed by digits" is what keeps `?0` — which becomes
// `$10` — from reading as a single ten.
func countEmitted(out string) int {
	n := 0
	for i := 0; i < len(out); {
		want := "$" + strconv.Itoa(n+1)
		if strings.HasPrefix(out[i:], want) {
			n++
			i += len(want)
			continue
		}
		i++
	}
	return n
}

// unrebind is countEmitted's twin: it rewrites those same tokens back to ?, so the result
// can be compared with the input.
func unrebind(out string) string {
	var b strings.Builder
	b.Grow(len(out))
	n := 0
	for i := 0; i < len(out); {
		want := "$" + strconv.Itoa(n+1)
		if strings.HasPrefix(out[i:], want) {
			b.WriteByte('?')
			n++
			i += len(want)
			continue
		}
		b.WriteByte(out[i])
		i++
	}
	return b.String()
}

// hasDollarDigit reports whether s contains a $ immediately followed by an ASCII digit.
func hasDollarDigit(s string) bool {
	for i := 0; i+1 < len(s); i++ {
		if s[i] == '$' && s[i+1] >= '0' && s[i+1] <= '9' {
			return true
		}
	}
	return false
}
