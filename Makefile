EVENTS := checkout.session.completed,checkout.session.async_payment_succeeded,checkout.session.async_payment_failed,checkout.session.expired

.PHONY: run webhooks test build docker

## run: start the server (reads .env); restart after changes
run:
	go run ./cmd/bunkcoin

## webhooks: forward Stripe events to the local server (copy the whsec_ it prints into .env)
webhooks:
	stripe listen --events $(EVENTS) --forward-to localhost:4242/webhooks/stripe

## test: unit + HTTP tests with the race detector
test:
	go test -race -count=1 ./...

## build: static binary in ./bin
build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/bunkcoin ./cmd/bunkcoin

docker:
	docker build -t bunkcoin-checkout .
