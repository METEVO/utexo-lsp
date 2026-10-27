package lspapi

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// Invoice relay: a payer asks the LSP to reach a recipient who is not
// reachable over Lightning directly, the recipient answers with a real
// wallet.createInvoice payload, and the payer settles it through its own
// node. The LSP only stores the request and the answer; it never touches
// either node, so it works with client nodes behind NAT.
//
// Auth is the shared APay bearer token plus an unguessable request id: the
// demo deployment has one token for both phones. A production relay needs
// per-wallet tokens and sender authentication.

const (
	invoiceRelayPending   = "pending"
	invoiceRelayDelivered = "delivered"
	invoiceRelayDeclined  = "declined"
	invoiceRelayExpired   = "expired"

	invoiceRelayDefaultExpiry = 10 * time.Minute
	maxInvoiceRelayUSD        = 1_000_000
	maxInvoiceRelayAssetID    = 200
	maxInvoiceRelayInvoiceLen = 4096
	maxInvoiceRelayBodyBytes  = 64 << 10
	// The inbox is a UI list, not an export: cap one poll so a spammer
	// cannot make the recipient app parse an unbounded body.
	maxInvoiceRelayInboxRows = 50
)

var errInvoiceRelayNotFound = errors.New("invoice relay request not found")

type InvoiceRelayRequest struct {
	RecipientPubkey string `json:"recipient_pubkey"`
	SenderPubkey    string `json:"sender_pubkey,omitempty"`
	USDAmount       uint64 `json:"amount_usd"`
	AssetID         string `json:"asset_id,omitempty"`
}

type InvoiceRelayAck struct {
	RequestID string `json:"request_id"`
	// Unix milliseconds, like every other client timestamp.
	ExpiresAt int64 `json:"expires_at"`
}

type InvoiceRelayPollRequest struct {
	RequestID string `json:"request_id"`
}

type InvoiceRelayPollResponse struct {
	Status      string `json:"status"`
	Invoice     string `json:"invoice,omitempty"`
	PaymentHash string `json:"payment_hash,omitempty"`
}

type InvoiceRelayInboxEntry struct {
	RequestID    string `json:"request_id"`
	SenderPubkey string `json:"sender_pubkey,omitempty"`
	USDAmount    uint64 `json:"amount_usd"`
	AssetID      string `json:"asset_id,omitempty"`
	ExpiresAt    int64  `json:"expires_at"`
}

type InvoiceRelayInboxResponse struct {
	Requests []InvoiceRelayInboxEntry `json:"requests"`
}

type InvoiceRelayAnswer struct {
	RequestID       string `json:"request_id"`
	RecipientPubkey string `json:"recipient_pubkey"`
	Invoice         string `json:"invoice,omitempty"`
	PaymentHash     string `json:"payment_hash,omitempty"`
	Declined        bool   `json:"declined,omitempty"`
}

type invoiceRelayRecord struct {
	RequestID       string
	SenderPubkey    string
	RecipientPubkey string
	USDAmount       uint64
	AssetID         string
	Status          string
	Invoice         string
	PaymentHash     string
	ExpiresAt       time.Time
	CreatedAt       time.Time
}

func newInvoiceRelayID() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func normalizeRelayPubkey(value string) (string, bool) {
	pubkey := strings.ToLower(strings.TrimSpace(value))
	if len(pubkey) != 66 {
		return "", false
	}
	// Compressed secp256k1 key: 02 or 03 followed by 32 bytes.
	if pubkey[0] != '0' || (pubkey[1] != '2' && pubkey[1] != '3') {
		return "", false
	}
	decoded, err := hex.DecodeString(pubkey)
	if err != nil || len(decoded) != 33 {
		return "", false
	}
	return pubkey, true
}

func (a *API) handleInvoiceRelayRequest(w http.ResponseWriter, r *http.Request) {
	if !a.metevoAuth(w, r) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxInvoiceRelayBodyBytes)
	var req InvoiceRelayRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	recipient, ok := normalizeRelayPubkey(req.RecipientPubkey)
	if !ok {
		writeErr(w, http.StatusBadRequest, "recipient_pubkey must be a 66-hex compressed node pubkey")
		return
	}
	sender := ""
	if strings.TrimSpace(req.SenderPubkey) != "" {
		normalized, ok := normalizeRelayPubkey(req.SenderPubkey)
		if !ok {
			writeErr(w, http.StatusBadRequest, "sender_pubkey must be a 66-hex compressed node pubkey")
			return
		}
		sender = normalized
	}
	if req.USDAmount == 0 || req.USDAmount > maxInvoiceRelayUSD {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("amount_usd must be between 1 and %d", maxInvoiceRelayUSD))
		return
	}
	assetID := strings.TrimSpace(req.AssetID)
	if len(assetID) > maxInvoiceRelayAssetID {
		writeErr(w, http.StatusBadRequest, "asset_id is too long")
		return
	}

	requestID, err := newInvoiceRelayID()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not allocate a request id")
		return
	}
	expiry := a.cfg.MetevoInvoiceExpiry
	if expiry <= 0 {
		expiry = invoiceRelayDefaultExpiry
	}
	now := time.Now()
	record := invoiceRelayRecord{
		RequestID:       requestID,
		SenderPubkey:    sender,
		RecipientPubkey: recipient,
		USDAmount:       req.USDAmount,
		AssetID:         assetID,
		Status:          invoiceRelayPending,
		ExpiresAt:       now.Add(expiry),
		CreatedAt:       now,
	}

	ctx, cancel := context.WithTimeout(r.Context(), a.cfg.HTTPTimeout)
	defer cancel()
	if err := a.db.InsertInvoiceRelayRequest(ctx, record); err != nil {
		log.Printf("invoice relay: insert request: %v", err)
		writeErr(w, http.StatusInternalServerError, "could not store the invoice request")
		return
	}

	writeJSON(w, http.StatusOK, InvoiceRelayAck{
		RequestID: record.RequestID,
		ExpiresAt: record.ExpiresAt.UnixMilli(),
	})
}

// handleInvoiceRelayResponse is the payer's poll: it only reads.
func (a *API) handleInvoiceRelayResponse(w http.ResponseWriter, r *http.Request) {
	if !a.metevoAuth(w, r) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxInvoiceRelayBodyBytes)
	var req InvoiceRelayPollRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	requestID := strings.ToLower(strings.TrimSpace(req.RequestID))
	if !isValidPaymentHash(requestID) {
		writeErr(w, http.StatusBadRequest, "request_id must be a 64-hex id")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), a.cfg.HTTPTimeout)
	defer cancel()
	record, err := a.db.LoadInvoiceRelayRequest(ctx, requestID)
	if errors.Is(err, errInvoiceRelayNotFound) {
		writeErr(w, http.StatusNotFound, "unknown request_id")
		return
	}
	if err != nil {
		log.Printf("invoice relay: load request: %v", err)
		writeErr(w, http.StatusInternalServerError, "could not load the invoice request")
		return
	}

	status := record.Status
	if status == invoiceRelayPending && time.Now().After(record.ExpiresAt) {
		status = invoiceRelayExpired
	}
	writeJSON(w, http.StatusOK, InvoiceRelayPollResponse{
		Status:      status,
		Invoice:     record.Invoice,
		PaymentHash: record.PaymentHash,
	})
}

func (a *API) handleInvoiceRelayInbox(w http.ResponseWriter, r *http.Request) {
	if !a.metevoAuth(w, r) {
		return
	}
	pubkey, ok := normalizeRelayPubkey(r.URL.Query().Get("pubkey"))
	if !ok {
		writeErr(w, http.StatusBadRequest, "pubkey must be a 66-hex compressed node pubkey")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), a.cfg.HTTPTimeout)
	defer cancel()
	records, err := a.db.ListPendingInvoiceRelayRequests(ctx, pubkey, time.Now())
	if err != nil {
		log.Printf("invoice relay: list requests: %v", err)
		writeErr(w, http.StatusInternalServerError, "could not list the invoice requests")
		return
	}

	requests := make([]InvoiceRelayInboxEntry, 0, len(records))
	for _, record := range records {
		requests = append(requests, InvoiceRelayInboxEntry{
			RequestID:    record.RequestID,
			SenderPubkey: record.SenderPubkey,
			USDAmount:    record.USDAmount,
			AssetID:      record.AssetID,
			ExpiresAt:    record.ExpiresAt.UnixMilli(),
		})
	}
	writeJSON(w, http.StatusOK, InvoiceRelayInboxResponse{Requests: requests})
}

// handleInvoiceRelayAnswer is where the recipient's app delivers the invoice.
func (a *API) handleInvoiceRelayAnswer(w http.ResponseWriter, r *http.Request) {
	if !a.metevoAuth(w, r) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxInvoiceRelayBodyBytes)
	var req InvoiceRelayAnswer
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	requestID := strings.ToLower(strings.TrimSpace(req.RequestID))
	if !isValidPaymentHash(requestID) {
		writeErr(w, http.StatusBadRequest, "request_id must be a 64-hex id")
		return
	}
	pubkey, ok := normalizeRelayPubkey(req.RecipientPubkey)
	if !ok {
		writeErr(w, http.StatusBadRequest, "recipient_pubkey must be a 66-hex compressed node pubkey")
		return
	}

	status := invoiceRelayDelivered
	invoice := ""
	paymentHash := ""
	if req.Declined {
		status = invoiceRelayDeclined
	} else {
		invoice = strings.TrimSpace(req.Invoice)
		if len(invoice) < 10 || len(invoice) > maxInvoiceRelayInvoiceLen || !strings.HasPrefix(invoice, "ln") {
			writeErr(w, http.StatusBadRequest, "invoice must be a bolt11 string")
			return
		}
		paymentHash = strings.ToLower(strings.TrimSpace(req.PaymentHash))
		if paymentHash != "" && !isValidPaymentHash(paymentHash) {
			writeErr(w, http.StatusBadRequest, "payment_hash must be a 64-hex hash")
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), a.cfg.HTTPTimeout)
	defer cancel()
	record, err := a.db.LoadInvoiceRelayRequest(ctx, requestID)
	if errors.Is(err, errInvoiceRelayNotFound) {
		writeErr(w, http.StatusNotFound, "unknown request_id")
		return
	}
	if err != nil {
		log.Printf("invoice relay: load request: %v", err)
		writeErr(w, http.StatusInternalServerError, "could not load the invoice request")
		return
	}
	if record.RecipientPubkey != pubkey {
		writeErr(w, http.StatusForbidden, "request belongs to another recipient")
		return
	}
	if record.Status != invoiceRelayPending {
		writeErr(w, http.StatusConflict, "request was already answered")
		return
	}
	if time.Now().After(record.ExpiresAt) {
		writeErr(w, http.StatusConflict, "request expired")
		return
	}

	updated, err := a.db.AnswerInvoiceRelayRequest(ctx, requestID, pubkey, status, invoice, paymentHash, time.Now())
	if err != nil {
		log.Printf("invoice relay: save answer: %v", err)
		writeErr(w, http.StatusInternalServerError, "could not store the invoice answer")
		return
	}
	if !updated {
		writeErr(w, http.StatusConflict, "request was already answered")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *SQLStore) InsertInvoiceRelayRequest(ctx context.Context, record invoiceRelayRecord) error {
	if s.driver == "postgres" {
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO metevo_invoice_requests
				(request_id, sender_pubkey, recipient_pubkey, usd_amount, asset_id, status, expires_at, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		`, record.RequestID, record.SenderPubkey, record.RecipientPubkey, record.USDAmount,
			record.AssetID, record.Status, record.ExpiresAt.UnixMilli(), record.CreatedAt.UnixMilli())
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO metevo_invoice_requests
			(request_id, sender_pubkey, recipient_pubkey, usd_amount, asset_id, status, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, record.RequestID, record.SenderPubkey, record.RecipientPubkey, record.USDAmount,
		record.AssetID, record.Status, record.ExpiresAt.UnixMilli(), record.CreatedAt.UnixMilli())
	return err
}

func (s *SQLStore) LoadInvoiceRelayRequest(ctx context.Context, requestID string) (invoiceRelayRecord, error) {
	query := `
		SELECT request_id, sender_pubkey, recipient_pubkey, usd_amount, asset_id, status,
			invoice, payment_hash, expires_at, created_at
		FROM metevo_invoice_requests WHERE request_id = ? LIMIT 1
	`
	if s.driver == "postgres" {
		query = strings.Replace(query, "= ?", "= $1", 1)
	}
	var (
		record      invoiceRelayRecord
		sender      sql.NullString
		assetID     sql.NullString
		invoice     sql.NullString
		paymentHash sql.NullString
		expiresAt   int64
		createdAt   int64
	)
	err := s.db.QueryRowContext(ctx, query, requestID).Scan(
		&record.RequestID, &sender, &record.RecipientPubkey, &record.USDAmount, &assetID,
		&record.Status, &invoice, &paymentHash, &expiresAt, &createdAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return invoiceRelayRecord{}, errInvoiceRelayNotFound
	}
	if err != nil {
		return invoiceRelayRecord{}, err
	}
	record.SenderPubkey = sender.String
	record.AssetID = assetID.String
	record.Invoice = invoice.String
	record.PaymentHash = paymentHash.String
	record.ExpiresAt = time.UnixMilli(expiresAt)
	record.CreatedAt = time.UnixMilli(createdAt)
	return record, nil
}

func (s *SQLStore) ListPendingInvoiceRelayRequests(ctx context.Context, recipientPubkey string, now time.Time) ([]invoiceRelayRecord, error) {
	query := `
		SELECT request_id, sender_pubkey, recipient_pubkey, usd_amount, asset_id, status,
			invoice, payment_hash, expires_at, created_at
		FROM metevo_invoice_requests
		WHERE recipient_pubkey = ? AND status = ? AND expires_at > ?
		ORDER BY created_at ASC
		LIMIT ?
	`
	if s.driver == "postgres" {
		query = `
			SELECT request_id, sender_pubkey, recipient_pubkey, usd_amount, asset_id, status,
				invoice, payment_hash, expires_at, created_at
			FROM metevo_invoice_requests
			WHERE recipient_pubkey = $1 AND status = $2 AND expires_at > $3
			ORDER BY created_at ASC
			LIMIT $4
		`
	}
	rows, err := s.db.QueryContext(
		ctx, query, recipientPubkey, invoiceRelayPending, now.UnixMilli(), maxInvoiceRelayInboxRows)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []invoiceRelayRecord
	for rows.Next() {
		var (
			record      invoiceRelayRecord
			sender      sql.NullString
			assetID     sql.NullString
			invoice     sql.NullString
			paymentHash sql.NullString
			expiresAt   int64
			createdAt   int64
		)
		if err := rows.Scan(
			&record.RequestID, &sender, &record.RecipientPubkey, &record.USDAmount, &assetID,
			&record.Status, &invoice, &paymentHash, &expiresAt, &createdAt,
		); err != nil {
			return nil, err
		}
		record.SenderPubkey = sender.String
		record.AssetID = assetID.String
		record.Invoice = invoice.String
		record.PaymentHash = paymentHash.String
		record.ExpiresAt = time.UnixMilli(expiresAt)
		record.CreatedAt = time.UnixMilli(createdAt)
		records = append(records, record)
	}
	return records, rows.Err()
}

func (s *SQLStore) AnswerInvoiceRelayRequest(
	ctx context.Context,
	requestID, recipientPubkey, status, invoice, paymentHash string,
	now time.Time,
) (bool, error) {
	query := `
		UPDATE metevo_invoice_requests
		SET status = ?, invoice = ?, payment_hash = ?, answered_at = ?
		WHERE request_id = ? AND recipient_pubkey = ? AND status = ? AND expires_at > ?
	`
	args := []any{status, invoice, paymentHash, now.UnixMilli(), requestID, recipientPubkey, invoiceRelayPending, now.UnixMilli()}
	if s.driver == "postgres" {
		query = `
			UPDATE metevo_invoice_requests
			SET status = $1, invoice = $2, payment_hash = $3, answered_at = $4
			WHERE request_id = $5 AND recipient_pubkey = $6 AND status = $7 AND expires_at > $8
		`
	}
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}
