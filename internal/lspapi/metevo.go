package lspapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"

	"utexo-lsp/pkg/node_client"
)

func (a *API) metevoEnabled() bool {
	return len(a.cfg.SupportedAssetIDs) == 1
}

func (a *API) metevoAssetID() string {
	return strings.TrimSpace(a.cfg.SupportedAssetIDs[0])
}

func (a *API) metevoAuth(w http.ResponseWriter, r *http.Request) bool {
	token := strings.TrimSpace(a.cfg.APayBearerToken)
	if token == "" || r.Header.Get("Authorization") != "Bearer "+token {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	return true
}

func (a *API) handleMetevoBootstrap(w http.ResponseWriter, r *http.Request) {
	if !a.metevoAuth(w, r) {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), a.cfg.HTTPTimeout)
	defer cancel()

	ticker := strings.TrimSpace(r.URL.Query().Get("ticker"))
	if ticker == "" {
		ticker = "USDM"
	}
	name := ticker + " Metevo Dollar"

	listed, err := a.lspClient.ListAssets(ctx, node_client.ListAssetsRequest{
		FilterAssetSchemas: []string{"Ifa", "Nia"},
	})
	if err != nil {
		writeErr(w, http.StatusBadGateway, wrapErr("/listassets", err).Error())
		return
	}
	for _, asset := range listed.IFA {
		if strings.EqualFold(strings.TrimSpace(asset.Ticker), ticker) {
			writeErr(w, http.StatusConflict, fmt.Sprintf(
				"an asset with ticker %s already exists (asset_id %s) — set SUPPORTED_ASSET_IDS to it and restart",
				ticker, asset.AssetID))
			return
		}
	}

	num := a.cfg.MetevoBootstrapUtxoNum
	if num == 0 {
		num = 2
	}
	utxoSize := a.cfg.MetevoBootstrapUtxoSize
	if utxoSize == 0 {
		utxoSize = 30010
	}
	if err := a.lspClient.CreateUtxos(ctx, node_client.CreateUtxosRequest{
		UpTo:     true,
		Num:      &num,
		Size:     &utxoSize,
		FeeRate:  1,
		SkipSync: false,
	}); err != nil {
		writeErr(w, http.StatusBadGateway, wrapErr("/createutxos", err).Error())
		return
	}

	resp, err := a.lspClient.IssueAssetIFA(ctx, node_client.IssueAssetIFARequest{
		Amounts:          []uint64{1_000_000_000_000},
		InflationAmounts: []uint64{100_000_000_000},
		Ticker:           ticker,
		Name:             name,
		Precision:        8,
	})
	if err != nil {
		writeErr(w, http.StatusBadGateway, wrapErr("/issueassetifa", err).Error())
		return
	}

	assetID := strings.TrimSpace(resp.Asset.AssetID)
	writeJSON(w, http.StatusOK, MetevoBootstrapResponse{
		AssetID:   assetID,
		Ticker:    resp.Asset.Ticker,
		Name:      resp.Asset.Name,
		Precision: resp.Asset.Precision,
		Issued:    uint64(resp.Asset.IssuedSupply),
		Config: fmt.Sprintf(
			"SUPPORTED_ASSET_IDS=%s\nMETEVO_USD_SATS_RATE=3000",
			assetID),
	})
}

func (a *API) handleMetevoBuy(w http.ResponseWriter, r *http.Request) {
	if !a.metevoEnabled() {
		writeErr(w, http.StatusNotFound, "metevo is not enabled on this LSP (SUPPORTED_ASSET_IDS must name exactly one asset)")
		return
	}
	if !a.metevoAuth(w, r) {
		return
	}

	var req MetevoBuyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if req.USDAmount == 0 {
		writeErr(w, http.StatusBadRequest, "usd_amount is required")
		return
	}
	if req.USDAmount > math.MaxUint64/1_000_000 {
		writeErr(w, http.StatusBadRequest, "usd_amount is too large")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), a.cfg.HTTPTimeout)
	defer cancel()

	assetID := a.metevoAssetID()
	assetAmount, err := a.metevoAssetAmountForUSD(ctx, assetID, req.USDAmount)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	amountMsat := req.USDAmount * a.cfg.MetevoUSDSatsRate * 1000
	if amountMsat < a.cfg.MinAmtMsat {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf(
			"quoted amount %d msat is below the LSP minimum of %d msat", amountMsat, a.cfg.MinAmtMsat))
		return
	}

	expirySec := uint32(a.cfg.MetevoInvoiceExpiry.Seconds())
	invResp, err := a.lspClient.LNInvoice(ctx, node_client.LNInvoiceRequest{
		AmtMsat:   &amountMsat,
		ExpirySec: expirySec,
	})
	if err != nil {
		writeErr(w, http.StatusBadGateway, wrapErr("could not create invoice", err).Error())
		return
	}
	decoded, err := a.validateLNInvoice(ctx, invResp.Invoice)
	if err != nil {
		writeErr(w, http.StatusBadGateway, wrapErr("created invoice failed validation", err).Error())
		return
	}

	order := MetevoOrder{
		PaymentHash: decoded.PaymentHash,
		USDAmount:   req.USDAmount,
		AssetAmount: assetAmount,
		AmountMsat:  amountMsat,
		Invoice:     invResp.Invoice,
		Status:      metevoOrderStatusPending,
	}
	if err := a.db.InsertMetevoOrder(ctx, order); err != nil {
		if errors.Is(err, errMetevoOrderExists) {
			writeErr(w, http.StatusConflict, "an order already exists for this payment_hash")
			return
		}
		writeErr(w, http.StatusInternalServerError, fmt.Sprintf("error persisting metevo order: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, MetevoBuyResponse{
		PaymentHash: order.PaymentHash,
		Invoice:     order.Invoice,
		AmountMsat:  order.AmountMsat,
		Sats:        order.AmountMsat / 1000,
		USDAmount:   order.USDAmount,
		AssetID:     assetID,
		AssetAmount: order.AssetAmount,
		SatsPerUSD:  a.cfg.MetevoUSDSatsRate,
		ExpiresAt:   decoded.Timestamp + decoded.ExpirySec,
	})
}

func (a *API) metevoAssetAmountForUSD(ctx context.Context, assetID string, usd uint64) (uint64, error) {
	meta, err := a.assetMetadata(ctx, assetID)
	if err != nil {
		return 0, fmt.Errorf("cannot resolve served asset %s: %v", assetID, err)
	}
	if meta.Precision < 0 || meta.Precision > 18 {
		return 0, fmt.Errorf("asset %s has unusable precision %d", assetID, meta.Precision)
	}
	mul := uint64(1)
	for i := 0; i < meta.Precision; i++ {
		mul *= 10
	}
	if usd > math.MaxUint64/mul {
		return 0, errors.New("usd_amount is too large for the asset's precision")
	}
	return usd * mul, nil
}

func (a *API) handleMetevoClaim(w http.ResponseWriter, r *http.Request) {
	if !a.metevoEnabled() {
		writeErr(w, http.StatusNotFound, "metevo is not enabled on this LSP (SUPPORTED_ASSET_IDS must name exactly one asset)")
		return
	}
	if !a.metevoAuth(w, r) {
		return
	}

	var req MetevoClaimRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	paymentHash := strings.ToLower(strings.TrimSpace(req.PaymentHash))
	if !isValidPaymentHash(paymentHash) {
		writeErr(w, http.StatusBadRequest, "invalid payment_hash")
		return
	}
	rgbInvoice := strings.TrimSpace(req.RGBInvoice)
	if rgbInvoice == "" {
		writeErr(w, http.StatusBadRequest, "rgb_invoice is required (blind or witness)")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), a.cfg.HTTPTimeout)
	defer cancel()

	order, err := a.db.LoadMetevoOrderByPaymentHash(ctx, paymentHash)
	if err != nil {
		if errors.Is(err, errMetevoOrderNotFound) {
			writeErr(w, http.StatusNotFound, "no metevo order for this payment_hash")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	if order.Status == metevoOrderStatusDelivered {
		writeJSON(w, http.StatusOK, MetevoClaimResponse{
			PaymentHash: order.PaymentHash,
			AssetID:     a.metevoAssetID(),
			AssetAmount: order.AssetAmount,
			Status:      string(order.Status),
		})
		return
	}

	if err := a.verifyMetevoPayment(ctx, order, req.PaymentPreimage); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}

	decodedRGB, err := a.validateRGBInvoice(ctx, rgbInvoice)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// A blind invoice carries no asset_id and no assignment amount — the wallet
	// is not yet aware of the asset. A witness invoice names both; pin them to
	// the order when it does.
	if decodedRGB.AssetID != nil && *decodedRGB.AssetID != a.metevoAssetID() {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf(
			"rgb_invoice is for %s, this order pays out %s",
			derefString(decodedRGB.AssetID), a.metevoAssetID()))
		return
	}
	if amount, ok := extractFungibleAssignmentAmount(decodedRGB.Assignment); ok && amount > 0 && uint64(amount) != order.AssetAmount {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf(
			"rgb_invoice asks for %d base units but the order covers %d", amount, order.AssetAmount))
		return
	}

	transitioned, err := a.db.AdvanceMetevoOrder(ctx, paymentHash,
		[]string{string(metevoOrderStatusPending), string(metevoOrderStatusPaid)},
		metevoOrderStatusDelivering, "")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !transitioned {
		// Another claim already owns this transition (or the order moved on).
		// Only the caller that won it may deliver the RGB assignment.
		current, loadErr := a.db.LoadMetevoOrderByPaymentHash(ctx, paymentHash)
		if loadErr != nil {
			writeErr(w, http.StatusInternalServerError, loadErr.Error())
			return
		}
		if current.Status == metevoOrderStatusDelivered {
			writeJSON(w, http.StatusOK, MetevoClaimResponse{
				PaymentHash: current.PaymentHash,
				AssetID:     a.metevoAssetID(),
				AssetAmount: current.AssetAmount,
				Status:      string(current.Status),
			})
			return
		}
		writeErr(w, http.StatusConflict, "this claim is already being delivered")
		return
	}

	if err := a.sendRGBByInvoice(ctx, rgbInvoice); err != nil {
		reason := fmt.Sprintf("sendrgb failed: %v", err)
		if _, updErr := a.db.AdvanceMetevoOrder(ctx, paymentHash,
			[]string{string(metevoOrderStatusDelivering)},
			metevoOrderStatusPaid, reason); updErr != nil {
			log.Printf("metevo: persist paid after failed delivery for %s: %v", paymentHash, updErr)
		}
		writeErr(w, http.StatusBadGateway, reason)
		return
	}
	if _, err := a.db.AdvanceMetevoOrder(ctx, paymentHash,
		[]string{string(metevoOrderStatusDelivering)},
		metevoOrderStatusDelivered, ""); err != nil {
		log.Printf("metevo: persist delivered for %s: %v", paymentHash, err)
	}

	writeJSON(w, http.StatusOK, MetevoClaimResponse{
		PaymentHash: order.PaymentHash,
		AssetID:     a.metevoAssetID(),
		AssetAmount: order.AssetAmount,
		Status:      string(metevoOrderStatusDelivered),
	})
}

func (a *API) verifyMetevoPayment(ctx context.Context, order MetevoOrder, preimage string) error {
	preimage = strings.ToLower(strings.TrimSpace(preimage))
	if preimage != "" {
		preimageBytes, err := hex.DecodeString(preimage)
		if err != nil || len(preimageBytes) != sha256.Size {
			return errors.New("invalid payment_preimage")
		}
		sum := sha256.Sum256(preimageBytes)
		if hex.EncodeToString(sum[:]) != order.PaymentHash {
			return errors.New("payment_preimage does not match payment_hash")
		}
		return nil
	}

	payments, err := a.lspClient.ListPayments(ctx)
	if err != nil {
		return wrapErr("/listpayments", err)
	}
	for _, p := range payments.Payments {
		if !strings.EqualFold(p.PaymentHash, order.PaymentHash) {
			continue
		}
		if p.Inbound && p.Status == node_client.PaymentStatusSucceeded {
			return nil
		}
		return fmt.Errorf("payment for %s is %s, not settled", order.PaymentHash, p.Status)
	}
	return fmt.Errorf("no settled inbound payment found for %s — pay the invoice from /metevo/buy first", order.PaymentHash)
}
