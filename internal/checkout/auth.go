package checkout

// Who is buying? Identity is ALWAYS read from a session the server can verify
// (signed cookie or signed JWT) — never from the request body.
//
// Identity = { UserID, OrgID }, both ULIDs. No names or emails: the purchase flow
// doesn't need them, so no personal data ends up in Stripe metadata or logs.
//
// AUTH_MODE=dev → local demo: the first request gets a signed session cookie for
//                 DEV_USER_ID / DEV_ORG_ID, like a logged-in platform user.
//                 Refused with live Stripe keys.
// AUTH_MODE=jwt → production: verify the HS256 JWT from the 129Knots auth service
//                 (Authorization: Bearer <token>, or cookie "bkc_token").
//                 Required claims: sub (user ULID), org_id (org ULID), exp.
//                 If your auth service uses RS256/JWKS, replace verifyJWT.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

type Identity struct {
	UserID string
	OrgID  string
}

type ctxKey int

const identityKey ctxKey = 1

// IdentityFrom returns the session identity, or nil if there is none.
func IdentityFrom(ctx context.Context) *Identity {
	id, _ := ctx.Value(identityKey).(*Identity)
	return id
}

const (
	devCookie     = "bkc_session"
	jwtCookie     = "bkc_token"
	devSessionTTL = 8 * time.Hour
)

var b64 = base64.RawURLEncoding

type Auth struct {
	Mode          string
	secret        []byte
	devIdentity   Identity
	secureCookies bool
	log           *Logger
}

func NewAuth(mode string, secret []byte, devIdentity Identity, secureCookies bool, log *Logger) *Auth {
	return &Auth{Mode: mode, secret: secret, devIdentity: devIdentity, secureCookies: secureCookies, log: log}
}

func toIdentity(userID, orgID string) *Identity {
	if IsULID(userID) && IsULID(orgID) {
		return &Identity{UserID: userID, OrgID: orgID}
	}
	return nil // only well-formed ULIDs can reach metadata or the ledger
}

// Middleware puts the verified identity (or nil) on the request context. It never
// rejects by itself — routes that need a session use requireAuth.
func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var id *Identity
		if a.Mode == "dev" {
			if c, err := r.Cookie(devCookie); err == nil {
				id = a.readDevSession(c.Value)
			}
			if id == nil {
				id = a.issueDevSession(w)
				a.log.Info("AUTH", "Session started (dev)", "user", id.UserID, "org", id.OrgID)
			}
		} else {
			token := ""
			if h := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(h), "bearer ") {
				token = strings.TrimSpace(h[7:])
			} else if c, err := r.Cookie(jwtCookie); err == nil {
				token = c.Value
			}
			if claims := VerifyJWT(token, a.secret, time.Now()); claims != nil {
				id = toIdentity(claims.Sub, claims.OrgID)
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey, id)))
	})
}

type sessionClaims struct {
	Sub   string `json:"sub"`
	OrgID string `json:"org_id"`
	Exp   int64  `json:"exp"`
}

func (a *Auth) sign(payload string) string {
	m := hmac.New(sha256.New, a.secret)
	m.Write([]byte(payload))
	return payload + "." + b64.EncodeToString(m.Sum(nil))
}

func (a *Auth) issueDevSession(w http.ResponseWriter) *Identity {
	body, _ := json.Marshal(sessionClaims{Sub: a.devIdentity.UserID, OrgID: a.devIdentity.OrgID, Exp: time.Now().Add(devSessionTTL).Unix()})
	http.SetCookie(w, &http.Cookie{
		Name: devCookie, Value: a.sign(b64.EncodeToString(body)), Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: a.secureCookies, MaxAge: int(devSessionTTL.Seconds()),
	})
	id := a.devIdentity
	return &id
}

func (a *Auth) readDevSession(value string) *Identity {
	payload, _, ok := strings.Cut(value, ".")
	if !ok || !hmac.Equal([]byte(a.sign(payload)), []byte(value)) {
		return nil
	}
	raw, err := b64.DecodeString(payload)
	if err != nil {
		return nil
	}
	var c sessionClaims
	if json.Unmarshal(raw, &c) != nil || c.Exp < time.Now().Unix() {
		return nil
	}
	return toIdentity(c.Sub, c.OrgID)
}

// VerifyJWT checks an HS256 JWT and returns its claims, or nil if invalid/expired.
func VerifyJWT(token string, secret []byte, now time.Time) *sessionClaims {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	var header struct {
		Alg string `json:"alg"`
	}
	hb, err1 := b64.DecodeString(parts[0])
	pb, err2 := b64.DecodeString(parts[1])
	sig, err3 := b64.DecodeString(parts[2])
	if err1 != nil || err2 != nil || err3 != nil || json.Unmarshal(hb, &header) != nil || header.Alg != "HS256" {
		return nil // never accept "none" or other algorithms
	}
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(sig, m.Sum(nil)) {
		return nil
	}
	var c sessionClaims
	if json.Unmarshal(pb, &c) != nil || c.Exp < now.Unix() {
		return nil
	}
	return &c
}

// SignJWT issues an HS256 JWT — used by tests; production tokens come from the auth service.
func SignJWT(sub, orgID string, exp time.Time, secret []byte) string {
	h := b64.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	body, _ := json.Marshal(sessionClaims{Sub: sub, OrgID: orgID, Exp: exp.Unix()})
	p := b64.EncodeToString(body)
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(h + "." + p))
	return h + "." + p + "." + b64.EncodeToString(m.Sum(nil))
}
