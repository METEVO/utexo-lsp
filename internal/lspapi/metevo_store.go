package lspapi

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

var (
	errMetevoOrderNotFound = errors.New("metevo order not found")
	errMetevoOrderExists   = errors.New("metevo order already exists")
)

var metevoOrderStatusRank = map[MetevoOrderStatus]int{
	metevoOrderStatusPending:    0,
	metevoOrderStatusPaid:       1,
	metevoOrderStatusDelivering: 2,
	metevoOrderStatusDelivered:  3,
}

func (s *SQLStore) InsertMetevoOrder(ctx context.Context, order MetevoOrder) error {
	paymentHash := strings.ToLower(strings.TrimSpace(order.PaymentHash))
	if !isValidPaymentHash(paymentHash) {
		return errors.New("invalid payment_hash")
	}
	if order.Status == "" {
		order.Status = metevoOrderStatusPending
	}
	if s.driver == "postgres" {
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO metevo_orders (payment_hash, usd_amount, asset_amount, amount_msat, invoice, status)
			VALUES ($1, $2, $3, $4, $5, $6)
		`, paymentHash, order.USDAmount, order.AssetAmount, order.AmountMsat, order.Invoice, string(order.Status))
		if isUniqueViolation(err) {
			return errMetevoOrderExists
		}
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO metevo_orders (payment_hash, usd_amount, asset_amount, amount_msat, invoice, status)
		VALUES (?, ?, ?, ?, ?, ?)
	`, paymentHash, order.USDAmount, order.AssetAmount, order.AmountMsat, order.Invoice, string(order.Status))
	if isUniqueViolation(err) {
		return errMetevoOrderExists
	}
	return err
}

func (s *SQLStore) LoadMetevoOrderByPaymentHash(ctx context.Context, paymentHash string) (MetevoOrder, error) {
	query := `
		SELECT id, payment_hash, usd_amount, asset_amount, amount_msat, invoice, status, last_error, created_at, updated_at
		FROM metevo_orders WHERE payment_hash = ? LIMIT 1
	`
	if s.driver == "postgres" {
		query = strings.Replace(query, "= ?", "= $1", 1)
	}
	row := s.db.QueryRowContext(ctx, query, strings.ToLower(strings.TrimSpace(paymentHash)))
	return scanMetevoOrder(row)
}

type metevoOrderScanner interface {
	Scan(dest ...any) error
}

func scanMetevoOrder(row metevoOrderScanner) (MetevoOrder, error) {
	var (
		o         MetevoOrder
		status    string
		lastError sql.NullString
		createdAt time.Time
		updatedAt time.Time
	)
	err := row.Scan(&o.ID, &o.PaymentHash, &o.USDAmount, &o.AssetAmount, &o.AmountMsat,
		&o.Invoice, &status, &lastError, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return MetevoOrder{}, errMetevoOrderNotFound
	}
	if err != nil {
		return MetevoOrder{}, err
	}
	o.Status = MetevoOrderStatus(strings.TrimSpace(status))
	if lastError.Valid {
		s := strings.TrimSpace(lastError.String)
		if s != "" {
			o.LastError = &s
		}
	}
	o.CreatedAt, o.UpdatedAt = createdAt, updatedAt
	return o, nil
}

func (s *SQLStore) AdvanceMetevoOrder(ctx context.Context, paymentHash string, from []string, to MetevoOrderStatus, lastErr string) (bool, error) {
	if metevoOrderStatusRank[to] == 0 {
		return false, fmt.Errorf("unknown target metevo order status %q", to)
	}
	paymentHash = strings.ToLower(strings.TrimSpace(paymentHash))

	current, err := s.LoadMetevoOrderByPaymentHash(ctx, paymentHash)
	if err != nil {
		return false, err
	}
	currentStatus := string(current.Status)
	if !slices.Contains(from, currentStatus) {
		return false, nil
	}

	if s.driver == "postgres" {
		_, err = s.db.ExecContext(ctx, `
			UPDATE metevo_orders SET status=$1, last_error=$2, updated_at=NOW() WHERE payment_hash=$3 AND status=$4
		`, string(to), nullIfEmpty(lastErr), paymentHash, currentStatus)
	} else {
		_, err = s.db.ExecContext(ctx, `
			UPDATE metevo_orders SET status=?, last_error=?, updated_at=CURRENT_TIMESTAMP WHERE payment_hash=? AND status=?
		`, string(to), nullIfEmpty(lastErr), paymentHash, currentStatus)
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
