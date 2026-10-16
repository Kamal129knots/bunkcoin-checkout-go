package checkout

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// StripeMaxAmount is Stripe's per-payment cap in minor units (e.g. US$999,999.99).
const StripeMaxAmount = 99_999_999

// Package is a preset purchase option shown on the top-up page.
type Package struct {
	ID       string `json:"id"`
	Quantity int64  `json:"quantity"`
	Label    string `json:"label"`
	Popular  bool   `json:"popular,omitempty"`
}

// Config is loaded once at startup. Secrets never reach the browser: only
// PublishableKey is exposed via /api/config.
type Config struct {
	Port            string
	PublicBaseURL   string // used to build Stripe's return_url
	StripeSecretKey string
	PublishableKey  string
	WebhookSecret   string

	Currency          string // ISO code, lowercase
	UnitAmount        int64  // price of 1 coin in minor units (100 = $1.00)
	MinQty, MaxQty    int64  // custom-amount bounds
	SessionTTLMinutes int64  // embedded session lifetime (30–1440)
	Packages          []Package

	AuthMode    string   // "dev" | "jwt"
	AuthSecret  []byte   // HMAC key: dev session cookie, or the auth service's JWT secret
	DevIdentity Identity // dev mode: the session's user + organisation (ULIDs)

	LogFormat string // "pretty" | "json"
	LogLevel  string // "debug" | "info" | "warn" | "error"
}

// DefaultPackages — PRODUCTION: could come from a table so ops can change them without a deploy.
var DefaultPackages = []Package{
	{ID: "starter", Quantity: 100, Label: "Starter"},
	{ID: "standard", Quantity: 500, Label: "Standard", Popular: true},
	{ID: "pro", Quantity: 1000, Label: "Pro"},
	{ID: "fleet", Quantity: 5000, Label: "Fleet"},
}

// IsLive reports whether live Stripe keys are configured.
func (c *Config) IsLive() bool { return strings.Contains(c.StripeSecretKey, "_live_") }

// LoadConfig reads configuration through getenv (os.Getenv in production, a map in
// tests) and fails fast with every problem listed, instead of failing on the first customer.
func LoadConfig(getenv func(string) string) (*Config, error) {
	var errs []error
	str := func(k, def string) string {
		if v := strings.TrimSpace(getenv(k)); v != "" {
			return v
		}
		return def
	}
	num := func(k string, def int64) int64 {
		v := strings.TrimSpace(getenv(k))
		if v == "" {
			return def
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s must be an integer, got %q", k, v))
			return def
		}
		return n
	}

	c := &Config{
		Port:              str("PORT", "4242"),
		PublicBaseURL:     strings.TrimRight(str("PUBLIC_BASE_URL", "http://localhost:4242"), "/"),
		StripeSecretKey:   str("STRIPE_SECRET_KEY", ""),
		PublishableKey:    str("STRIPE_PUBLISHABLE_KEY", ""),
		WebhookSecret:     str("STRIPE_WEBHOOK_SECRET", ""),
		Currency:          strings.ToLower(str("BUNKCOIN_CURRENCY", "usd")),
		UnitAmount:        num("BUNKCOIN_UNIT_AMOUNT", 100),
		MinQty:            num("BUNKCOIN_MIN_QTY", 10),
		MaxQty:            num("BUNKCOIN_MAX_QTY", 100000),
		SessionTTLMinutes: num("CHECKOUT_SESSION_TTL_MINUTES", 30),
		Packages:          DefaultPackages,
		AuthMode:          strings.ToLower(str("AUTH_MODE", "dev")),
		AuthSecret:        []byte(str("AUTH_SECRET", "")),
		DevIdentity: Identity{
			UserID: strings.ToUpper(str("DEV_USER_ID", "01JA2Y7K3V9QF6M8N4P5R0S1TW")),
			OrgID:  strings.ToUpper(str("DEV_ORG_ID", "01JA2Y6H8D4C2B0Z9X7W5V3T1R")),
		},
		LogLevel: strings.ToLower(str("LOG_LEVEL", "info")),
	}
	c.LogFormat = strings.ToLower(str("LOG_FORMAT", map[bool]string{true: "json", false: "pretty"}[c.IsLive()]))

	switch {
	case c.StripeSecretKey == "":
		errs = append(errs, errors.New("STRIPE_SECRET_KEY is required"))
	case !strings.HasPrefix(c.StripeSecretKey, "sk_") && !strings.HasPrefix(c.StripeSecretKey, "rk_"):
		errs = append(errs, errors.New("STRIPE_SECRET_KEY must start with sk_ or rk_"))
	}
	if !strings.HasPrefix(c.PublishableKey, "pk_") {
		errs = append(errs, errors.New("STRIPE_PUBLISHABLE_KEY is required and must start with pk_"))
	}
	if c.StripeSecretKey != "" && c.PublishableKey != "" &&
		strings.Contains(c.StripeSecretKey, "_live_") != strings.Contains(c.PublishableKey, "_live_") {
		errs = append(errs, errors.New("secret and publishable keys are from different modes (test vs live)"))
	}
	if c.WebhookSecret == "" {
		errs = append(errs, errors.New("STRIPE_WEBHOOK_SECRET is required (run `make webhooks` locally to get one)"))
	}
	if len(c.Currency) != 3 {
		errs = append(errs, fmt.Errorf("BUNKCOIN_CURRENCY %q must be a 3-letter ISO code", c.Currency))
	}
	if c.UnitAmount <= 0 {
		errs = append(errs, errors.New("BUNKCOIN_UNIT_AMOUNT must be > 0"))
	}
	if c.MinQty <= 0 || c.MaxQty < c.MinQty {
		errs = append(errs, errors.New("need 0 < BUNKCOIN_MIN_QTY <= BUNKCOIN_MAX_QTY"))
	}
	if c.UnitAmount > 0 && c.MaxQty > StripeMaxAmount/c.UnitAmount {
		errs = append(errs, fmt.Errorf("BUNKCOIN_MAX_QTY × BUNKCOIN_UNIT_AMOUNT exceeds Stripe's per-payment cap of %d minor units", StripeMaxAmount))
	}
	if c.SessionTTLMinutes < 30 || c.SessionTTLMinutes > 1440 {
		errs = append(errs, errors.New("CHECKOUT_SESSION_TTL_MINUTES must be between 30 and 1440"))
	}

	switch c.AuthMode {
	case "dev":
		if c.IsLive() {
			errs = append(errs, errors.New("AUTH_MODE=dev (automatic demo session) is not allowed with live Stripe keys"))
		}
		if !IsULID(c.DevIdentity.UserID) || !IsULID(c.DevIdentity.OrgID) {
			errs = append(errs, errors.New("DEV_USER_ID and DEV_ORG_ID must be valid ULIDs (26 chars, Crockford base32)"))
		}
		if len(c.AuthSecret) == 0 { // fine locally: the dev session just restarts with the server
			b := make([]byte, 32)
			_, _ = rand.Read(b)
			c.AuthSecret = []byte(hex.EncodeToString(b))
		}
	case "jwt":
		if len(c.AuthSecret) < 32 {
			errs = append(errs, errors.New("AUTH_SECRET must be at least 32 characters when AUTH_MODE=jwt (same HS256 secret your auth service signs with)"))
		}
	default:
		errs = append(errs, fmt.Errorf(`AUTH_MODE must be "dev" or "jwt", got %q`, c.AuthMode))
	}

	if len(errs) > 0 {
		return nil, fmt.Errorf("invalid configuration:\n  - %w", joinLines(errs))
	}
	return c, nil
}

func joinLines(errs []error) error {
	parts := make([]string, len(errs))
	for i, e := range errs {
		parts[i] = e.Error()
	}
	return errors.New(strings.Join(parts, "\n  - "))
}
