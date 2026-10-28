package lspapi

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"testing"
)

// failingStatusStore fails every status write, like a locked or full database.
type failingStatusStore struct {
	Store
	calls int
}

func (s *failingStatusStore) UpdateOnchainStatus(ctx context.Context, id int64, status, reason string) error {
	s.calls++
	return errors.New("disk full")
}

func (s *failingStatusStore) UpdateLightningDelivery(ctx context.Context, id int64, update LightningDeliveryUpdate) error {
	s.calls++
	return errors.New("disk full")
}

// A lost status write leaves the record in its previous state, so the next
// cron tick acts on it again. It must be logged, never dropped.
func TestFailedStatusWritesAreLogged(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	store := &failingStatusStore{}
	api := &API{db: store}

	api.persistOnchainStatus(context.Background(), 7, statusCompleted, "")
	api.persistLightningDelivery(context.Background(), 9, LightningDeliveryUpdate{Status: statusCompleted})

	if store.calls != 2 {
		t.Fatalf("expected both writes to be attempted, got %d", store.calls)
	}
	logged := buf.String()
	if !strings.Contains(logged, "disk full") {
		t.Fatalf("a failed status write must be logged, got %q", logged)
	}
	if !strings.Contains(logged, "7") || !strings.Contains(logged, "9") {
		t.Fatalf("the log must name the affected record, got %q", logged)
	}
}
