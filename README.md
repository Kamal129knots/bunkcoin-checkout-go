# Bunk Coin top-up (129Knots) — Go

Organisations on the 129Knots platform buy Bunk Coin with preset packages or a custom amount, paid through **Stripe embedded Checkout**. Coins are credited to the buying organisation by a verified Stripe webhook, exactly once.

- **Backend:** Go 1.22+, standard library `net/http`, `github.com/stripe/stripe-go/v87` → Stripe API **`2026-09-30.endive`**. No other dependencies.
- **Frontend:** plain HTML/CSS/JS in `web/`, embedded into the binary. Same pages and API as the Node version.
- **Identity:** user and organisation are **ULIDs read from the session**. No names or emails are stored, logged or sent to Stripe.

---

## Run locally

```bash
cp .env.example .env     # paste sk_test_ / pk_test_ keys from the SAME sandbox as `stripe login`

# terminal B — forward webhooks (copy the whsec_ it prints into .env)
stripe login
make webhooks

# terminal A — the app
make run
```

1. Open http://localhost:4242 → **dashboard** with the Bunk Coin card.
2. Click **+** (or **Top up**) → **purchase page** with *Choose amount* and *Purchase history*.
3. Pick an amount → **Continue to payment** → pay with `4242 4242 4242 4242`, any future expiry, any CVC.
4. Stripe redirects to the **result page**: ✅ coins added and the new balance. History shows *Credited*.

Terminal A prints every server step, numbered ① to ⑪. Terminal B shows each webhook and the status your server answered.

```bash
make test    # 22 test functions incl. idempotent retries, org isolation, concurrent webhook duplicates (race detector on)
```

**Keys checklist** (the most common setup problem): `stripe config --list` shows the CLI's `account_id`. Your `sk_test_` key must be from the same account, or `stripe listen` never sees your payments and nothing is credited. After editing `.env`, stop and restart `make run`. Values exported in your shell override `.env`.

---

## Project layout

```
cmd/bunkcoin/main.go            wiring: .env, config, logger, Stripe client, store, ledger, HTTP server
internal/checkout/
  server.go                     routes, middleware, checkout ①–⑤, status endpoint
  webhook.go                    webhook ⑥–⑧ and fulfilment ⑨–⑪ (the only place coins are credited)
  store.go                      OrderStore + Ledger interfaces, in-memory implementations, LedgerError
  auth.go                       identity from the session: dev cookie or HS256 JWT; ULIDs only
  pricing.go                    validates the choice and prices it
  apierror.go                   one error shape; Stripe errors → customer-safe messages
  logger.go                     pretty / JSON logs
  config.go, dotenv.go          env loading + validation (fails fast)
  ulid.go, ratelimit.go
  checkout_test.go
web/                            frontend (embedded via web/embed.go); theme.css = brand tokens
```

## API

| Method | Path | Auth | Purpose |
|---|---|---|---|
| GET | `/api/config` | — | Packages, price, limits, publishable key |
| GET | `/api/me` | — | `{ signed_in, user_id, org_id, balance }` |
| GET | `/api/orders` | ✓ | Organisation's last 10 purchases |
| POST | `/api/checkout-sessions` | ✓ | `{package_id}` or `{quantity}` → `{order_id, session_id, client_secret, quote}` |
| GET | `/api/checkout-sessions/{id}` | ✓ (own org) | `{order_id, status, payment_status, fulfilment, quantity, balance_after, …}` |
| POST | `/webhooks/stripe` | Stripe signature | Fulfilment |
| GET | `/healthz` | — | Liveness |

---

## Identity and metadata

| Mode | Where `{UserID, OrgID}` comes from |
|---|---|
| `AUTH_MODE=dev` | Signed HTTP-only `bkc_session` cookie issued on first visit for `DEV_USER_ID` / `DEV_ORG_ID` |
| `AUTH_MODE=jwt` | HS256 JWT from the 129Knots auth service: `sub` = user ULID, `org_id` = organisation ULID, `exp` |

Every purchase gets an **order ULID** before Stripe is called:

| Stripe field | Value |
|---|---|
| `client_reference_id` | order ULID |
| `metadata` (session) | `app`, `order_id`, `org_id`, `user_id`, `package_id`, `quantity`, `unit_amount` |
| `payment_intent_data.metadata` | `app`, `order_id`, `org_id`, `user_id`, `quantity` |
| `customer` | one Stripe Customer per organisation (`metadata.org_id`), no name or email |

## Flow and logs

```
CHECKOUT ① Checkout requested           browser sent { package_id } or { quantity }
         ② Priced the order              quantity × unit price, server-side only
         ③ Order reserved → pending      new order ULID (a retried request reuses the same order)
         ④ Stripe customer               the organisation's Stripe Customer
         ⑤ Session created & linked      embedded_page Checkout Session
   … customer pays inside Stripe's iframe …
WEBHOOK  ⑥ Webhook received             POST /webhooks/stripe
         ⑦ Signature verified            STRIPE_WEBHOOK_SECRET (any API version accepted)
         ⑧ Payment confirmed (paid)      or: awaiting payment / expired / failed
FULFIL   ⑨ Integrity checks passed       ULIDs valid, client_reference_id = order_id, subtotal = qty × unit
         ⑩ Credited N BunkCoin           Ledger.Credit(org, qty, order_id): idempotent by order
         ⑪ Order fulfilled ✓
```

### When crediting fails

| Situation | Order status | Answer to Stripe | Customer sees |
|---|---|---|---|
| Credited | `fulfilled` | 200 | ✅ "N Bunk Coin added. Balance is now X" |
| Paid, webhook not processed yet | `payment_received` (set by the status check; **label only, never credits**) | — | ✅ "Payment received", history shows *Payment received* until the webhook credits |
| Same event delivered again | unchanged | 200 | — |
| Ledger temporarily down | `credit_retrying` | **500**, so Stripe retries for up to 3 days | ⏳ |
| Ledger rejects (account closed) | `needs_review` | 200 | 🛟 "our team will finalise it" |
| Amount/metadata mismatch | `needs_review` | 200 | 🛟 |
| Bank payment not cleared | `awaiting_payment` | 200 | 🕒 |

---

## Going to production

1. **Login:** `AUTH_MODE=jwt`, `AUTH_SECRET` = the HS256 secret your auth service signs with. For RS256/JWKS, replace `VerifyJWT` in `auth.go`.
2. **OrderStore** → Postgres (implement the `OrderStore` interface):
   ```sql
   CREATE TABLE bunkcoin_orders (
     order_id          CHAR(26) PRIMARY KEY,
     org_id            CHAR(26) NOT NULL,
     user_id           CHAR(26) NOT NULL,
     package_id        TEXT NOT NULL,
     quantity          BIGINT NOT NULL,
     unit_amount       BIGINT NOT NULL,
     total             BIGINT NOT NULL,
     currency          CHAR(3) NOT NULL,
     status            TEXT NOT NULL,
     idempotency_key   TEXT UNIQUE NOT NULL,
     expires_at        BIGINT NOT NULL,
     stripe_session_id TEXT UNIQUE,
     failure_reason    TEXT,
     attempts          INT NOT NULL DEFAULT 0,
     balance_after     BIGINT,
     created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
     credited_at       TIMESTAMPTZ,
     updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
   );
   CREATE INDEX ON bunkcoin_orders (org_id, order_id DESC);
   ALTER TABLE organisations ADD COLUMN stripe_customer_id TEXT;
   ```
   `Fulfil` = one transaction: `UPDATE … SET status='fulfilled' WHERE order_id=$1 AND status<>'fulfilled' RETURNING *`, credit, commit.
3. **Ledger** → your balance service (implement the `Ledger` interface). `Credit` must be idempotent by order id and return `*LedgerError{Permanent: true}` for closed/suspended accounts, `Permanent: false` for outages.
4. **Webhook endpoint** in the Stripe Dashboard for the four `checkout.session.*` events; put its `whsec_` in your secrets store (Kubernetes secret / AWS Secrets Manager).
5. **Logs & alerts:** `LOG_FORMAT=json`; alert on `cat=FULFIL level=error` and `cat=WEBHOOK level=error`.
6. **Edge:** pass the webhook body through the ingress unmodified; use the trusted client-IP header in `ratelimit.go`; add a CSP allowing `js.stripe.com`.
7. **Container:** `make docker` (distroless, non-root).


## Sample card numbers

Mastercard success	5555 5555 5555 4444
Amex success (4-digit CVC)	3782 822463 10005
Visa debit success	4000 0566 5566 5556
Requires 3D Secure authentication	4000 0025 0000 3155
Generic decline	4000 0000 0000 0002
Insufficient funds	4000 0000 0000 9995
Incorrect CVC decline	4000 0000 0000 0127
Expired card decline	4000 0000 0000 00