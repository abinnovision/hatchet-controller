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

// Webhook is an incoming webhook. Hatchet never returns Auth on reads.
type Webhook struct {
	Name               string
	SourceName         string
	EventKeyExpression string
	ScopeExpression    string
	// StaticPayload is a JSON object.
	StaticPayload map[string]any
	// AuthType is BASIC, API_KEY or HMAC.
	AuthType string
	Auth     WebhookAuth
}

// WebhookAuth holds the credentials for the member matching Webhook.AuthType.
type WebhookAuth struct {
	Username string
	Password string

	HeaderName string
	APIKey     string

	Algorithm           string
	Encoding            string
	SignatureHeaderName string
	SigningSecret       string
}

// Member is a tenant member. Hatchet does not return the user ID.
type Member struct {
	ID    string
	Email string
	Role  string
}

// Invite is a pending, unexpired tenant invite.
type Invite struct {
	ID      string
	Email   string
	Role    string
	Expires time.Time
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
	// GetWebhook returns an incoming webhook of the tenant without its auth; a missing one is ErrNotFound.
	GetWebhook(ctx context.Context, tenantID, name string) (Webhook, error)
	// CreateWebhook creates an incoming webhook in the tenant.
	CreateWebhook(ctx context.Context, tenantID string, w Webhook) error
	// UpdateWebhook sets the mutable fields of an incoming webhook; an empty scope clears it.
	UpdateWebhook(ctx context.Context, tenantID, name, eventKeyExpression, scopeExpression string, staticPayload map[string]any) error
	// DeleteWebhook deletes an incoming webhook of the tenant.
	DeleteWebhook(ctx context.Context, tenantID, name string) error
	// ListMembers lists the members of the tenant.
	ListMembers(ctx context.Context, tenantID string) ([]Member, error)
	// UpdateMember sets the role of a member.
	UpdateMember(ctx context.Context, tenantID, memberID, role string) error
	// DeleteMember removes a member from the tenant.
	DeleteMember(ctx context.Context, tenantID, memberID string) error
	// ListInvites lists the pending invites of the tenant.
	ListInvites(ctx context.Context, tenantID string) ([]Invite, error)
	// CreateInvite invites an email to the tenant.
	CreateInvite(ctx context.Context, tenantID, email, role string) error
	// UpdateInvite sets the role of a pending invite.
	UpdateInvite(ctx context.Context, tenantID, inviteID, role string) error
	// DeleteInvite deletes a pending invite.
	DeleteInvite(ctx context.Context, tenantID, inviteID string) error
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
// errors[].description or top-level description truncated to about 256 characters, or only the status code.
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

func webhooksPath(tenantID string) string {
	return "/api/v1/stable/tenants/" + url.PathEscape(tenantID) + "/webhooks"
}

func (c *client) GetWebhook(ctx context.Context, tenantID, name string) (Webhook, error) {
	var out struct {
		Name               string         `json:"name"`
		SourceName         string         `json:"sourceName"`
		EventKeyExpression string         `json:"eventKeyExpression"`
		ScopeExpression    string         `json:"scopeExpression"`
		StaticPayload      map[string]any `json:"staticPayload"`
		AuthType           string         `json:"authType"`
	}
	if err := c.do(ctx, http.MethodGet, webhooksPath(tenantID)+"/"+url.PathEscape(name), tenantID, nil, &out); err != nil {
		return Webhook{}, err
	}
	return Webhook{
		Name: out.Name, SourceName: out.SourceName, EventKeyExpression: out.EventKeyExpression,
		ScopeExpression: out.ScopeExpression, StaticPayload: out.StaticPayload, AuthType: out.AuthType,
	}, nil
}

func (c *client) CreateWebhook(ctx context.Context, tenantID string, w Webhook) error {
	body := map[string]any{
		"name":               w.Name,
		"sourceName":         w.SourceName,
		"eventKeyExpression": w.EventKeyExpression,
		"authType":           w.AuthType,
	}
	if w.ScopeExpression != "" {
		body["scopeExpression"] = w.ScopeExpression
	}
	if w.StaticPayload != nil {
		body["staticPayload"] = w.StaticPayload
	}
	switch w.AuthType {
	case "BASIC":
		body["auth"] = map[string]string{"username": w.Auth.Username, "password": w.Auth.Password}
	case "API_KEY":
		body["auth"] = map[string]string{"headerName": w.Auth.HeaderName, "apiKey": w.Auth.APIKey}
	case "HMAC":
		body["auth"] = map[string]string{
			"algorithm": w.Auth.Algorithm, "encoding": w.Auth.Encoding,
			"signatureHeaderName": w.Auth.SignatureHeaderName, "signingSecret": w.Auth.SigningSecret,
		}
	default:
		return fmt.Errorf("unsupported webhook auth type %q", w.AuthType)
	}
	return c.do(ctx, http.MethodPost, webhooksPath(tenantID), tenantID, body, nil)
}

// UpdateWebhook always sends scopeExpression and staticPayload because Hatchet ignores null fields.
func (c *client) UpdateWebhook(ctx context.Context, tenantID, name, eventKeyExpression, scopeExpression string,
	staticPayload map[string]any) error {
	if staticPayload == nil {
		staticPayload = map[string]any{}
	}
	body := map[string]any{
		"eventKeyExpression": eventKeyExpression,
		"scopeExpression":    scopeExpression,
		"staticPayload":      staticPayload,
	}
	return c.do(ctx, http.MethodPatch, webhooksPath(tenantID)+"/"+url.PathEscape(name), tenantID, body, nil)
}

func (c *client) DeleteWebhook(ctx context.Context, tenantID, name string) error {
	return c.do(ctx, http.MethodDelete, webhooksPath(tenantID)+"/"+url.PathEscape(name), tenantID, nil, nil)
}

func tenantPath(tenantID, collection string) string {
	return "/api/v1/tenants/" + url.PathEscape(tenantID) + "/" + collection
}

func (c *client) ListMembers(ctx context.Context, tenantID string) ([]Member, error) {
	var out struct {
		Rows []struct {
			Metadata struct {
				ID string `json:"id"`
			} `json:"metadata"`
			User struct {
				Email string `json:"email"`
			} `json:"user"`
			Role string `json:"role"`
		} `json:"rows"`
	}
	if err := c.do(ctx, http.MethodGet, tenantPath(tenantID, "members"), tenantID, nil, &out); err != nil {
		return nil, err
	}
	members := make([]Member, 0, len(out.Rows))
	for _, r := range out.Rows {
		members = append(members, Member{ID: r.Metadata.ID, Email: r.User.Email, Role: r.Role})
	}
	return members, nil
}

func (c *client) UpdateMember(ctx context.Context, tenantID, memberID, role string) error {
	path := tenantPath(tenantID, "members") + "/" + url.PathEscape(memberID)
	return c.do(ctx, http.MethodPatch, path, tenantID, map[string]string{"role": role}, nil)
}

func (c *client) DeleteMember(ctx context.Context, tenantID, memberID string) error {
	return c.do(ctx, http.MethodDelete, tenantPath(tenantID, "members")+"/"+url.PathEscape(memberID), tenantID, nil, nil)
}

func (c *client) ListInvites(ctx context.Context, tenantID string) ([]Invite, error) {
	var out struct {
		Rows []struct {
			Metadata struct {
				ID string `json:"id"`
			} `json:"metadata"`
			Email   string    `json:"email"`
			Role    string    `json:"role"`
			Expires time.Time `json:"expires"`
		} `json:"rows"`
	}
	if err := c.do(ctx, http.MethodGet, tenantPath(tenantID, "invites"), tenantID, nil, &out); err != nil {
		return nil, err
	}
	invites := make([]Invite, 0, len(out.Rows))
	for _, r := range out.Rows {
		invites = append(invites, Invite{ID: r.Metadata.ID, Email: r.Email, Role: r.Role, Expires: r.Expires})
	}
	return invites, nil
}

func (c *client) CreateInvite(ctx context.Context, tenantID, email, role string) error {
	body := map[string]string{"email": email, "role": role}
	return c.do(ctx, http.MethodPost, tenantPath(tenantID, "invites"), tenantID, body, nil)
}

func (c *client) UpdateInvite(ctx context.Context, tenantID, inviteID, role string) error {
	path := tenantPath(tenantID, "invites") + "/" + url.PathEscape(inviteID)
	return c.do(ctx, http.MethodPatch, path, tenantID, map[string]string{"role": role}, nil)
}

func (c *client) DeleteInvite(ctx context.Context, tenantID, inviteID string) error {
	return c.do(ctx, http.MethodDelete, tenantPath(tenantID, "invites")+"/"+url.PathEscape(inviteID), tenantID, nil, nil)
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
			Description string `json:"description"`
			Errors      []struct {
				Description string `json:"description"`
			} `json:"errors"`
		}
		if json.Unmarshal(body, &parsed) == nil {
			desc = parsed.Description
			if len(parsed.Errors) > 0 {
				desc = parsed.Errors[0].Description
			}
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
