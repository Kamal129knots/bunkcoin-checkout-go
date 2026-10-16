package checkout

// HTTP routes and the checkout flow.
//
// Identity: { UserID, OrgID } (ULIDs) from the verified session — see auth.go.
// Orders:   each purchase gets an order ULID that ties together our order row,
//           the Stripe Checkout Session and the ledger credit.
//
// Flow (each step is logged with its number):
//   CHECKOUT  ① requested → ② priced → ③ order reserved → ④ Stripe customer → ⑤ session created & linked
//   (customer pays inside Stripe's iframe — nothing reaches this server)
//   WEBHOOK   ⑥ event received → ⑦ signature verified → ⑧ routed           (webhook.go)
//   FULFIL    ⑨ integrity checks → ⑩ ledger credit (exactly once) → ⑪ order fulfilled
//   STATUS    return page polls GET /api/checkout-sessions/{id} until fulfilment = "fulfilled"
//
// Comments marked "PRODUCTION:" say what to change for the real deployment.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/stripe/stripe-go/v87"
)

const metaApp = "bunkcoin_checkout" // tags our sessions; other Checkout traffic on the account is ignored

var (
	idemKeyRe   = regexp.MustCompile(`^[A-Za-z0-9_-]{8,100}$`)
	sessionIDRe = regexp.MustCompile(`^cs_(test|live)_[A-Za-z0-9]{10,200}$`)
)

// SessionsAPI and CustomersAPI are the parts of stripe-go we use
// (sc.V1CheckoutSessions / sc.V1Customers satisfy them), so tests can use fakes.
type SessionsAPI interface {
	Create(ctx context.Context, p *stripe.CheckoutSessionCreateParams) (*stripe.CheckoutSession, error)
	Retrieve(ctx context.Context, id string, p *stripe.CheckoutSessionRetrieveParams) (*stripe.CheckoutSession, error)
}
type CustomersAPI interface {
	Create(ctx context.Context, p *stripe.CustomerCreateParams) (*stripe.Customer, error)
}

// Server holds the dependencies. Everything is injected so tests can pass fakes.
type Server struct {
	Cfg            *Config
	Sessions       SessionsAPI
	Customers      CustomersAPI
	Store          OrderStore // PRODUCTION: Postgres
	Ledger         Ledger     // PRODUCTION: your BunkCoin balance service
	Auth           *Auth
	Log            *Logger
	ConstructEvent func(payload []byte, sigHeader, secret string) (stripe.Event, error)
	StripeTimeout  time.Duration
}

// Routes builds the HTTP handler. static serves the web/ folder.
func (s *Server) Routes(static fs.FS) http.Handler {
	limiter := newRateLimiter(20, time.Minute)
	mux := http.NewServeMux()

	// Webhook: no session needed; signature verification proves it's Stripe.
	mux.HandleFunc("POST /webhooks/stripe", s.handleWebhook)

	api := http.NewServeMux()
	api.HandleFunc("GET /api/config", s.handleConfig)
	api.HandleFunc("GET /api/me", s.handleMe)
	api.HandleFunc("GET /api/orders", s.requireAuth(s.handleOrders))
	api.HandleFunc("POST /api/checkout-sessions", s.requireAuth(limiter.wrap(s.handleCreateSession)))
	api.HandleFunc("GET /api/checkout-sessions/{id}", s.requireAuth(s.handleSessionStatus))
	api.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, newAPIError(404, "not_found", "Unknown API endpoint."))
	})
	mux.Handle("/api/", s.Auth.Middleware(api))

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResponse(w, 200, map[string]string{"status": "ok"})
	})
	// Pages also go through Auth so the dev session cookie is issued on first visit.
	mux.Handle("/", s.Auth.Middleware(http.FileServerFS(static)))

	return s.withBasics(mux)
}

// ── Middleware: request id, security headers, access log, panic recovery ─────

type ridKey struct{}

func requestID(ctx context.Context) string { id, _ := ctx.Value(ridKey{}).(string); return id }

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) { s.status = code; s.ResponseWriter.WriteHeader(code) }

func (s *Server) withBasics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 6)
		_, _ = rand.Read(b)
		id := "req_" + hex.EncodeToString(b)
		r = r.WithContext(context.WithValue(r.Context(), ridKey{}, id))
		h := w.Header()
		h.Set("X-Request-Id", id)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("X-Frame-Options", "DENY") // our page isn't framed; Stripe frames *inside* it

		rec := &statusRecorder{ResponseWriter: w, status: 200}
		start := time.Now()
		defer func() {
			if p := recover(); p != nil {
				s.Log.Error("SYSTEM", "panic", "panic", fmt.Sprint(p), "stack", string(debug.Stack()), "req", id)
				writeError(rec, r, newAPIError(500, "internal_error", "Something went wrong on our side. Please try again later."))
			}
			// Routine page reads are debug; writes and errors are shown.
			msg := fmt.Sprintf("%s %s → %d", r.Method, r.URL.Path, rec.status)
			kv := []any{"ms", time.Since(start).Milliseconds(), "req", id}
			switch {
			case rec.status >= 500:
				s.Log.Error("HTTP", msg, kv...)
			case rec.status >= 400:
				s.Log.Warn("HTTP", msg, kv...)
			case r.Method == http.MethodGet:
				s.Log.Debug("HTTP", msg, kv...)
			default:
				s.Log.Info("HTTP", msg, kv...)
			}
		}()
		next.ServeHTTP(rec, r)
	})
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if IdentityFrom(r.Context()) == nil {
			s.Log.Warn("AUTH", "Rejected: no valid session", "path", r.URL.Path, "req", requestID(r.Context()))
			writeError(w, r, newAPIError(401, "not_signed_in", "Please sign in to 129Knots to continue."))
			return
		}
		next(w, r)
	}
}

// ── GET /api/config ───────────────────────────────────────────────────────────

func (s *Server) handleConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSONResponse(w, 200, map[string]any{
		"publishable_key": s.Cfg.PublishableKey,
		"currency":        s.Cfg.Currency,
		"unit_amount":     s.Cfg.UnitAmount,
		"min_quantity":    s.Cfg.MinQty,
		"max_quantity":    s.Cfg.MaxQty,
		"packages":        s.Cfg.Packages,
	})
}

// ── GET /api/me — session identity + balance ──────────────────────────────────

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	id := IdentityFrom(r.Context())
	if id == nil {
		writeJSONResponse(w, 200, map[string]any{"signed_in": false})
		return
	}
	bal, err := s.Ledger.Balance(r.Context(), id.OrgID) // PRODUCTION: your balance service
	if err != nil {
		writeError(w, r, &APIError{Status: 503, Code: "balance_unavailable", Message: "Your balance is temporarily unavailable.", Retryable: true})
		return
	}
	writeJSONResponse(w, 200, map[string]any{"signed_in": true, "user_id": id.UserID, "org_id": id.OrgID, "balance": bal})
}

// ── GET /api/orders — purchase history for the session's organisation ───────

func (s *Server) handleOrders(w http.ResponseWriter, r *http.Request) {
	id := IdentityFrom(r.Context())
	orders, err := s.Store.ListByOrg(r.Context(), id.OrgID, 10)
	if err != nil {
		writeError(w, r, newAPIError(500, "internal_error", "Couldn't load purchase history."))
		return
	}
	out := make([]map[string]any, 0, len(orders))
	for _, o := range orders {
		out = append(out, map[string]any{
			"order_id": o.OrderID, "quantity": o.Quantity, "total": o.Total, "currency": o.Currency,
			"status": o.Status, "created_at": o.CreatedAt, "credited_at": o.CreditedAt,
		})
	}
	writeJSONResponse(w, 200, map[string]any{"orders": out})
}

// ── POST /api/checkout-sessions — ①–⑤ ────────────────────────────────────────

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := IdentityFrom(ctx)
	rid := requestID(ctx)
	base := []any{"org", id.OrgID, "user", id.UserID, "req", rid}

	var req PurchaseRequest
	raw, apiErr := decodeJSON(w, r, &req, 4<<10)
	if apiErr != nil {
		writeError(w, r, apiErr)
		return
	}
	s.Log.Info("CHECKOUT", "① Checkout requested", append(base, "choice", raw)...)

	// ② Price it on the server. The browser only ever sends a choice — never an amount or an account.
	quote, err := BuildQuote(s.Cfg, req)
	if err != nil {
		var fe *FieldError
		errors.As(err, &fe)
		s.Log.Warn("CHECKOUT", "② Validation failed", append(base, "field", fe.Field, "reason", fe.Message)...)
		writeError(w, r, validationError(fe))
		return
	}
	s.Log.Info("CHECKOUT", "② Priced the order", append(base, "package", quote.PackageID, "qty", quote.Quantity,
		"unit", Money(quote.UnitAmount, quote.Currency), "total", Money(quote.Total, quote.Currency))...)

	// Idempotency key = browser's key + who + what. The same click retried → same key.
	clientKey := r.Header.Get("Idempotency-Key")
	if clientKey != "" && !idemKeyRe.MatchString(clientKey) {
		writeError(w, r, badRequest("invalid_idempotency_key", "Idempotency-Key must be 8–100 letters, digits, '-' or '_'."))
		return
	}
	if clientKey == "" {
		clientKey = "srv_" + NewULID()
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{id.OrgID, id.UserID, quote.PackageID,
		strconv.FormatInt(quote.Quantity, 10), strconv.FormatInt(quote.UnitAmount, 10), quote.Currency}, "|")))
	idemKey := "bkc-" + clientKey + "-" + hex.EncodeToString(sum[:8])

	// ③ Reserve the order BEFORE calling Stripe. A retry with the same key reuses the
	//    same order — same order_id and expires_at — so the Stripe request is identical,
	//    which Stripe's idempotency requires.
	existing, err := s.Store.FindByIdempotencyKey(ctx, idemKey)
	if err != nil {
		writeError(w, r, newAPIError(500, "internal_error", "Something went wrong on our side. Please try again later."))
		return
	}
	var order Order
	if existing != nil {
		order = *existing
		s.Log.Info("CHECKOUT", "③ Retry of an earlier attempt — reusing order", append(base, "order", order.OrderID)...)
	} else {
		order, err = s.Store.Create(ctx, Order{
			OrderID: NewULID(), OrgID: id.OrgID, UserID: id.UserID, PackageID: quote.PackageID,
			Quantity: quote.Quantity, UnitAmount: quote.UnitAmount, Total: quote.Total, Currency: quote.Currency,
			Status: StatusPending, IdempotencyKey: idemKey,
			ExpiresAt: time.Now().Add(time.Duration(s.Cfg.SessionTTLMinutes) * time.Minute).Unix(),
		})
		if err != nil {
			writeError(w, r, newAPIError(500, "internal_error", "Something went wrong on our side. Please try again later."))
			return
		}
		s.Log.Info("CHECKOUT", "③ Order reserved → pending", append(base, "order", order.OrderID)...)
	}
	base = append(base, "order", order.OrderID)

	// ④ One Stripe Customer per organisation.
	customerID, apiErr := s.stripeCustomerFor(ctx, id.OrgID, base)
	if apiErr != nil {
		writeError(w, r, apiErr)
		return
	}

	// Metadata = OUR identifiers stored on Stripe's objects. Not shown to the customer;
	// it comes back in the webhook so we know which order to fulfil and whom to credit.
	// Only ULIDs and numbers — no names or emails (limits: 50 keys, 500 chars per value).
	qtyStr := strconv.FormatInt(quote.Quantity, 10)
	metadata := map[string]string{
		"app": metaApp, "order_id": order.OrderID, "org_id": id.OrgID, "user_id": id.UserID,
		"package_id": quote.PackageID, "quantity": qtyStr, "unit_amount": strconv.FormatInt(quote.UnitAmount, 10),
	}

	// ⑤ Create the session.
	params := &stripe.CheckoutSessionCreateParams{
		UIMode:            stripe.String(string(stripe.CheckoutSessionUIModeEmbeddedPage)), // Stripe's form inside our page
		Mode:              stripe.String(string(stripe.CheckoutSessionModePayment)),        // one-time payment
		Customer:          stripe.String(customerID),
		ClientReferenceID: stripe.String(order.OrderID), // Stripe's field for "your id for this purchase"
		SubmitType:        stripe.String("pay"),
		ReturnURL:         stripe.String(s.Cfg.PublicBaseURL + "/return.html?session_id={CHECKOUT_SESSION_ID}"),
		ExpiresAt:         stripe.Int64(order.ExpiresAt),
		// What the customer SEES (Stripe computes amount = quantity × unit_amount):
		LineItems: []*stripe.CheckoutSessionCreateLineItemParams{{
			Quantity: stripe.Int64(quote.Quantity),
			PriceData: &stripe.CheckoutSessionCreateLineItemPriceDataParams{
				Currency:   stripe.String(quote.Currency),
				UnitAmount: stripe.Int64(quote.UnitAmount),
				ProductData: &stripe.CheckoutSessionCreateLineItemPriceDataProductDataParams{
					Name: stripe.String("BunkCoin"), Description: stripe.String("129Knots platform credits"), UnitLabel: stripe.String("coin"),
				},
			},
		}},
		Metadata: metadata,
		// Same identifiers on the PaymentIntent, so they show on the payment (Dashboard → Payments).
		PaymentIntentData: &stripe.CheckoutSessionCreatePaymentIntentDataParams{
			Description: stripe.String("BunkCoin order " + order.OrderID),
			Metadata:    map[string]string{"app": metaApp, "order_id": order.OrderID, "org_id": id.OrgID, "user_id": id.UserID, "quantity": qtyStr},
		},
		// No PaymentMethodTypes: removed in 2026-09-30.endive. Methods come from
		// Dashboard → Settings → Payment methods (AllowedPaymentMethodTypes restricts in code).
	}
	params.SetIdempotencyKey(order.IdempotencyKey)

	sctx, cancel := context.WithTimeout(ctx, s.StripeTimeout)
	defer cancel()
	t0 := time.Now()
	sess, err := s.Sessions.Create(sctx, params)
	if err != nil {
		e := fromStripe(err)
		s.Log.Error("STRIPE", "⑤ Stripe refused to create the session",
			append([]any{"order", order.OrderID, "ms", time.Since(t0).Milliseconds(), "code", e.Code, "req", rid}, stripeLogFields(err)...)...)
		writeError(w, r, e) // the reserved order stays pending without a session; a retry reuses it
		return
	}
	if sess.ClientSecret == "" {
		writeError(w, r, newAPIError(502, "payment_setup_failed", "We couldn't start the payment. Please try again later."))
		return
	}

	// Link our order to the session. PRODUCTION: UPDATE … SET stripe_session_id=$2 WHERE order_id=$1.
	_, _ = s.Store.Update(ctx, order.OrderID, func(o *Order) { o.SessionID, o.StripeCustomerID = sess.ID, customerID })
	s.Log.Info("STRIPE", "⑤ Session created & linked to order", "order", order.OrderID, "session", Short(sess.ID),
		"ms", time.Since(t0).Milliseconds(), "expires", time.Unix(order.ExpiresAt, 0).UTC().Format("15:04")+" UTC", "req", rid)

	writeJSONResponse(w, http.StatusCreated, map[string]any{
		"order_id":      order.OrderID,
		"session_id":    sess.ID,
		"client_secret": sess.ClientSecret, // lets Stripe.js open THIS session in the browser
		"quote":         quote,
	})
}

// stripeCustomerFor returns the organisation's Stripe Customer, creating it on the
// first purchase. It holds only the org ULID — no name or email.
// PRODUCTION: store stripe_customer_id on your organisations table.
func (s *Server) stripeCustomerFor(ctx context.Context, orgID string, base []any) (string, *APIError) {
	if id, _ := s.Store.GetStripeCustomerID(ctx, orgID); id != "" {
		s.Log.Info("CHECKOUT", "④ Using the organisation's Stripe customer", append(base, "customer", id)...)
		return id, nil
	}
	p := &stripe.CustomerCreateParams{
		Description: stripe.String("129Knots organisation " + orgID),
		Metadata:    map[string]string{"app": metaApp, "org_id": orgID},
	}
	p.SetIdempotencyKey("bkc-cust-" + orgID)
	sctx, cancel := context.WithTimeout(ctx, s.StripeTimeout)
	defer cancel()
	c, err := s.Customers.Create(sctx, p)
	if err != nil {
		e := fromStripe(err)
		s.Log.Error("STRIPE", "④ Could not create Stripe customer", append([]any{"org", orgID, "code", e.Code}, stripeLogFields(err)...)...)
		return "", e
	}
	_ = s.Store.SetStripeCustomerID(ctx, orgID, c.ID)
	s.Log.Info("CHECKOUT", "④ Created Stripe customer for organisation (first purchase)", append(base, "customer", c.ID)...)
	return c.ID, nil
}

// ── GET /api/checkout-sessions/{id} — payment (Stripe) + fulfilment (our order) ─

func (s *Server) handleSessionStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := IdentityFrom(ctx)
	sid := r.PathValue("id")
	if !sessionIDRe.MatchString(sid) {
		writeError(w, r, badRequest("invalid_session_id", "That checkout link looks invalid."))
		return
	}
	sctx, cancel := context.WithTimeout(ctx, s.StripeTimeout)
	defer cancel()
	sess, err := s.Sessions.Retrieve(sctx, sid, nil) // Stripe answers "was it paid?"
	if err != nil {
		writeError(w, r, fromStripe(err))
		return
	}
	// Only our sessions, and only the session's own organisation.
	if sess.Metadata["app"] != metaApp || sess.Metadata["org_id"] != id.OrgID {
		s.Log.Warn("STATUS", "Status lookup for a session of another organisation — hidden (404)", "session", Short(sid), "org", id.OrgID)
		writeError(w, r, newAPIError(404, "not_found", "We couldn't find that checkout session."))
		return
	}

	orderID := sess.Metadata["order_id"]
	order, _ := s.Store.Get(ctx, orderID) // WE answer "was it credited?"

	// Keep the history honest while the webhook is on its way. This only changes the
	// LABEL of a still-pending order — it never credits. Crediting stays webhook-only.
	if order != nil && order.Status == StatusPending && sess.Status == stripe.CheckoutSessionStatusComplete {
		next := StatusAwaitingPayment // e.g. bank debit: checkout done, money not cleared
		if sess.PaymentStatus == stripe.CheckoutSessionPaymentStatusPaid {
			next = StatusPaymentReceived // paid; coins arrive when the webhook is processed
		}
		updated, _ := s.Store.Update(ctx, orderID, func(o *Order) {
			if o.Status == StatusPending { // never overwrite a status the webhook already set
				o.Status = next
			}
		})
		if updated.Status == next {
			s.Log.Info("STATUS", "Stripe reports "+string(sess.PaymentStatus)+" — order marked "+string(next)+", waiting for webhook to credit",
				"order", orderID, "org", id.OrgID)
		}
		order = &updated
	}
	fulfilment := "unknown"
	var balanceAfter *int64
	if order != nil {
		fulfilment, balanceAfter = string(order.Status), order.BalanceAfter
	}
	s.Log.Info("STATUS", fmt.Sprintf("Return page check: payment=%s, fulfilment=%s", sess.PaymentStatus, fulfilment), "order", orderID, "org", id.OrgID)

	qty, _ := strconv.ParseInt(sess.Metadata["quantity"], 10, 64)
	email := ""
	if sess.CustomerDetails != nil {
		email = sess.CustomerDetails.Email // typed by the payer in Stripe's form; shown only to them
	}
	writeJSONResponse(w, 200, map[string]any{
		"order_id":       orderID,
		"status":         sess.Status,        // open | complete | expired
		"payment_status": sess.PaymentStatus, // paid | unpaid | no_payment_required
		"fulfilment":     fulfilment,         // pending | payment_received | awaiting_payment | credit_retrying | fulfilled | needs_review | ...
		"quantity":       qty,
		"amount_total":   sess.AmountTotal,
		"currency":       sess.Currency,
		"customer_email": email,
		"balance_after":  balanceAfter,
	})
}

// ── helpers ───────────────────────────────────────────────────────────────────

// decodeJSON reads a single JSON object, rejecting unknown fields (so a client can't
// slip in a price or an organisation). Returns the compact raw body for logging.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any, maxBytes int64) (string, *APIError) {
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		return "", newAPIError(http.StatusUnsupportedMediaType, "unsupported_media_type", "Send JSON.")
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return "", newAPIError(http.StatusRequestEntityTooLarge, "body_too_large", "Request is too large.")
		}
		return "", badRequest("body_unreadable", "Could not read the request.")
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var ute *json.UnmarshalTypeError
		switch {
		case errors.As(err, &ute) && ute.Field == "quantity":
			return "", validationError(&FieldError{"quantity", "Must be a whole number."})
		case errors.As(err, &ute):
			return "", validationError(&FieldError{"package_id", "Choose how much BunkCoin you need."})
		case errors.Is(err, io.EOF):
			return "", validationError(&FieldError{"package_id", "Choose how much BunkCoin you need."})
		case strings.HasPrefix(err.Error(), "json: unknown field"):
			return "", badRequest("unknown_field", "Request contains an unsupported field.")
		default:
			return "", badRequest("malformed_json", "Request body isn't valid JSON.")
		}
	}
	if dec.More() {
		return "", badRequest("malformed_json", "Request body must be a single JSON object.")
	}
	return strings.TrimSpace(string(body)), nil
}
