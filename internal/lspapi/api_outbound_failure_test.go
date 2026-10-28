package lspapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"utexo-lsp/pkg/node_client"
)

// seedOutboundPendingInvoice writes the rows aPay sees once the receiver has
// handed out its invoice and the outbox tries to pay it.
func seedOutboundPendingInvoice(t *testing.T, store *SQLStore, paymentHash, outboundInvoice string) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx,
		`INSERT INTO async_orders (peer_pubkey, status) VALUES (?, ?)`,
		"02"+strings.Repeat("a", 64), string(asyncInvoiceStatusActive),
	); err != nil {
		t.Fatalf("insert async order: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO async_rotating_invoices
			(order_id, invoice_slot, hash_index, payment_hash, amount_msat, expires_at, status, request_invoice_bolt11)
		VALUES (1, 1, 1, ?, ?, ?, ?, ?)`,
		paymentHash, 2_000_000, time.Now().UTC().Add(time.Hour), string(asyncInvoiceStatusOutboundPending), outboundInvoice,
	); err != nil {
		t.Fatalf("insert outbound invoice: %v", err)
	}
}

// A payment the node reports as Failed must never be recorded as paid: the
// outbox would then wait for a claim that cannot happen and the order would
// look settled to every later reader.
func TestAPaySendOutboundPaymentMarksNodeFailureAsCancelled(t *testing.T) {
	store := newMetevoTestStore(t)
	paymentHash := strings.Repeat("cd", 32)
	outboundInvoice := "lnbcrt1outbound"
	seedOutboundPendingInvoice(t, store, paymentHash, outboundInvoice)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sendpayment" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(node_client.SendPaymentResponse{
			PaymentHash: paymentHash,
			Status:      node_client.PaymentStatusFailed,
		})
	}))
	t.Cleanup(srv.Close)
	client := node_client.NewClient(srv.URL, "", srv.Client())

	api := &API{
		cfg:       Config{HTTPTimeout: time.Second},
		db:        store,
		lspClient: client,
		rgbClient: client,
	}

	if err := api.aPaySendOutboundPaymentJob(context.Background(), paymentHash); err != nil {
		t.Fatalf("a node-reported failure is terminal, not a retry: %v", err)
	}

	invoice, err := store.LoadAsyncRotatingInvoiceByPaymentHash(context.Background(), paymentHash)
	if err != nil {
		t.Fatalf("reload invoice: %v", err)
	}
	if invoice.Status == asyncInvoiceStatusOutboundPaid {
		t.Fatalf("a failed outbound payment was recorded as %q", invoice.Status)
	}
	if invoice.Status != asyncInvoiceStatusOutboundCancelled {
		t.Fatalf("expected %q after a node-reported failure, got %q",
			asyncInvoiceStatusOutboundCancelled, invoice.Status)
	}
}
