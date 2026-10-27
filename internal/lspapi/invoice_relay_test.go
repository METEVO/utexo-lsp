package lspapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	relayTestToken     = "relay-test-token"
	relayTestRecipient = "02aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	relayTestSender    = "03bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	relayTestOther     = "02cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	relayTestInvoice   = "lnbcrt1metevotestinvoice"
	relayTestHash      = "a24672c1b3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3f4a5b6c7d8e9f0"
)

type relayTestServer struct {
	t      *testing.T
	api    *API
	store  Store
	server *httptest.Server
}

func newRelayTestServer(t *testing.T) *relayTestServer {
	t.Helper()
	store, err := NewStore(Config{
		DatabaseDriver: "sqlite",
		DatabaseURL:    filepath.Join(t.TempDir(), "relay.db"),
	})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	api := &API{
		cfg: Config{
			APayBearerToken:     relayTestToken,
			HTTPTimeout:         5 * time.Second,
			MetevoInvoiceExpiry: 10 * time.Minute,
		},
		db: store,
	}
	server := httptest.NewServer(api.routes())
	t.Cleanup(func() {
		server.Close()
		_ = store.Close()
	})
	return &relayTestServer{t: t, api: api, store: store, server: server}
}

func (s *relayTestServer) do(method, path string, body any, token string) (int, map[string]any) {
	s.t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			s.t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, s.server.URL+path, reader)
	if err != nil {
		s.t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	var decoded map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		s.t.Fatalf("decode response: %v", err)
	}
	return resp.StatusCode, decoded
}

func (s *relayTestServer) requestInvoice(recipient string, usd uint64) string {
	s.t.Helper()
	code, body := s.do("POST", "/metevo/invoice-request", map[string]any{
		"recipient_pubkey": recipient,
		"sender_pubkey":    relayTestSender,
		"amount_usd":       usd,
	}, relayTestToken)
	if code != http.StatusOK {
		s.t.Fatalf("invoice request: status %d body %v", code, body)
	}
	requestID, _ := body["request_id"].(string)
	if len(requestID) != 64 {
		s.t.Fatalf("expected a 64-hex request id, got %q", requestID)
	}
	return requestID
}

func (s *relayTestServer) poll(requestID string) (int, map[string]any) {
	s.t.Helper()
	return s.do("POST", "/metevo/invoice-response", map[string]any{"request_id": requestID}, relayTestToken)
}

func (s *relayTestServer) inbox(pubkey string) []any {
	s.t.Helper()
	code, body := s.do("GET", "/metevo/invoice-inbox?pubkey="+url.QueryEscape(pubkey), nil, relayTestToken)
	if code != http.StatusOK {
		s.t.Fatalf("inbox: status %d body %v", code, body)
	}
	requests, _ := body["requests"].([]any)
	return requests
}

func (s *relayTestServer) answer(requestID, pubkey string, invoice string, declined bool) (int, map[string]any) {
	s.t.Helper()
	return s.do("POST", "/metevo/invoice-answer", map[string]any{
		"request_id":       requestID,
		"recipient_pubkey": pubkey,
		"invoice":          invoice,
		"payment_hash":     relayTestHash,
		"declined":         declined,
	}, relayTestToken)
}

func TestInvoiceRelayDeliversRecipientInvoice(t *testing.T) {
	s := newRelayTestServer(t)
	requestID := s.requestInvoice(relayTestRecipient, 20)

	if got := s.inbox(relayTestOther); len(got) != 0 {
		t.Fatalf("another node must not see the request, got %v", got)
	}
	inbox := s.inbox(strings.ToUpper(relayTestRecipient))
	if len(inbox) != 1 {
		t.Fatalf("expected one inbox entry, got %v", inbox)
	}
	entry := inbox[0].(map[string]any)
	if entry["request_id"] != requestID || entry["amount_usd"] != float64(20) || entry["sender_pubkey"] != relayTestSender {
		t.Fatalf("unexpected inbox entry: %v", entry)
	}

	if code, body := s.poll(requestID); code != http.StatusOK || body["status"] != invoiceRelayPending {
		t.Fatalf("expected pending, got %d %v", code, body)
	}

	if code, body := s.answer(requestID, relayTestRecipient, relayTestInvoice, false); code != http.StatusOK {
		t.Fatalf("answer: %d %v", code, body)
	}

	code, body := s.poll(requestID)
	if code != http.StatusOK {
		t.Fatalf("poll: %d %v", code, body)
	}
	if body["status"] != invoiceRelayDelivered || body["invoice"] != relayTestInvoice || body["payment_hash"] != relayTestHash {
		t.Fatalf("unexpected delivered answer: %v", body)
	}

	if got := s.inbox(relayTestRecipient); len(got) != 0 {
		t.Fatalf("answered request must leave the inbox, got %v", got)
	}
	if code, _ := s.answer(requestID, relayTestRecipient, relayTestInvoice, false); code != http.StatusConflict {
		t.Fatalf("second answer must conflict, got %d", code)
	}
}

func TestInvoiceRelayDecline(t *testing.T) {
	s := newRelayTestServer(t)
	requestID := s.requestInvoice(relayTestRecipient, 5)

	if code, body := s.answer(requestID, relayTestRecipient, "", true); code != http.StatusOK {
		t.Fatalf("decline: %d %v", code, body)
	}
	code, body := s.poll(requestID)
	if code != http.StatusOK || body["status"] != invoiceRelayDeclined {
		t.Fatalf("expected declined, got %d %v", code, body)
	}
	if _, hasInvoice := body["invoice"]; hasInvoice {
		t.Fatalf("a declined request must not carry an invoice: %v", body)
	}
}

func TestInvoiceRelayExpiredRequest(t *testing.T) {
	s := newRelayTestServer(t)
	requestID, err := newInvoiceRelayID()
	if err != nil {
		t.Fatalf("new request id: %v", err)
	}
	now := time.Now()
	if err := s.store.InsertInvoiceRelayRequest(t.Context(), invoiceRelayRecord{
		RequestID:       requestID,
		RecipientPubkey: relayTestRecipient,
		USDAmount:       20,
		Status:          invoiceRelayPending,
		ExpiresAt:       now.Add(-time.Minute),
		CreatedAt:       now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("insert expired request: %v", err)
	}

	if got := s.inbox(relayTestRecipient); len(got) != 0 {
		t.Fatalf("expired requests must not reach the inbox, got %v", got)
	}
	code, body := s.poll(requestID)
	if code != http.StatusOK || body["status"] != invoiceRelayExpired {
		t.Fatalf("expected expired, got %d %v", code, body)
	}
	if code, _ := s.answer(requestID, relayTestRecipient, relayTestInvoice, false); code != http.StatusConflict {
		t.Fatalf("answering an expired request must conflict, got %d", code)
	}
}

func TestInvoiceRelayValidationAndAuth(t *testing.T) {
	s := newRelayTestServer(t)

	if code, _ := s.do("POST", "/metevo/invoice-request", map[string]any{
		"recipient_pubkey": relayTestRecipient,
		"amount_usd":       20,
	}, ""); code != http.StatusUnauthorized {
		t.Fatalf("missing bearer token must be rejected, got %d", code)
	}
	if code, _ := s.do("POST", "/metevo/invoice-request", map[string]any{
		"recipient_pubkey": "not-a-pubkey",
		"amount_usd":       20,
	}, relayTestToken); code != http.StatusBadRequest {
		t.Fatalf("invalid recipient must be rejected, got %d", code)
	}
	if code, _ := s.do("POST", "/metevo/invoice-request", map[string]any{
		"recipient_pubkey": relayTestRecipient,
		"amount_usd":       0,
	}, relayTestToken); code != http.StatusBadRequest {
		t.Fatalf("zero amount must be rejected, got %d", code)
	}

	requestID := s.requestInvoice(relayTestRecipient, 20)
	if code, _ := s.answer(requestID, relayTestOther, relayTestInvoice, false); code != http.StatusForbidden {
		t.Fatalf("another node must not answer, got %d", code)
	}
	if code, _ := s.answer(requestID, relayTestRecipient, "not-an-invoice", false); code != http.StatusBadRequest {
		t.Fatalf("malformed invoice must be rejected, got %d", code)
	}
	if code, _ := s.answer("f"+strings.Repeat("0", 63), relayTestRecipient, relayTestInvoice, false); code != http.StatusNotFound {
		t.Fatalf("unknown request must be 404, got %d", code)
	}
	if code, _ := s.poll("f" + strings.Repeat("0", 63)); code != http.StatusNotFound {
		t.Fatalf("polling an unknown request must be 404, got %d", code)
	}
}
