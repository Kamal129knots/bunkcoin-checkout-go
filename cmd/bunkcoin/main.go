// Command bunkcoin serves the Bunk Coin top-up pages and API, and fulfils
// purchases from Stripe webhooks.
//
// Stripe API version: 2026-09-30.endive (pinned by stripe-go v87).
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/129knots/bunkcoin-checkout/internal/checkout"
	"github.com/129knots/bunkcoin-checkout/web"
	"github.com/stripe/stripe-go/v87"
)

func main() {
	if err := checkout.LoadDotEnv(".env"); err != nil {
		fmt.Fprintln(os.Stderr, "reading .env:", err)
		os.Exit(1)
	}
	cfg, err := checkout.LoadConfig(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		fmt.Fprintln(os.Stderr, "\nCopy .env.example to .env and fill in your Stripe test keys.")
		os.Exit(1)
	}
	log := checkout.NewLogger(os.Stdout, cfg.LogFormat, cfg.LogLevel)

	// Our logger already records every Stripe failure with context; silence stripe-go's own output.
	stripe.DefaultLeveledLogger = &stripe.LeveledLogger{Level: stripe.LevelNull}
	sc := stripe.NewClient(cfg.StripeSecretKey) // retries network failures twice; every create has an idempotency key

	// PRODUCTION: replace both with real implementations:
	//   store  → Postgres (bunkcoin_orders + organisations.stripe_customer_id)
	//   ledger → your BunkCoin balance service (Credit idempotent by order id)
	srv := &checkout.Server{
		Cfg:            cfg,
		Sessions:       sc.V1CheckoutSessions,
		Customers:      sc.V1Customers,
		Store:          checkout.NewMemoryStore(),
		Ledger:         checkout.NewMemoryLedger(),
		Auth:           checkout.NewAuth(cfg.AuthMode, cfg.AuthSecret, cfg.DevIdentity, strings.HasPrefix(cfg.PublicBaseURL, "https://"), log),
		Log:            log,
		ConstructEvent: checkout.VerifyStripeEvent,
		StripeTimeout:  20 * time.Second,
	}

	httpSrv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           srv.Routes(web.FS),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		mode := "test"
		if cfg.IsLive() {
			mode = "LIVE"
		}
		log.Info("SYSTEM", "BunkCoin checkout running → http://localhost:"+cfg.Port, "stripe_api", stripe.APIVersion,
			"mode", mode, "auth", cfg.AuthMode, "price", fmt.Sprintf("%d %s/coin", cfg.UnitAmount, cfg.Currency))
		if cfg.AuthMode == "dev" {
			log.Info("SYSTEM", "Dev session identity", "user", cfg.DevIdentity.UserID, "org", cfg.DevIdentity.OrgID)
			log.Info("SYSTEM", "Orders and balances are in memory — restarting clears them")
		}
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("SYSTEM", "server error", "reason", err.Error())
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	sig := <-stop
	log.Info("SYSTEM", sig.String()+" received, shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
}
