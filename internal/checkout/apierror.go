package checkout

// One error shape for every endpoint:
//
//	{"error":{"code":"...","message":"...","retryable":true,"request_id":"req_…","fields":[...]}}
//
// `message` is always safe to show a customer; internals only go to logs.

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"

	"github.com/stripe/stripe-go/v87"
)

type APIError struct {
	Status    int          `json:"-"`
	Code      string       `json:"code"`
	Message   string       `json:"message"`
	Retryable bool         `json:"retryable"`
	RequestID string       `json:"request_id,omitempty"`
	Fields    []FieldError `json:"fields,omitempty"`
	Cause     error        `json:"-"`
}

func (e *APIError) Error() string { return e.Code + ": " + e.Message }

func newAPIError(status int, code, msg string) *APIError {
	return &APIError{Status: status, Code: code, Message: msg}
}

func badRequest(code, msg string) *APIError { return newAPIError(http.StatusBadRequest, code, msg) }

func validationError(fe *FieldError) *APIError {
	return &APIError{Status: http.StatusUnprocessableEntity, Code: "validation_failed", Message: fe.Message, Fields: []FieldError{*fe}}
}

func writeJSONResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, r *http.Request, e *APIError) {
	e.RequestID = requestID(r.Context())
	writeJSONResponse(w, e.Status, map[string]*APIError{"error": e})
}

// fromStripe maps Stripe / network failures onto customer-safe errors.
// Our bugs and config problems → generic 5xx. Transient problems → retryable.
func fromStripe(err error) *APIError {
	e := &APIError{Cause: err}
	if errors.Is(err, context.DeadlineExceeded) {
		e.Status, e.Code, e.Retryable, e.Message = 504, "payment_provider_timeout", true,
			"Our payment provider took too long to respond. Please try again."
		return e
	}
	var se *stripe.Error
	if !errors.As(err, &se) {
		var ne net.Error
		if errors.As(err, &ne) {
			e.Status, e.Code, e.Retryable, e.Message = 503, "payment_provider_unavailable", true,
				"We couldn't reach our payment provider. Please try again in a moment."
		} else {
			e.Status, e.Code, e.Message = 500, "internal_error", "Something went wrong on our side. Please try again later."
		}
		return e
	}
	switch {
	case se.HTTPStatusCode == 401 || se.HTTPStatusCode == 403:
		e.Status, e.Code, e.Message = 500, "payments_misconfigured", "Payments are temporarily unavailable. Our team has been notified."
	case se.Type == stripe.ErrorTypeRateLimit || se.HTTPStatusCode == 429:
		e.Status, e.Code, e.Retryable, e.Message = 429, "rate_limited", true,
			"We're handling a lot of requests right now. Please wait a few seconds and try again."
	case se.Type == stripe.ErrorTypeIdempotency:
		e.Status, e.Code, e.Message = 409, "duplicate_request", "This request conflicts with an earlier one. Please refresh and try again."
	case se.Type == stripe.ErrorTypeCard:
		e.Status, e.Code, e.Message = 402, "card_error", se.Msg // Stripe writes these for end users
	case se.Code == stripe.ErrorCodeResourceMissing:
		e.Status, e.Code, e.Message = 404, "not_found", "We couldn't find that checkout session."
	case se.Code == stripe.ErrorCodeAmountTooLarge || se.Code == stripe.ErrorCodeAmountTooSmall:
		e.Status, e.Code, e.Message = 422, "amount_out_of_range", "That amount can't be processed. Please choose a different amount."
	case se.Type == stripe.ErrorTypeInvalidRequest:
		e.Status, e.Code, e.Message = 502, "payment_setup_failed", "We couldn't start the payment. Please try again later."
	default: // api_error / Stripe 5xx
		e.Status, e.Code, e.Retryable, e.Message = 502, "payment_provider_error", true, "Our payment provider had a problem. Please try again."
	}
	return e
}

// stripeLogFields returns Stripe error details for logs (never for responses).
func stripeLogFields(err error) []any {
	var se *stripe.Error
	if !errors.As(err, &se) {
		return []any{"reason", err.Error()}
	}
	return []any{"stripe_type", string(se.Type), "stripe_code", string(se.Code), "stripe_param", se.Param,
		"stripe_req", se.RequestID, "reason", se.Msg}
}
