package checkout

import "fmt"

// PurchaseRequest is what the browser sends: a CHOICE, never a price or an
// organisation (who is buying comes from the session).
type PurchaseRequest struct {
	PackageID string `json:"package_id,omitempty"`
	Quantity  *int64 `json:"quantity,omitempty"`
}

// Quote is the server-calculated purchase.
type Quote struct {
	PackageID  string `json:"package_id"` // "custom" for custom amounts
	Quantity   int64  `json:"quantity"`
	UnitAmount int64  `json:"unit_amount"`
	Total      int64  `json:"total"`
	Currency   string `json:"currency"`
}

// FieldError is a validation problem the UI can show next to an input.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

func (e *FieldError) Error() string { return e.Field + ": " + e.Message }

// BuildQuote validates the choice against config and prices it. Amounts are only
// ever calculated here.
func BuildQuote(c *Config, req PurchaseRequest) (Quote, error) {
	q := Quote{UnitAmount: c.UnitAmount, Currency: c.Currency}
	switch {
	case req.PackageID != "" && req.Quantity != nil:
		return q, &FieldError{"package_id", "Choose a package or a custom amount, not both."}
	case req.PackageID != "":
		for _, p := range c.Packages {
			if p.ID == req.PackageID {
				q.PackageID, q.Quantity = p.ID, p.Quantity
			}
		}
		if q.Quantity == 0 {
			return q, &FieldError{"package_id", "That package is no longer available. Please pick another."}
		}
	case req.Quantity != nil:
		n := *req.Quantity
		if n < c.MinQty {
			return q, &FieldError{"quantity", fmt.Sprintf("Minimum purchase is %d BunkCoin.", c.MinQty)}
		}
		if n > c.MaxQty {
			return q, &FieldError{"quantity", fmt.Sprintf("Maximum purchase is %d BunkCoin. Contact us for larger orders.", c.MaxQty)}
		}
		q.PackageID, q.Quantity = "custom", n
	default:
		return q, &FieldError{"package_id", "Choose how much BunkCoin you need."}
	}
	if q.Quantity > StripeMaxAmount/q.UnitAmount { // overflow-safe
		return q, &FieldError{"quantity", "That amount is above the per-payment limit."}
	}
	q.Total = q.Quantity * q.UnitAmount
	return q, nil
}
