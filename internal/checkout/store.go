package checkout

// Orders + BunkCoin ledger. The Memory* implementations are for local development.
//
// PRODUCTION
//   OrderStore → Postgres table bunkcoin_orders (see README): primary key order_id (ULID),
//                unique indexes on stripe_session_id and idempotency_key.
//   Ledger     → your BunkCoin balance service; Credit must be idempotent by reference.

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

type OrderStatus string

const (
	StatusPending         OrderStatus = "pending"          // order reserved / checkout open, not paid yet
	StatusPaymentReceived OrderStatus = "payment_received" // Stripe reports paid; waiting for the webhook to credit
	StatusAwaitingPayment OrderStatus = "awaiting_payment" // completed with a delayed method (bank debit), not cleared
	StatusCreditRetrying  OrderStatus = "credit_retrying"  // paid, crediting failed temporarily — Stripe retries
	StatusFulfilled       OrderStatus = "fulfilled"        // paid and BunkCoin credited
	StatusNeedsReview     OrderStatus = "needs_review"     // paid, but can't be credited automatically — a human must act
	StatusPaymentFailed   OrderStatus = "payment_failed"   // delayed payment failed
	StatusExpired         OrderStatus = "expired"          // checkout expired unpaid
)

type Order struct {
	OrderID          string // ULID
	OrgID            string // ULID
	UserID           string // ULID
	PackageID        string
	Quantity         int64
	UnitAmount       int64
	Total            int64
	Currency         string
	Status           OrderStatus
	IdempotencyKey   string
	ExpiresAt        int64
	SessionID        string
	StripeCustomerID string
	FailureReason    string
	Attempts         int
	BalanceAfter     *int64
	CreatedAt        time.Time
	CreditedAt       *time.Time
	UpdatedAt        time.Time
}

// OrderStore persists orders. Swap MemoryStore for a Postgres implementation.
type OrderStore interface {
	Create(ctx context.Context, o Order) (Order, error)                             // INSERT
	Get(ctx context.Context, orderID string) (*Order, error)                        // SELECT … WHERE order_id=$1
	FindByIdempotencyKey(ctx context.Context, key string) (*Order, error)           // SELECT … WHERE idempotency_key=$1
	Update(ctx context.Context, orderID string, mutate func(*Order)) (Order, error) // UPDATE … WHERE status<>'fulfilled' (upsert)
	// Fulfil runs credit exactly once per order (see MemoryStore.Fulfil for the SQL).
	Fulfil(ctx context.Context, o Order, credit func(Order) (LedgerEntry, error)) (alreadyDone bool, done Order, err error)
	ListByOrg(ctx context.Context, orgID string, limit int) ([]Order, error)
	GetStripeCustomerID(ctx context.Context, orgID string) (string, error) // organisations.stripe_customer_id
	SetStripeCustomerID(ctx context.Context, orgID, customerID string) error
}

type MemoryStore struct {
	mu        sync.Mutex
	orders    map[string]*Order // orderID → order
	byIdem    map[string]string // idempotency key → orderID
	locks     map[string]*sync.Mutex
	customers map[string]string // orgID → Stripe customer id
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{orders: map[string]*Order{}, byIdem: map[string]string{}, locks: map[string]*sync.Mutex{}, customers: map[string]string{}}
}

func (m *MemoryStore) Create(_ context.Context, o Order) (Order, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	if o.CreatedAt.IsZero() {
		o.CreatedAt = now
	}
	o.UpdatedAt = now
	cp := o
	m.orders[o.OrderID] = &cp
	if o.IdempotencyKey != "" {
		m.byIdem[o.IdempotencyKey] = o.OrderID
	}
	return o, nil
}

func (m *MemoryStore) Get(_ context.Context, id string) (*Order, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if o, ok := m.orders[id]; ok {
		cp := *o
		return &cp, nil
	}
	return nil, nil
}

func (m *MemoryStore) FindByIdempotencyKey(ctx context.Context, key string) (*Order, error) {
	m.mu.Lock()
	id, ok := m.byIdem[key]
	m.mu.Unlock()
	if !ok {
		return nil, nil
	}
	return m.Get(ctx, id)
}

// Update creates the order if missing (Stripe's metadata can rebuild it) and never
// downgrades a fulfilled order.
func (m *MemoryStore) Update(_ context.Context, id string, mutate func(*Order)) (Order, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.orders[id]
	if !ok {
		o = &Order{OrderID: id, CreatedAt: time.Now()}
		m.orders[id] = o
	}
	if o.Status != StatusFulfilled {
		mutate(o)
		o.OrderID = id
		o.UpdatedAt = time.Now()
	}
	return *o, nil
}

// Fulfil runs credit exactly once per order, even when Stripe delivers the same event
// several times or concurrently. If credit fails, the order is NOT marked fulfilled,
// so a later webhook retry can try again.
//
// SQL (one transaction):
//
//	UPDATE bunkcoin_orders SET status='fulfilled', credited_at=now()
//	 WHERE order_id=$1 AND status<>'fulfilled' RETURNING *;   -- 0 rows → already done
//	…credit the ledger… COMMIT;   (ROLLBACK if the credit fails)
func (m *MemoryStore) Fulfil(_ context.Context, o Order, credit func(Order) (LedgerEntry, error)) (bool, Order, error) {
	m.mu.Lock()
	lock, ok := m.locks[o.OrderID]
	if !ok {
		lock = &sync.Mutex{}
		m.locks[o.OrderID] = lock
	}
	m.mu.Unlock()

	lock.Lock() // serialise deliveries for the same order
	defer lock.Unlock()

	m.mu.Lock()
	cur := m.orders[o.OrderID]
	if cur != nil && cur.Status == StatusFulfilled {
		cp := *cur
		m.mu.Unlock()
		return true, cp, nil
	}
	m.mu.Unlock()

	entry, err := credit(o) // ← coins added here
	if err != nil {
		return false, Order{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	done := o
	if cur != nil {
		done.CreatedAt, done.StripeCustomerID, done.IdempotencyKey, done.ExpiresAt = cur.CreatedAt, cur.StripeCustomerID, cur.IdempotencyKey, cur.ExpiresAt
		done.Attempts = cur.Attempts
	}
	if done.CreatedAt.IsZero() {
		done.CreatedAt = time.Now()
	}
	now := time.Now()
	bal := entry.BalanceAfter
	done.Status, done.BalanceAfter, done.CreditedAt, done.UpdatedAt, done.FailureReason = StatusFulfilled, &bal, &now, now, ""
	m.orders[o.OrderID] = &done
	return false, done, nil
}

// ListByOrg returns the organisation's latest orders, hiding ones that never reached Stripe.
func (m *MemoryStore) ListByOrg(_ context.Context, orgID string, limit int) ([]Order, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Order
	for _, o := range m.orders {
		if o.OrgID == orgID && o.SessionID != "" {
			out = append(out, *o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OrderID > out[j].OrderID }) // ULIDs sort by time
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemoryStore) GetStripeCustomerID(_ context.Context, orgID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.customers[orgID], nil
}

func (m *MemoryStore) SetStripeCustomerID(_ context.Context, orgID, customerID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.customers[orgID] = customerID
	return nil
}

// ── Ledger ──────────────────────────────────────────────────────────────────

type LedgerEntry struct {
	OrgID         string
	Quantity      int64
	Reference     string // order ULID
	BalanceBefore int64
	BalanceAfter  int64
	Duplicate     bool
}

// Ledger is where BunkCoin balances live. PRODUCTION: your balance service.
type Ledger interface {
	Balance(ctx context.Context, orgID string) (int64, error)
	// Credit must be idempotent by reference: the same order never adds coins twice.
	Credit(ctx context.Context, orgID string, quantity int64, reference string) (LedgerEntry, error)
}

// LedgerError says whether retrying can help:
//
//	Permanent: true  → account closed/suspended etc. → needs_review (no retry)
//	Permanent: false → outage/timeout → 500, Stripe retries the webhook
type LedgerError struct {
	Code      string
	Msg       string
	Permanent bool
}

func (e *LedgerError) Error() string { return e.Msg }

type MemoryLedger struct {
	mu          sync.Mutex
	balances    map[string]int64
	byReference map[string]LedgerEntry
	FailureMode string // test hook: "" | "temporary" | "permanent"
}

func NewMemoryLedger() *MemoryLedger {
	return &MemoryLedger{balances: map[string]int64{}, byReference: map[string]LedgerEntry{}}
}

func (l *MemoryLedger) Balance(_ context.Context, orgID string) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.balances[orgID], nil
}

func (l *MemoryLedger) Credit(_ context.Context, orgID string, qty int64, ref string) (LedgerEntry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch l.FailureMode {
	case "temporary":
		return LedgerEntry{}, &LedgerError{Code: "ledger_unavailable", Msg: "ledger service unavailable (simulated)"}
	case "permanent":
		return LedgerEntry{}, &LedgerError{Code: "ledger_rejected", Msg: "ledger rejected the credit (simulated)", Permanent: true}
	}
	if orgID == "" {
		return LedgerEntry{}, &LedgerError{Code: "no_account", Msg: "no organisation on the order", Permanent: true}
	}
	if e, ok := l.byReference[ref]; ok {
		e.Duplicate = true
		return e, nil
	}
	before := l.balances[orgID]
	e := LedgerEntry{OrgID: orgID, Quantity: qty, Reference: ref, BalanceBefore: before, BalanceAfter: before + qty}
	l.balances[orgID] = e.BalanceAfter
	l.byReference[ref] = e
	return e, nil
}

// asLedgerError unwraps a *LedgerError if err is one.
func asLedgerError(err error) (*LedgerError, bool) {
	var le *LedgerError
	return le, errors.As(err, &le)
}
