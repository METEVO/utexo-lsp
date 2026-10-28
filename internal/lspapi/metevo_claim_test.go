package lspapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"utexo-lsp/pkg/node_client"
)

func newMetevoTestStore(t *testing.T) *SQLStore {
	t.Helper()
	store, err := NewStore(Config{
		DatabaseDriver: "sqlite",
		DatabaseURL:    filepath.Join(t.TempDir(), "metevo.db"),
	})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	return store
}

func metevoTestHash(t *testing.T, preimage string) string {
	t.Helper()
	raw, err := hex.DecodeString(preimage)
	if err != nil {
		t.Fatalf("decode preimage: %v", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func TestMetevoClaimDoesNotDeliverAnOrderAnotherClaimIsDelivering(t *testing.T) {
	const assetID = "rgb:usdm-test"
	preimage := strings.Repeat("11", 32)
	paymentHash := metevoTestHash(t, preimage)

	store := newMetevoTestStore(t)
	ctx := context.Background()
	if err := store.InsertMetevoOrder(ctx, MetevoOrder{
		PaymentHash: paymentHash,
		USDAmount:   20,
		AssetAmount: 2_000_000_000,
		AmountMsat:  2_000_000,
		Invoice:     "lnbcrt1test",
	}); err != nil {
		t.Fatalf("insert order: %v", err)
	}
	// A concurrent claim already won the pending->delivering transition and is
	// sending the RGB assignment right now.
	transitioned, err := store.AdvanceMetevoOrder(ctx, paymentHash,
		[]string{string(metevoOrderStatusPending)}, metevoOrderStatusDelivering, "")
	if err != nil || !transitioned {
		t.Fatalf("seed delivering transition: transitioned=%v err=%v", transitioned, err)
	}

	var sendRGBCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/decodergbinvoice":
			_ = json.NewEncoder(w).Encode(node_client.DecodeRGBInvoiceResponse{
				RecipientID: "recipient",
				AssetID:     strptr(assetID),
			})
		case "/sendrgb":
			atomic.AddInt32(&sendRGBCalls, 1)
			_ = json.NewEncoder(w).Encode(node_client.SendRGBResponse{})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	client := node_client.NewClient(srv.URL, "", srv.Client())

	api := &API{
		cfg: Config{
			HTTPTimeout:      time.Second,
			APayBearerToken:  "secret",
			SupportedAssetIDs: []string{assetID},
		},
		db:        store,
		lspClient: client,
		rgbClient: client,
	}

	body, err := json.Marshal(MetevoClaimRequest{
		PaymentHash:     paymentHash,
		PaymentPreimage: preimage,
		RGBInvoice:      "rgb:invoice",
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/metevo/claim", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	rr := httptest.NewRecorder()

	api.handleMetevoClaim(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 for an order already being delivered, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := atomic.LoadInt32(&sendRGBCalls); got != 0 {
		t.Fatalf("an order already being delivered must not pay out again, got %d sendrgb calls", got)
	}
	order, err := store.LoadMetevoOrderByPaymentHash(ctx, paymentHash)
	if err != nil {
		t.Fatalf("reload order: %v", err)
	}
	if order.Status != metevoOrderStatusDelivering {
		t.Fatalf("expected status to stay %q, got %q", metevoOrderStatusDelivering, order.Status)
	}
}

// Only one of many racing callers may win the same transition: the winner is
// what authorises the payout.
func TestAdvanceMetevoOrderHasExactlyOneWinner(t *testing.T) {
	store := newMetevoTestStore(t)
	ctx := context.Background()
	paymentHash := strings.Repeat("ab", 32)
	if err := store.InsertMetevoOrder(ctx, MetevoOrder{
		PaymentHash: paymentHash,
		USDAmount:   1,
		AssetAmount: 100_000_000,
		AmountMsat:  100_000,
		Invoice:     "lnbcrt1race",
	}); err != nil {
		t.Fatalf("insert order: %v", err)
	}

	const racers = 8
	start := make(chan struct{})
	results := make(chan bool, racers)
	var wg sync.WaitGroup
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func() {
			defer wg.Done()
			<-start
			ok, err := store.AdvanceMetevoOrder(ctx, paymentHash,
				[]string{string(metevoOrderStatusPending)}, metevoOrderStatusDelivering, "")
			if err != nil {
				results <- false
				return
			}
			results <- ok
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	winners := 0
	for ok := range results {
		if ok {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("expected exactly one winning transition, got %d", winners)
	}
}
