package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stripe/stripe-go/v87"
)

const (
	whsec     = "whsec_test_secret"
	orgA      = "01JA2Y6H8D4C2B0Z9X7W5V3T1R"
	userA     = "01JA2Y7K3V9QF6M8N4P5R0S1TW"
	orgB      = "01JA30AAAAAAAAAAAAAAAAAAAA"
	userB     = "01JA30BBBBBBBBBBBBBBBBBBBB"
	jwtSecret = "jjjjjjjjjjjjjjjjjjjjjjjjjjjjjjjjjjjjjjjj"
)

func testConfig(t *testing.T) *Config {
	t.Helper()
	env := map[string]string{"STRIPE_SECRET_KEY": "sk_test_x", "STRIPE_PUBLISHABLE_KEY": "pk_test_x", "STRIPE_WEBHOOK_SECRET": whsec}
	c, err := LoadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// ── fake Stripe (enforces idempotency like the real API) ──────────────────────

type fakeSessions struct {
	mu      sync.Mutex
	err     error
	created []*stripe.CheckoutSessionCreateParams
	byIdem  map[string]struct {
		params string
		sess   *stripe.CheckoutSession
	}
	retrieved *stripe.CheckoutSession
}

func (f *fakeSessions) Create(_ context.Context, p *stripe.CheckoutSessionCreateParams) (*stripe.CheckoutSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.created = append(f.created, p)
	if f.byIdem == nil {
		f.byIdem = map[string]struct {
			params string
			sess   *stripe.CheckoutSession
		}{}
	}
	pj, _ := json.Marshal(p)
	if prev, ok := f.byIdem[*p.IdempotencyKey]; ok {
		if prev.params != string(pj) { // real Stripe rejects a reused key with different parameters
			return nil, &stripe.Error{Type: stripe.ErrorTypeIdempotency, HTTPStatusCode: 400, Msg: "Keys for idempotent requests can only be used with the same parameters"}
		}
		return prev.sess, nil
	}
	s := &stripe.CheckoutSession{ID: fmt.Sprintf("cs_test_fake%010d", len(f.byIdem)+1), ClientSecret: "cs_secret_x", Metadata: p.Metadata}
	f.byIdem[*p.IdempotencyKey] = struct {
		params string
		sess   *stripe.CheckoutSession
	}{string(pj), s}
	return s, nil
}

func (f *fakeSessions) Retrieve(context.Context, string, *stripe.CheckoutSessionRetrieveParams) (*stripe.CheckoutSession, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.retrieved, nil
}

type fakeCustomers struct {
	mu      sync.Mutex
	created int
}

func (f *fakeCustomers) Create(_ context.Context, p *stripe.CustomerCreateParams) (*stripe.Customer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created++
	return &stripe.Customer{ID: "cus_fake_" + p.Metadata["org_id"][22:]}, nil
}

type harness struct {
	t         *testing.T
	ts        *httptest.Server
	sessions  *fakeSessions
	customers *fakeCustomers
	store     *MemoryStore
	ledger    *MemoryLedger
	token     string
	cookies   []*http.Cookie
	authMode  string
}

func newHarness(t *testing.T, authMode string) *harness {
	t.Helper()
	cfg := testConfig(t)
	h := &harness{t: t, sessions: &fakeSessions{}, customers: &fakeCustomers{}, store: NewMemoryStore(), ledger: NewMemoryLedger(), authMode: authMode}
	s := &Server{
		Cfg: cfg, Sessions: h.sessions, Customers: h.customers, Store: h.store, Ledger: h.ledger,
		Auth:           NewAuth(authMode, []byte(jwtSecret), Identity{UserID: userA, OrgID: orgA}, false, nil),
		Log:            NewLogger(io.Discard, "pretty", "error"),
		ConstructEvent: VerifyStripeEvent, StripeTimeout: time.Second,
	}
	h.ts = httptest.NewServer(s.Routes(fstest.MapFS{"index.html": {Data: []byte("ok")}}))
	t.Cleanup(h.ts.Close)
	h.as(userA, orgA)
	return h
}

func (h *harness) as(user, org string) {
	if user == "" {
		h.token = ""
		return
	}
	h.token = SignJWT(user, org, time.Now().Add(10*time.Minute), []byte(jwtSecret))
}

type resp struct {
	status int
	body   map[string]any
	header http.Header
}

func (h *harness) call(method, path, body string, hdr map[string]string) resp {
	h.t.Helper()
	req, _ := http.NewRequest(method, h.ts.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if h.authMode == "jwt" && h.token != "" {
		req.Header.Set("Authorization", "Bearer "+h.token)
	}
	for _, c := range h.cookies {
		req.AddCookie(c)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	if cs := res.Cookies(); len(cs) > 0 {
		h.cookies = cs
	}
	var m map[string]any
	_ = json.NewDecoder(res.Body).Decode(&m)
	return resp{res.StatusCode, m, res.Header}
}

func errCode(r resp) string {
	e, _ := r.body["error"].(map[string]any)
	s, _ := e["code"].(string)
	return s
}

// ── unit ──────────────────────────────────────────────────────────────────────

func TestULID(t *testing.T) {
	a, b := newULIDAt(time.UnixMilli(1_700_000_000_000)), newULIDAt(time.UnixMilli(1_700_000_000_001))
	if !IsULID(a) || !IsULID(b) || a >= b {
		t.Fatalf("bad ulids %s %s", a, b)
	}
	for _, v := range []string{"org_pacific", "", "01JA2Y6H8D4C2B0Z9X7W5V3T1", "01JA2Y6H8D4C2B0Z9X7W5V3T1I"} {
		if IsULID(v) {
			t.Errorf("%q accepted", v)
		}
	}
}

func TestConfig(t *testing.T) {
	_, err := LoadConfig(func(string) string { return "" })
	if err == nil || strings.Count(err.Error(), "\n  - ") != 3 { // 3 missing Stripe values listed together
		t.Fatalf("want all problems listed, got %v", err)
	}
	live := map[string]string{"STRIPE_SECRET_KEY": "sk_live_x", "STRIPE_PUBLISHABLE_KEY": "pk_live_x", "STRIPE_WEBHOOK_SECRET": "w"}
	if _, err := LoadConfig(func(k string) string { return live[k] }); err == nil || !strings.Contains(err.Error(), "live") {
		t.Fatal("dev session must be refused with live keys")
	}
	bad := map[string]string{"STRIPE_SECRET_KEY": "sk_test_x", "STRIPE_PUBLISHABLE_KEY": "pk_test_x", "STRIPE_WEBHOOK_SECRET": "w", "DEV_ORG_ID": "org_x"}
	if _, err := LoadConfig(func(k string) string { return bad[k] }); err == nil || !strings.Contains(err.Error(), "ULID") {
		t.Fatal("non-ULID dev identity must be refused")
	}
}

func TestBuildQuote(t *testing.T) {
	c := testConfig(t)
	n := func(v int64) *int64 { return &v }
	if q, _ := BuildQuote(c, PurchaseRequest{PackageID: "pro"}); q.Total != 100000 {
		t.Fatal(q)
	}
	if q, _ := BuildQuote(c, PurchaseRequest{Quantity: n(2500)}); q.Total != 250000 {
		t.Fatal(q)
	}
	for name, tc := range map[string]struct {
		req   PurchaseRequest
		field string
	}{
		"below min": {PurchaseRequest{Quantity: n(9)}, "quantity"}, "above max": {PurchaseRequest{Quantity: n(100001)}, "quantity"},
		"negative": {PurchaseRequest{Quantity: n(-5)}, "quantity"}, "unknown pkg": {PurchaseRequest{PackageID: "gold"}, "package_id"},
		"both": {PurchaseRequest{PackageID: "pro", Quantity: n(50)}, "package_id"}, "neither": {PurchaseRequest{}, "package_id"},
	} {
		var fe *FieldError
		if _, err := BuildQuote(c, tc.req); !errors.As(err, &fe) || fe.Field != tc.field {
			t.Errorf("%s: got %v", name, err)
		}
	}
}

func TestJWT(t *testing.T) {
	now := time.Now()
	tok := SignJWT(userA, orgA, now.Add(time.Minute), []byte(jwtSecret))
	if c := VerifyJWT(tok, []byte(jwtSecret), now); c == nil || c.OrgID != orgA {
		t.Fatal("valid token rejected")
	}
	if VerifyJWT(tok, []byte("x"), now) != nil || VerifyJWT(SignJWT(userA, orgA, now.Add(-time.Minute), []byte(jwtSecret)), []byte(jwtSecret), now) != nil {
		t.Fatal("wrong secret / expired accepted")
	}
	parts := strings.Split(tok, ".")
	forged := b64.EncodeToString([]byte(fmt.Sprintf(`{"sub":%q,"org_id":%q,"exp":%d}`, userA, orgB, now.Add(time.Hour).Unix())))
	if VerifyJWT(parts[0]+"."+forged+"."+parts[2], []byte(jwtSecret), now) != nil {
		t.Fatal("tampered token accepted")
	}
	none := b64.EncodeToString([]byte(`{"alg":"none"}`))
	if VerifyJWT(none+"."+forged+".", []byte(jwtSecret), now) != nil {
		t.Fatal("alg=none accepted")
	}
}

func TestOrderFromSessionIntegrity(t *testing.T) {
	oid := NewULID()
	mk := func(edit func(*stripe.CheckoutSession)) *stripe.CheckoutSession {
		s := &stripe.CheckoutSession{ID: "cs_1", AmountSubtotal: 50000, AmountTotal: 50000, Currency: "usd", ClientReferenceID: oid,
			Metadata: map[string]string{"order_id": oid, "org_id": orgA, "user_id": userA, "quantity": "500", "unit_amount": "100"}}
		if edit != nil {
			edit(s)
		}
		return s
	}
	if _, p := orderFromSession(mk(nil)); p != "" {
		t.Fatal(p)
	}
	cases := map[string]func(*stripe.CheckoutSession){
		"amount mismatch":     func(s *stripe.CheckoutSession) { s.AmountSubtotal = 1 },
		"client_reference_id": func(s *stripe.CheckoutSession) { s.ClientReferenceID = NewULID() },
		"org_id":              func(s *stripe.CheckoutSession) { s.Metadata["org_id"] = "org_x" },
		"order_id":            func(s *stripe.CheckoutSession) { delete(s.Metadata, "order_id") },
	}
	for want, edit := range cases {
		if _, p := orderFromSession(mk(edit)); !strings.Contains(p, want) {
			t.Errorf("want problem %q, got %q", want, p)
		}
	}
}

// ── identity ──────────────────────────────────────────────────────────────────

func TestDevSessionIssuesULIDIdentity(t *testing.T) {
	h := newHarness(t, "dev")
	first := h.call("GET", "/api/me", "", nil)
	if !strings.Contains(first.header.Get("Set-Cookie"), "bkc_session=") {
		t.Fatal("no session cookie")
	}
	me := h.call("GET", "/api/me", "", nil).body
	if me["signed_in"] != true || me["user_id"] != userA || me["org_id"] != orgA || me["name"] != nil {
		t.Fatalf("got %v", me)
	}
}

func TestJWTModeRequiresValidULIDSession(t *testing.T) {
	h := newHarness(t, "jwt")
	h.as("", "")
	if h.call("GET", "/api/me", "", nil).body["signed_in"] != false {
		t.Fatal("expected signed out")
	}
	if r := h.call("POST", "/api/checkout-sessions", `{"package_id":"pro"}`, nil); r.status != 401 {
		t.Fatalf("got %d", r.status)
	}
	h.as("user_1", "org_1")
	if r := h.call("POST", "/api/checkout-sessions", `{"package_id":"pro"}`, nil); r.status != 401 {
		t.Fatalf("non-ULID identity accepted: %d", r.status)
	}
}

// ── checkout ──────────────────────────────────────────────────────────────────

func TestCreateSessionPassesULIDMetadata(t *testing.T) {
	h := newHarness(t, "jwt")
	r := h.call("POST", "/api/checkout-sessions", `{"quantity":2500}`, map[string]string{"Idempotency-Key": "key-aaaaaaaa"})
	if r.status != 201 {
		t.Fatalf("%d %v", r.status, r.body)
	}
	orderID := r.body["order_id"].(string)
	p := h.sessions.created[len(h.sessions.created)-1]
	want := map[string]string{"app": metaApp, "order_id": orderID, "org_id": orgA, "user_id": userA, "package_id": "custom", "quantity": "2500", "unit_amount": "100"}
	switch {
	case !IsULID(orderID):
		t.Error("order id not a ULID")
	case *p.UIMode != "embedded_page" || *p.ClientReferenceID != orderID:
		t.Error("ui_mode / client_reference_id")
	case fmt.Sprint(p.Metadata) != fmt.Sprint(want):
		t.Errorf("metadata = %v", p.Metadata)
	case p.PaymentIntentData.Metadata["order_id"] != orderID:
		t.Error("payment intent metadata")
	case *p.LineItems[0].Quantity != 2500 || *p.LineItems[0].PriceData.UnitAmount != 100:
		t.Error("line item")
	case p.CustomerEmail != nil:
		t.Error("email must not be sent")
	}
	o, _ := h.store.Get(context.Background(), orderID)
	if o.Status != StatusPending || o.SessionID != r.body["session_id"] {
		t.Fatalf("order %+v", o)
	}
}

func TestRetryReusesOrderAndIdenticalParams(t *testing.T) {
	h := newHarness(t, "jwt")
	hdr := map[string]string{"Idempotency-Key": "retry-key-123456"}
	a := h.call("POST", "/api/checkout-sessions", `{"package_id":"pro"}`, hdr)
	time.Sleep(1100 * time.Millisecond) // expires_at must NOT change on retry
	b := h.call("POST", "/api/checkout-sessions", `{"package_id":"pro"}`, hdr)
	if b.status != 201 || a.body["session_id"] != b.body["session_id"] || a.body["order_id"] != b.body["order_id"] {
		t.Fatalf("a=%v b=%d %v", a.body, b.status, b.body)
	}
}

func TestReusesOrganisationsStripeCustomer(t *testing.T) {
	h := newHarness(t, "jwt")
	h.call("POST", "/api/checkout-sessions", `{"package_id":"pro"}`, nil)
	h.call("POST", "/api/checkout-sessions", `{"package_id":"starter"}`, nil)
	if h.customers.created != 1 {
		t.Fatalf("customers created = %d", h.customers.created)
	}
}

func TestCreateSessionValidation(t *testing.T) {
	h := newHarness(t, "jwt")
	for name, tc := range map[string]struct {
		body   string
		hdr    map[string]string
		status int
		code   string
	}{
		"malformed":          {`{"quantity":`, nil, 400, "malformed_json"},
		"unknown field":      {`{"quantity":100,"amount":1}`, nil, 400, "unknown_field"},
		"client sends org":   {`{"quantity":100,"org_id":"` + orgB + `"}`, nil, 400, "unknown_field"},
		"string qty":         {`{"quantity":"100"}`, nil, 422, "validation_failed"},
		"fractional qty":     {`{"quantity":10.5}`, nil, 422, "validation_failed"},
		"too small":          {`{"quantity":1}`, nil, 422, "validation_failed"},
		"bad idem key":       {`{"package_id":"pro"}`, map[string]string{"Idempotency-Key": "x"}, 400, "invalid_idempotency_key"},
		"wrong content type": {`{"package_id":"pro"}`, map[string]string{"Content-Type": "text/plain"}, 415, "unsupported_media_type"},
		"too large":          {`{"package_id":"` + strings.Repeat("a", 5000) + `"}`, nil, 413, "body_too_large"},
	} {
		r := h.call("POST", "/api/checkout-sessions", tc.body, tc.hdr)
		if r.status != tc.status || errCode(r) != tc.code {
			t.Errorf("%s: got %d %q, want %d %q", name, r.status, errCode(r), tc.status, tc.code)
		}
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "dial tcp: i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestStripeErrorMapping(t *testing.T) {
	for name, tc := range map[string]struct {
		err       error
		status    int
		code      string
		retryable bool
	}{
		"rate limit": {&stripe.Error{Type: stripe.ErrorTypeRateLimit, HTTPStatusCode: 429}, 429, "rate_limited", true},
		"bad key":    {&stripe.Error{Type: stripe.ErrorTypeInvalidRequest, HTTPStatusCode: 401}, 500, "payments_misconfigured", false},
		"our bug":    {&stripe.Error{Type: stripe.ErrorTypeInvalidRequest, HTTPStatusCode: 400, Param: "line_items"}, 502, "payment_setup_failed", false},
		"stripe 500": {&stripe.Error{Type: stripe.ErrorTypeAPI, HTTPStatusCode: 500}, 502, "payment_provider_error", true},
		"network":    {timeoutErr{}, 503, "payment_provider_unavailable", true},
		"deadline":   {context.DeadlineExceeded, 504, "payment_provider_timeout", true},
	} {
		h := newHarness(t, "jwt")
		h.sessions.err = tc.err
		r := h.call("POST", "/api/checkout-sessions", `{"package_id":"pro"}`, nil)
		e := r.body["error"].(map[string]any)
		if r.status != tc.status || e["code"] != tc.code || e["retryable"] != tc.retryable {
			t.Errorf("%s: got %d %v", name, r.status, e)
		}
	}
}

func TestSessionStatusScopedToOrganisation(t *testing.T) {
	h := newHarness(t, "jwt")
	sess := func(org string) *stripe.CheckoutSession {
		return &stripe.CheckoutSession{ID: "cs_test_abcdefghijk", Status: "complete", PaymentStatus: "paid", AmountTotal: 50000, Currency: "usd",
			Metadata: map[string]string{"app": metaApp, "order_id": NewULID(), "org_id": org, "quantity": "500"}}
	}
	h.sessions.retrieved = sess(orgA)
	if r := h.call("GET", "/api/checkout-sessions/cs_test_abcdefghijk", "", nil); r.status != 200 || r.body["quantity"].(float64) != 500 {
		t.Fatalf("%d %v", r.status, r.body)
	}
	h.sessions.retrieved = sess(orgB)
	if r := h.call("GET", "/api/checkout-sessions/cs_test_abcdefghijk", "", nil); r.status != 404 {
		t.Fatalf("other org's session visible: %d", r.status)
	}
	if r := h.call("GET", "/api/checkout-sessions/pi_123", "", nil); r.status != 400 {
		t.Fatalf("malformed id: %d", r.status)
	}
}

// ── webhook / fulfilment ──────────────────────────────────────────────────────

func signedEvent(t *testing.T, typ string, obj any) (string, string) {
	t.Helper()
	raw, _ := json.Marshal(obj)
	body := fmt.Sprintf(`{"id":"evt_%d","object":"event","api_version":%q,"type":%q,"data":{"object":%s}}`,
		time.Now().UnixNano(), "2025-01-27.acacia", typ, raw) // older version on purpose: must still be accepted
	sp := stripe.GenerateTestSignedPayload(&stripe.UnsignedPayload{Payload: []byte(body), Secret: whsec})
	return body, sp.Header
}

func paidSession(orderID, org string, qty, subtotal int64, payStatus string) map[string]any {
	return map[string]any{"id": "cs_test_" + orderID, "object": "checkout.session", "status": "complete", "payment_status": payStatus,
		"client_reference_id": orderID, "amount_subtotal": subtotal, "amount_total": subtotal, "currency": "usd",
		"metadata": map[string]string{"app": metaApp, "order_id": orderID, "org_id": org, "user_id": userA, "package_id": "standard",
			"quantity": fmt.Sprint(qty), "unit_amount": "100"}}
}

func (h *harness) webhook(body, sig string) resp {
	return h.call("POST", "/webhooks/stripe", body, map[string]string{"Stripe-Signature": sig})
}

func balance(h *harness, org string) int64 {
	b, _ := h.ledger.Balance(context.Background(), org)
	return b
}

func TestWebhookCreditsExactlyOnceUnderConcurrentDuplicates(t *testing.T) {
	h := newHarness(t, "jwt")
	oid := NewULID()
	body, sig := signedEvent(t, "checkout.session.completed", paidSession(oid, orgA, 500, 50000, "paid"))
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); h.webhook(body, sig) }()
	}
	wg.Wait()
	o, _ := h.store.Get(context.Background(), oid)
	if balance(h, orgA) != 500 || balance(h, orgB) != 0 || o.Status != StatusFulfilled || *o.BalanceAfter != 500 {
		t.Fatalf("balance=%d order=%+v", balance(h, orgA), o)
	}
}

func TestWebhookRejectsForgedSignature(t *testing.T) {
	h := newHarness(t, "jwt")
	body, _ := signedEvent(t, "checkout.session.completed", paidSession(NewULID(), orgA, 500, 50000, "paid"))
	if r := h.webhook(body, "t=1,v1=deadbeef"); r.status != 400 || balance(h, orgA) != 0 {
		t.Fatalf("got %d", r.status)
	}
}

func TestWebhookDelayedPayment(t *testing.T) {
	h := newHarness(t, "jwt")
	oid := NewULID()
	h.webhook(signedEvent(t, "checkout.session.completed", paidSession(oid, orgA, 500, 50000, "unpaid")))
	if o, _ := h.store.Get(context.Background(), oid); o.Status != StatusAwaitingPayment || balance(h, orgA) != 0 {
		t.Fatalf("%+v", o)
	}
	h.webhook(signedEvent(t, "checkout.session.async_payment_succeeded", paidSession(oid, orgA, 500, 50000, "paid")))
	if balance(h, orgA) != 500 {
		t.Fatal("not credited after async success")
	}
}

func TestWebhookTemporaryLedgerFailureRetries(t *testing.T) {
	h := newHarness(t, "jwt")
	oid := NewULID()
	body, sig := signedEvent(t, "checkout.session.completed", paidSession(oid, orgA, 100, 10000, "paid"))
	h.ledger.FailureMode = "temporary"
	if r := h.webhook(body, sig); r.status != 500 {
		t.Fatalf("want 500 so Stripe retries, got %d", r.status)
	}
	if o, _ := h.store.Get(context.Background(), oid); o.Status != StatusCreditRetrying || o.Attempts != 1 {
		t.Fatalf("%+v", o)
	}
	h.ledger.FailureMode = ""
	if r := h.webhook(body, sig); r.status != 200 || balance(h, orgA) != 100 {
		t.Fatalf("retry: %d balance %d", r.status, balance(h, orgA))
	}
}

func TestWebhookPermanentLedgerFailureNeedsReview(t *testing.T) {
	h := newHarness(t, "jwt")
	oid := NewULID()
	h.ledger.FailureMode = "permanent"
	if r := h.webhook(signedEvent(t, "checkout.session.completed", paidSession(oid, orgA, 100, 10000, "paid"))); r.status != 200 {
		t.Fatalf("got %d", r.status)
	}
	if o, _ := h.store.Get(context.Background(), oid); o.Status != StatusNeedsReview || o.FailureReason != "ledger_rejected" {
		t.Fatalf("%+v", o)
	}
}

func TestWebhookAmountMismatchNeedsReview(t *testing.T) {
	h := newHarness(t, "jwt")
	oid := NewULID()
	if r := h.webhook(signedEvent(t, "checkout.session.completed", paidSession(oid, orgA, 5000, 100, "paid"))); r.status != 200 {
		t.Fatalf("got %d", r.status)
	}
	if o, _ := h.store.Get(context.Background(), oid); o.Status != StatusNeedsReview || balance(h, orgA) != 0 {
		t.Fatalf("%+v", o)
	}
}

func TestWebhookExpiredFailedAndIgnored(t *testing.T) {
	h := newHarness(t, "jwt")
	e, f := NewULID(), NewULID()
	exp := paidSession(e, orgA, 100, 10000, "unpaid")
	exp["status"] = "expired"
	h.webhook(signedEvent(t, "checkout.session.expired", exp))
	h.webhook(signedEvent(t, "checkout.session.async_payment_failed", paidSession(f, orgA, 100, 10000, "unpaid")))
	oe, _ := h.store.Get(context.Background(), e)
	of, _ := h.store.Get(context.Background(), f)
	if oe.Status != StatusExpired || of.Status != StatusPaymentFailed {
		t.Fatalf("%s %s", oe.Status, of.Status)
	}
	other := paidSession(NewULID(), orgA, 500, 50000, "paid")
	other["metadata"] = map[string]string{"app": "else"}
	if r := h.webhook(signedEvent(t, "checkout.session.completed", other)); r.status != 200 || balance(h, orgA) != 0 {
		t.Fatal("foreign session not ignored")
	}
	if r := h.webhook(signedEvent(t, "customer.created", map[string]any{"id": "cus_1", "object": "customer"})); r.status != 200 {
		t.Fatal("unhandled event not acknowledged")
	}
}

func TestEndToEndHistoryPerOrganisation(t *testing.T) {
	h := newHarness(t, "jwt")
	r := h.call("POST", "/api/checkout-sessions", `{"package_id":"standard"}`, nil)
	oid := r.body["order_id"].(string)
	s := paidSession(oid, orgA, 500, 50000, "paid")
	s["id"] = r.body["session_id"]
	h.webhook(signedEvent(t, "checkout.session.completed", s))

	mine := h.call("GET", "/api/orders", "", nil).body["orders"].([]any)
	me := h.call("GET", "/api/me", "", nil).body
	h.as(userB, orgB)
	theirs := h.call("GET", "/api/orders", "", nil).body["orders"].([]any)
	if len(mine) != 1 || mine[0].(map[string]any)["status"] != "fulfilled" || me["balance"].(float64) != 500 || len(theirs) != 0 {
		t.Fatalf("mine=%v me=%v theirs=%v", mine, me, theirs)
	}
}

func TestStatusCheckMarksPaymentReceivedButNeverCredits(t *testing.T) {
	h := newHarness(t, "jwt")
	r := h.call("POST", "/api/checkout-sessions", `{"package_id":"standard"}`, nil)
	oid, sid := r.body["order_id"].(string), r.body["session_id"].(string)

	// Customer paid, but the webhook hasn't arrived: Stripe says paid, our order is pending.
	h.sessions.retrieved = &stripe.CheckoutSession{ID: sid, Status: "complete", PaymentStatus: "paid", AmountTotal: 50000, Currency: "usd",
		Metadata: map[string]string{"app": metaApp, "order_id": oid, "org_id": orgA, "quantity": "500"}}
	st := h.call("GET", "/api/checkout-sessions/"+sid, "", nil)
	if st.body["fulfilment"] != "payment_received" || balance(h, orgA) != 0 {
		t.Fatalf("fulfilment=%v balance=%d", st.body["fulfilment"], balance(h, orgA))
	}
	hist := h.call("GET", "/api/orders", "", nil).body["orders"].([]any)
	if hist[0].(map[string]any)["status"] != "payment_received" {
		t.Fatalf("history: %v", hist)
	}

	// Webhook arrives → credited; later status checks don't downgrade it.
	s := paidSession(oid, orgA, 500, 50000, "paid")
	s["id"] = sid
	h.webhook(signedEvent(t, "checkout.session.completed", s))
	if st := h.call("GET", "/api/checkout-sessions/"+sid, "", nil); st.body["fulfilment"] != "fulfilled" || balance(h, orgA) != 500 {
		t.Fatalf("after webhook: %v balance=%d", st.body["fulfilment"], balance(h, orgA))
	}
}
