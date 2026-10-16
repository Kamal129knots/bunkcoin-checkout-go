package checkout

// POST /webhooks/stripe — ⑥–⑪: the ONLY place BunkCoin is credited.
//
// Fulfilment happens here, never on the return page: the customer may close the tab,
// and delayed methods (bank debits) only settle later.
//
// How we answer Stripe:
//   2xx → handled (or deliberately ignored); Stripe stops retrying
//   400 → bad signature; retrying won't help
//   5xx → temporary failure (ledger down); Stripe retries with backoff for up to 3 days

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/stripe/stripe-go/v87"
)

var handledEvents = map[stripe.EventType]bool{
	stripe.EventTypeCheckoutSessionCompleted:             true, // finished checkout (paid, or delayed method pending)
	stripe.EventTypeCheckoutSessionAsyncPaymentSucceeded: true, // delayed method (bank debit) cleared
	stripe.EventTypeCheckoutSessionAsyncPaymentFailed:    true, // delayed method bounced
	stripe.EventTypeCheckoutSessionExpired:               true, // nobody paid before expires_at
}

// VerifyStripeEvent checks the signature with stripe-go. We accept events sent under
// any API version: we only read stable checkout.session fields (id, status,
// payment_status, amounts, client_reference_id, metadata). Without this, a webhook
// endpoint or `stripe listen` on a different version than stripe-go v87 (2026-09-30.endive)
// would be rejected with 400.
func VerifyStripeEvent(payload []byte, sigHeader, secret string) (stripe.Event, error) {
	return stripe.ConstructEvent(payload, sigHeader, secret, stripe.WithIgnoreAPIVersionMismatch())
}

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	rid := requestID(r.Context())
	payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		writeError(w, r, badRequest("body_unreadable", "Could not read body."))
		return
	}
	event, err := s.ConstructEvent(payload, r.Header.Get("Stripe-Signature"), s.Cfg.WebhookSecret)
	if err != nil {
		s.Log.Error("WEBHOOK", "⑦ Webhook signature verification FAILED — rejected (400)", "reason", err.Error(),
			"hint", "wrong STRIPE_WEBHOOK_SECRET, or body altered by a proxy", "req", rid)
		writeError(w, r, badRequest("invalid_webhook", "Webhook verification failed."))
		return
	}
	s.Log.Info("WEBHOOK", "⑥ Webhook received: "+string(event.Type), "event", event.ID, "req", rid)
	s.Log.Debug("WEBHOOK", "⑦ Signature verified", "event", event.ID, "api_version", event.APIVersion)

	if apiErr := s.handleEvent(r, event); apiErr != nil {
		writeError(w, r, apiErr)
		return
	}
	writeJSONResponse(w, 200, map[string]bool{"received": true})
}

// ── ⑧ Route the event ─────────────────────────────────────────────────────────

func (s *Server) handleEvent(r *http.Request, event stripe.Event) *APIError {
	ctx := r.Context()
	base := []any{"event", event.ID, "req", requestID(ctx)}
	if !handledEvents[event.Type] {
		s.Log.Info("WEBHOOK", fmt.Sprintf("⑧ Ignored: we don't handle %s (acknowledged 200)", event.Type), base...)
		return nil
	}
	var sess stripe.CheckoutSession
	if err := json.Unmarshal(event.Data.Raw, &sess); err != nil {
		s.Log.Error("WEBHOOK", "⑧ Could not read the session in the event", append(base, "reason", err.Error())...)
		return badRequest("invalid_webhook", "Bad event payload.")
	}
	if sess.Metadata["app"] != metaApp {
		s.Log.Info("WEBHOOK", "⑧ Ignored: session belongs to another integration", append(base, "session", Short(sess.ID))...)
		return nil
	}
	orderID := sess.Metadata["order_id"]
	base = append(base, "order", orderID, "org", sess.Metadata["org_id"])

	setStatus := func(st OrderStatus) {
		o := orderFields(&sess)
		_, _ = s.Store.Update(ctx, orderID, func(cur *Order) { mergeOrder(cur, o); cur.Status = st })
	}

	switch event.Type {
	case stripe.EventTypeCheckoutSessionCompleted:
		if sess.PaymentStatus != stripe.CheckoutSessionPaymentStatusPaid {
			// e.g. bank debit: checkout finished but money hasn't cleared. Wait for async_payment_*.
			s.Log.Info("WEBHOOK", "⑧ Checkout completed but payment NOT yet cleared → awaiting_payment", append(base, "payment_status", sess.PaymentStatus)...)
			setStatus(StatusAwaitingPayment)
			return nil
		}
		s.Log.Info("WEBHOOK", "⑧ Payment confirmed (paid) → fulfilling", base...)
		return s.fulfil(r, &sess, base)

	case stripe.EventTypeCheckoutSessionAsyncPaymentSucceeded:
		s.Log.Info("WEBHOOK", "⑧ Delayed payment cleared → fulfilling", base...)
		return s.fulfil(r, &sess, base)

	case stripe.EventTypeCheckoutSessionAsyncPaymentFailed:
		s.Log.Warn("WEBHOOK", "⑧ Delayed payment FAILED → no coins", base...)
		setStatus(StatusPaymentFailed) // PRODUCTION: notify the organisation's admins with a retry link
		return nil

	case stripe.EventTypeCheckoutSessionExpired:
		s.Log.Info("WEBHOOK", "⑧ Checkout expired unpaid", base...)
		setStatus(StatusExpired)
		return nil
	}
	return nil
}

// ── ⑨–⑪ Credit BunkCoin ────────────────────────────────────────────────────────
//
// Outcomes and how we answer Stripe:
//
//	credited / already credited           → 200 (done)
//	permanent problem (bad metadata,      → 200 + needs_review + ALERT log
//	  amount mismatch, account rejected)     (retrying can never fix it; a human
//	                                          must credit manually or refund)
//	temporary problem (ledger down)       → 500 + credit_retrying
//	                                        (Stripe retries with backoff for up to 3 days)
func (s *Server) fulfil(r *http.Request, sess *stripe.CheckoutSession, base []any) *APIError {
	ctx := r.Context()

	// ⑨ Integrity checks: what Stripe charged and recorded vs. what we priced.
	order, problem := orderFromSession(sess)
	if problem != "" {
		if IsULID(order.OrderID) {
			_, _ = s.Store.Update(ctx, order.OrderID, func(cur *Order) {
				mergeOrder(cur, order)
				cur.Status, cur.FailureReason = StatusNeedsReview, problem
			})
		}
		s.Log.Error("FULFIL", "⑨ ALERT: paid but NOT credited — failed integrity check, needs manual review",
			append(base, "session", Short(sess.ID), "problem", problem, "action", "verify in Stripe Dashboard, then credit manually or refund")...)
		return nil // 200: retrying can't fix bad data
	}
	s.Log.Info("FULFIL", "⑨ Integrity checks passed", append(base, "qty", order.Quantity,
		"check", fmt.Sprintf("%d × %d = %d", order.Quantity, order.UnitAmount, sess.AmountSubtotal), "total", Money(order.Total, order.Currency))...)

	// ⑩ Credit exactly once: Store.Fulfil serialises deliveries per order and skips if
	//    already fulfilled; the ledger is also idempotent by order id.
	alreadyDone, done, err := s.Store.Fulfil(ctx, order, func(o Order) (LedgerEntry, error) {
		s.Log.Info("FULFIL", "⑩ Crediting ledger…", append(base, "qty", o.Quantity)...)
		entry, err := s.Ledger.Credit(ctx, o.OrgID, o.Quantity, o.OrderID)
		if err != nil {
			return entry, err
		}
		kv := []any{"org", o.OrgID, "order", o.OrderID, "balance", fmt.Sprintf("%d → %d", entry.BalanceBefore, entry.BalanceAfter)}
		if entry.Duplicate {
			kv = append(kv, "note", "ledger already had this order")
		}
		s.Log.Info("LEDGER", fmt.Sprintf("⑩ Credited %d BunkCoin", o.Quantity), kv...)
		return entry, nil
	})
	if err != nil {
		if le, ok := asLedgerError(err); ok && le.Permanent {
			_, _ = s.Store.Update(ctx, order.OrderID, func(cur *Order) {
				mergeOrder(cur, order)
				cur.Status, cur.FailureReason = StatusNeedsReview, le.Code
			})
			s.Log.Error("FULFIL", "⑩ ALERT: paid but ledger REJECTED the credit permanently — needs manual review",
				append(base, "reason", le.Code, "detail", le.Msg, "action", "fix the account then credit manually, or refund")...)
			return nil // 200: Stripe retrying won't help
		}
		code := "credit_failed"
		if le, ok := asLedgerError(err); ok {
			code = le.Code
		}
		updated, _ := s.Store.Update(ctx, order.OrderID, func(cur *Order) {
			mergeOrder(cur, order)
			cur.Status, cur.FailureReason = StatusCreditRetrying, code
			cur.Attempts++
		})
		s.Log.Error("FULFIL", fmt.Sprintf("⑩ Temporary failure crediting (attempt %d) → 500, Stripe will retry", updated.Attempts),
			append(base, "reason", err.Error())...)
		// PRODUCTION: alert if attempts > N or the order has been credit_retrying for > 1 hour.
		return &APIError{Status: 500, Code: "fulfilment_failed", Message: "Fulfilment failed; Stripe will retry.", Retryable: true, Cause: err}
	}

	if alreadyDone {
		s.Log.Info("FULFIL", "⑪ Duplicate delivery — already fulfilled, nothing to do", base...)
	} else {
		s.Log.Info("FULFIL", "⑪ Order fulfilled ✓", append(base, "balance_after", *done.BalanceAfter)...)
	}
	return nil
}

// orderFields rebuilds the order from Stripe's copy of our metadata, so fulfilment
// works even if our store lost the row.
func orderFields(sess *stripe.CheckoutSession) Order {
	m := sess.Metadata
	qty, _ := strconv.ParseInt(m["quantity"], 10, 64)
	unit, _ := strconv.ParseInt(m["unit_amount"], 10, 64)
	return Order{
		OrderID: m["order_id"], OrgID: m["org_id"], UserID: m["user_id"], PackageID: m["package_id"],
		Quantity: qty, UnitAmount: unit, Total: sess.AmountTotal, Currency: string(sess.Currency), SessionID: sess.ID,
	}
}

// mergeOrder copies the identifying fields onto an existing (or blank) order row.
func mergeOrder(cur *Order, o Order) {
	cur.OrgID, cur.UserID, cur.PackageID, cur.SessionID = o.OrgID, o.UserID, o.PackageID, o.SessionID
	cur.Quantity, cur.UnitAmount, cur.Total, cur.Currency = o.Quantity, o.UnitAmount, o.Total, o.Currency
	if cur.CreatedAt.IsZero() {
		cur.CreatedAt = time.Now()
	}
}

// orderFromSession rebuilds the order and runs the integrity checks.
// Returns a non-empty problem when the payment must not be credited automatically.
func orderFromSession(sess *stripe.CheckoutSession) (Order, string) {
	o := orderFields(sess)
	m := sess.Metadata
	switch {
	case !IsULID(o.OrderID):
		return o, fmt.Sprintf("missing/invalid order_id in metadata (%q)", o.OrderID)
	case !IsULID(o.OrgID):
		return o, fmt.Sprintf("missing/invalid org_id in metadata (%q)", o.OrgID)
	case o.UserID != "" && !IsULID(o.UserID):
		return o, fmt.Sprintf("invalid user_id in metadata (%q)", o.UserID)
	case sess.ClientReferenceID != o.OrderID:
		return o, fmt.Sprintf("client_reference_id (%s) ≠ metadata.order_id (%s)", sess.ClientReferenceID, o.OrderID)
	case o.Quantity <= 0 || o.UnitAmount <= 0:
		return o, fmt.Sprintf("invalid quantity/unit_amount in metadata (%s × %s)", m["quantity"], m["unit_amount"])
	case sess.AmountSubtotal != o.Quantity*o.UnitAmount:
		return o, fmt.Sprintf("amount mismatch: charged subtotal %d ≠ %d × %d", sess.AmountSubtotal, o.Quantity, o.UnitAmount)
	}
	return o, ""
}
