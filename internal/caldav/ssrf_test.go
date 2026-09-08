package caldav

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calnode/calnode/internal/netutil"
)

// M1: `server_url` is a blind SSRF into the operator's private network — on a
// MULTI-TENANT instance, and only there.
//
// ⛔ The narrow metadata-only guard is correct for a self-hoster and stays the default:
// a Nextcloud, Radicale or Baïkal on their own LAN or on localhost is the intended
// configuration of a self-hostable product. Every term of that inverts on a multi-tenant
// instance — the string comes from a TENANT, the private network it reaches is the
// OPERATOR's (the pod network, node-exporter, postgres_exporter, Alloy, the website pod,
// the media plane), and connect-success versus "could not reach" plus timing is a slow
// port scan any workspace member can run inside their write budget.
//
// So the strict guard (the one webhook delivery already uses) is switched on by
// WithStrictSSRFGuard, which server.New passes cfg.MultiTenant.

// newGuardedClient builds a client with the strict guard on, and with resolve as its
// lookup when one is given.
func newGuardedClient(t *testing.T, resolve netutil.Resolver) *Client {
	t.Helper()
	opts := []Option{WithStrictSSRFGuard(true)}
	if resolve != nil {
		opts = append(opts, withResolver(resolve))
	}
	c, err := New(newTestDB(t), testKeyHex, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// TestStrictGuard_refusesTheOperatorsNetwork walks the addresses that matter on this
// fleet: the k3s service network, loopback, the cloud metadata address, and IPv6
// loopback. None needs DNS — LookupIPAddr returns an IP literal unchanged — so these are
// the real production resolver, not a stub.
func TestStrictGuard_refusesTheOperatorsNetwork(t *testing.T) {
	for _, target := range []string{
		"http://10.43.0.1/",       // k3s ClusterIP range: every Service on the node
		"http://127.0.0.1:9100/",  // node-exporter on the host
		"http://169.254.169.254/", // cloud metadata
		"http://[::1]:5432/",      // IPv6 loopback: the region's PostgreSQL
		"http://192.168.1.1/",     // an operator LAN a self-hoster would allow
		"http://100.64.0.1/",      // CGNAT
		"http://[fd00:ec2::254]/", // AWS IMDS over IPv6 (a ULA, not link-local)
	} {
		t.Run(target, func(t *testing.T) {
			c := newGuardedClient(t, nil)

			_, _, _, err := c.do(context.Background(), "PROPFIND", target, "u", "p", "0", "")
			if err == nil {
				t.Fatal("the dial succeeded; the strict guard let a private address through")
			}
			assertRefusedWithoutDisclosing(t, err, target)
		})
	}
}

// ⛔ The case an IP literal cannot cover: a NAME. DNS rebinding is why the guard resolves
// and then dials the resolved address rather than handing the hostname to the dialer, and
// a stub resolver is the only way to exercise it without depending on somebody else's
// zone.
func TestStrictGuard_refusesANameThatResolvesPrivate(t *testing.T) {
	var asked string
	c := newGuardedClient(t, func(_ context.Context, host string) ([]net.IPAddr, error) {
		asked = host
		// What the real ResolveSafe does with a name that answers 10.43.0.1.
		return nil, fmt.Errorf("%q resolved to a private or loopback address", host)
	})

	_, _, _, err := c.do(context.Background(), "PROPFIND",
		"https://caldav.customer.example/dav/", "u", "p", "0", "")
	if err == nil {
		t.Fatal("the dial succeeded for a name that resolves private")
	}
	if asked != "caldav.customer.example" {
		t.Errorf("the guard resolved %q; want the request's hostname", asked)
	}
	assertRefusedWithoutDisclosing(t, err, "10.43.0.1")
}

// The other half, and the one that says the guard is a guard rather than an outage: a
// PUBLIC address still connects in the same mode. Without this, "every dial fails" would
// satisfy every assertion above.
func TestStrictGuard_apublicAddressStillConnects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMultiStatus)
		fmt.Fprint(w, `<?xml version="1.0"?><D:multistatus xmlns:D="DAV:"></D:multistatus>`)
	}))
	defer srv.Close()

	// httptest binds 127.0.0.1, which the strict guard refuses — correctly. The stub
	// resolver stands in for "this name is public", and the dial that follows is a real
	// one to the real server, so what is proved is that a permitted resolution ends in a
	// connection rather than in a second refusal.
	host, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("split test server address: %v", err)
	}
	c := newGuardedClient(t, func(_ context.Context, _ string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP(host)}}, nil
	})

	status, _, _, err := c.do(context.Background(), "PROPFIND",
		"http://caldav.public.example:"+port+"/dav/", "u", "p", "0", "")
	if err != nil {
		t.Fatalf("a permitted address did not connect: %v", err)
	}
	if status != http.StatusMultiStatus {
		t.Errorf("status = %d; want 207", status)
	}
}

// ⛔ Single-tenant is unchanged, and this is the assertion that keeps it that way: a
// self-hoster's CalDAV server on 127.0.0.1 must still connect. Only the metadata range
// is refused there.
func TestNarrowGuard_isTheDefaultAndAllowsAPrivateServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMultiStatus)
		fmt.Fprint(w, `<?xml version="1.0"?><D:multistatus xmlns:D="DAV:"></D:multistatus>`)
	}))
	defer srv.Close()

	c, err := New(newTestDB(t), testKeyHex) // no options: the shipped default
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.strictSSRF {
		t.Fatal("the strict guard is on by default; a self-hoster's own network would be refused")
	}

	status, _, _, err := c.do(context.Background(), "PROPFIND", srv.URL, "u", "p", "0", "")
	if err != nil {
		t.Fatalf("a self-hosted server on loopback was refused: %v", err)
	}
	if status != http.StatusMultiStatus {
		t.Errorf("status = %d; want 207", status)
	}

	// The metadata range is still refused, in both modes, for everyone.
	if _, _, _, err := c.do(context.Background(), "PROPFIND", "http://169.254.169.254/", "u", "p", "0", ""); err == nil {
		t.Error("cloud metadata was reachable through the narrow guard")
	}
}

// assertRefusedWithoutDisclosing pins the user-facing half of M1: the sentence a refused
// dial produces is the same one an unreachable server produces, and it names no address.
//
// ⚠️ handler.ConnectCalDAV writes this error's text straight into a 400 for the connect
// form, so an error that named the blocked address would BE the oracle the guard exists
// to close.
func assertRefusedWithoutDisclosing(t *testing.T, err error, secret string) {
	t.Helper()
	if !errors.Is(err, errCouldNotReach) {
		t.Errorf("error = %v; want the generic %q", err, errCouldNotReach)
	}
	msg := err.Error()
	if strings.Contains(msg, secret) {
		t.Errorf("error %q names %q; the address must stay in the log line", msg, secret)
	}
	for _, leak := range []string{"blocked", "private", "loopback", "link-local"} {
		if strings.Contains(strings.ToLower(msg), leak) {
			t.Errorf("error %q says %q, which tells the caller WHY it failed and turns "+
				"connect-failure into a network probe", msg, leak)
		}
	}
}
