// Package hatchet is a minimal client for the Hatchet REST endpoints used with exchange tokens.
package hatchet

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Tenant is a Hatchet tenant.
type Tenant struct {
	ID   string
	Name string
	Slug string
}

// Token is a newly created API token; Value is only returned on creation.
type Token struct {
	ID    string
	Value string
}

// TokenInfo describes an existing API token.
type TokenInfo struct {
	ID        string
	Name      string
	ExpiresAt time.Time
}

// Client covers the Hatchet API calls the controller needs.
type Client interface {
	// CreateTenant creates a tenant owned by the service user.
	CreateTenant(ctx context.Context, name, slug string) (Tenant, error)
	// GetTenant returns a tenant the service user is a member of.
	GetTenant(ctx context.Context, id string) (Tenant, error)
	// CreateAPIToken creates an API token in the tenant.
	CreateAPIToken(ctx context.Context, tenantID, name string, expiresIn time.Duration) (Token, error)
	// ListAPITokens lists non-revoked API tokens of the tenant, including expired ones.
	ListAPITokens(ctx context.Context, tenantID string) ([]TokenInfo, error)
	// RevokeAPIToken revokes an API token of the tenant.
	RevokeAPIToken(ctx context.Context, tenantID, tokenID string) error
}

// TokenSource signs exchange JWTs; keyset.Signer implements it.
type TokenSource interface {
	Sign(subject, tenantID string, now time.Time) (string, error)
}

// Sentinel errors wrapped by *Error; match them with errors.Is.
var (
	// ErrUnauthorized maps 401 (service user unverified or not a member) and 403 (key not trusted).
	ErrUnauthorized = errors.New("unauthorized")
	// ErrNotFound maps 404.
	ErrNotFound = errors.New("not found")
	// ErrSlugTaken maps 400 "Tenant with that slug already exists.".
	ErrSlugTaken = errors.New("slug already exists")
)

// Error is a failed Hatchet call. Message is safe for status conditions: the parsed
// errors[].description truncated to about 256 characters, or only the status code.
type Error struct {
	StatusCode int
	Message    string
	Err        error
}

func (e *Error) Error() string {
	msg := e.Message
	if e.StatusCode != 0 {
		msg = fmt.Sprintf("status %d: %s", e.StatusCode, msg)
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

// nilTenantID is the exchange JWT tenant_id for routes that are not tenant-scoped.
const nilTenantID = "00000000-0000-0000-0000-000000000000"

// maxMessageLen bounds Error.Message.
const maxMessageLen = 256

type client struct {
	baseURL       string
	signer        TokenSource
	serviceUserID string
	http          *http.Client
}

// New returns a Client calling baseURL with exchange JWTs signed for serviceUserID.
func New(baseURL string, signer TokenSource, serviceUserID string, httpClient *http.Client) Client {
	return &client{baseURL: strings.TrimRight(baseURL, "/"), signer: signer, serviceUserID: serviceUserID, http: httpClient}
}

type apiTenant struct {
	Metadata struct {
		ID string `json:"id"`
	} `json:"metadata"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

func (t apiTenant) tenant() Tenant { return Tenant{ID: t.Metadata.ID, Name: t.Name, Slug: t.Slug} }

func (c *client) CreateTenant(ctx context.Context, name, slug string) (Tenant, error) {
	var out apiTenant
	body := map[string]string{"name": name, "slug": slug}
	if err := c.do(ctx, http.MethodPost, "/api/v1/tenants", nilTenantID, body, &out); err != nil {
		return Tenant{}, err
	}
	return out.tenant(), nil
}

func (c *client) GetTenant(ctx context.Context, id string) (Tenant, error) {
	var out apiTenant
	if err := c.do(ctx, http.MethodGet, "/api/v1/tenants/"+url.PathEscape(id), id, nil, &out); err != nil {
		return Tenant{}, err
	}
	return out.tenant(), nil
}

func (c *client) CreateAPIToken(ctx context.Context, tenantID, name string, expiresIn time.Duration) (Token, error) {
	var out struct {
		Token string `json:"token"`
	}
	body := map[string]string{"name": name, "expiresIn": expiresIn.String()}
	path := "/api/v1/tenants/" + url.PathEscape(tenantID) + "/api-tokens"
	if err := c.do(ctx, http.MethodPost, path, tenantID, body, &out); err != nil {
		return Token{}, err
	}
	id, err := tokenID(out.Token)
	if err != nil {
		return Token{}, err
	}
	return Token{ID: id, Value: out.Token}, nil
}

func (c *client) ListAPITokens(ctx context.Context, tenantID string) ([]TokenInfo, error) {
	var out struct {
		Rows []struct {
			Metadata struct {
				ID string `json:"id"`
			} `json:"metadata"`
			Name      string    `json:"name"`
			ExpiresAt time.Time `json:"expiresAt"`
		} `json:"rows"`
	}
	path := "/api/v1/tenants/" + url.PathEscape(tenantID) + "/api-tokens"
	if err := c.do(ctx, http.MethodGet, path, tenantID, nil, &out); err != nil {
		return nil, err
	}
	tokens := make([]TokenInfo, 0, len(out.Rows))
	for _, r := range out.Rows {
		tokens = append(tokens, TokenInfo{ID: r.Metadata.ID, Name: r.Name, ExpiresAt: r.ExpiresAt})
	}
	return tokens, nil
}

func (c *client) RevokeAPIToken(ctx context.Context, tenantID, tokenID string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/api-tokens/"+url.PathEscape(tokenID), tenantID, nil, nil)
}

// do sends an exchange-token request and decodes a 2xx JSON response into out, if non-nil.
func (c *client) do(ctx context.Context, method, path, tenantID string, in, out any) error {
	jwt, err := c.signer.Sign(c.serviceUserID, tenantID, time.Now())
	if err != nil {
		return fmt.Errorf("signing exchange token: %w", err)
	}

	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("X-Exchange-Token", "1")
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return &Error{Message: "hatchet unreachable", Err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return &Error{StatusCode: resp.StatusCode, Message: "hatchet unreachable", Err: err}
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("decoding %s %s response: %w", method, path, err)
		}
		return nil
	}
	return newError(resp.StatusCode, respBody)
}

func newError(status int, body []byte) *Error {
	e := &Error{StatusCode: status, Message: fmt.Sprintf("hatchet returned status %d", status)}
	var desc string
	if status < 500 {
		var parsed struct {
			Errors []struct {
				Description string `json:"description"`
			} `json:"errors"`
		}
		if json.Unmarshal(body, &parsed) == nil && len(parsed.Errors) > 0 {
			desc = parsed.Errors[0].Description
		}
		if desc != "" {
			e.Message = truncate(desc, maxMessageLen)
		}
	}

	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		e.Err = ErrUnauthorized
	case status == http.StatusNotFound:
		e.Err = ErrNotFound
	case status == http.StatusBadRequest && desc == "Tenant with that slug already exists.":
		e.Err = ErrSlugTaken
	}
	return e
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

// tokenID reads the token_id claim from an API token without verifying it.
func tokenID(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errors.New("api token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("decoding api token payload: %w", err)
	}
	var claims struct {
		TokenID string `json:"token_id"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("parsing api token payload: %w", err)
	}
	if claims.TokenID == "" {
		return "", errors.New("api token has no token_id claim")
	}
	return claims.TokenID, nil
}
