package webhook_test

import (
	"context"
	"errors"
	"testing"

	"github.com/calnode/calnode/internal/webhook"
)

// Managed webhooks (F1, migration 00063).
//
// `managed = 1` marks a subscription the PLATFORM created and owns. It changes what the
// ADMINISTRATION side of this package does — List hides it, Update and Delete refuse it —
// and deliberately changes nothing about the DELIVERY side.

// markManaged flips the flag the way the platform API does, since Create has no parameter
// for it: nothing a credential caller can reach may mint a managed row.
func markManaged(t *testing.T, e *env, id string) {
	t.Helper()
	if _, err := e.db.ExecContext(context.Background(),
		`UPDATE webhooks SET managed = 1 WHERE id = ?`, id); err != nil {
		t.Fatalf("mark managed: %v", err)
	}
}

func TestManagedWebhook_isHiddenFromList(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	managed, _, err := e.svc.Create(ctx, testUserID, "https://platform.example/in", []string{"booking.created"})
	if err != nil {
		t.Fatalf("create managed: %v", err)
	}
	markManaged(t, e, managed.ID)
	mine, _, err := e.svc.Create(ctx, testUserID, "https://mine.example/in", []string{"booking.created"})
	if err != nil {
		t.Fatalf("create own: %v", err)
	}

	list, err := e.svc.List(ctx, testUserID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("List returned %d webhooks; want 1 (the caller's own)", len(list))
	}
	if list[0].ID != mine.ID {
		t.Errorf("List returned %q; want the caller's own %q", list[0].ID, mine.ID)
	}
}

func TestManagedWebhook_updateAndDeleteAreRefused(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	wh, _, err := e.svc.Create(ctx, testUserID, "https://platform.example/in", []string{"booking.created"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	markManaged(t, e, wh.ID)

	events := []string{"booking.cancelled"}
	if err := e.svc.Update(ctx, testUserID, wh.ID, &events, nil); !errors.Is(err, webhook.ErrManaged) {
		t.Errorf("Update on a managed webhook = %v; want ErrManaged", err)
	}
	if err := e.svc.Delete(ctx, testUserID, wh.ID); !errors.Is(err, webhook.ErrManaged) {
		t.Errorf("Delete on a managed webhook = %v; want ErrManaged", err)
	}

	// Neither refusal may have changed anything.
	var count int
	var eventsJSON string
	if err := e.db.QueryRowContext(ctx,
		`SELECT COUNT(*), MAX(events) FROM webhooks WHERE id = ?`, wh.ID).Scan(&count, &eventsJSON); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if count != 1 {
		t.Fatalf("the managed webhook was deleted anyway")
	}
	if eventsJSON != `["booking.created"]` {
		t.Errorf("events = %s; want the original — a refused Update must change nothing", eventsJSON)
	}
}

// ⛔ The one that matters. Hiding a subscription from its owner's settings page must never
// stop it receiving events: the provisioning webhook is how the platform hears about every
// booking in the tenancy, and a `managed = 0` predicate added to Enqueue "for consistency"
// would switch that off silently, with the row still present and still is_active = 1.
func TestManagedWebhookStillDelivers(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	wh, _, err := e.svc.Create(ctx, testUserID, "https://platform.example/in", []string{"booking.created"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	markManaged(t, e, wh.ID)

	if err := e.svc.Enqueue(ctx, "booking.created", webhook.BookingPayload{
		EventTypeSlug: "30-min",
		HostID:        testUserID,
		StartAt:       "2026-06-15T09:00:00Z",
		EndAt:         "2026-06-15T09:30:00Z",
		Status:        "confirmed",
		CreatedAt:     "2026-06-14T15:00:00Z",
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	var deliveries int
	if err := e.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM webhook_deliveries WHERE webhook_id = ?`, wh.ID).Scan(&deliveries); err != nil {
		t.Fatalf("count deliveries: %v", err)
	}
	if deliveries != 1 {
		t.Errorf("a managed webhook got %d deliveries; want 1 — managed hides a row from its "+
			"owner's settings page, it does not unsubscribe it", deliveries)
	}
}
