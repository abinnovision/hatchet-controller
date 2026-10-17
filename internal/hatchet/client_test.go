package hatchet

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeSigner encodes subject and tenant ID so tests can assert them from the header.
type fakeSigner struct{}

func (fakeSigner) Sign(subject, tenantID string, _ time.Time) (string, error) {
	return subject + "|" + tenantID, nil
}

type request struct {
	method, path, auth, exchange string
	body                         map[string]string
}

// serve starts a server that records the last request and replies with status and body.
func serve(t *testing.T, status int, body string) (Client, *request) {
	t.Helper()
	got := &request{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path = r.Method, r.URL.Path
		got.auth, got.exchange = r.Header.Get("Authorization"), r.Header.Get("X-Exchange-Token")
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			_ = json.Unmarshal(b, &got.body)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL+"/", fakeSigner{}, "svc", srv.Client()), got
}

func assertRequest(t *testing.T, got *request, method, path, tenantID string) {
	t.Helper()
	if got.method != method || got.path != path {
		t.Errorf("request = %s %s, want %s %s", got.method, got.path, method, path)
	}
	if want := "Bearer svc|" + tenantID; got.auth != want {
		t.Errorf("Authorization = %q, want %q", got.auth, want)
	}
	if got.exchange != "1" {
		t.Errorf("X-Exchange-Token = %q", got.exchange)
	}
}

func apiToken(t *testing.T, claims string) string {
	t.Helper()
	return "e30." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".sig"
}

func TestCreateTenant(t *testing.T) {
	c, got := serve(t, http.StatusOK, `{"metadata":{"id":"t1"},"name":"n","slug":"ns.n-abc"}`)
	tenant, err := c.CreateTenant(context.Background(), "n", "ns.n-abc")
	if err != nil {
		t.Fatal(err)
	}
	assertRequest(t, got, http.MethodPost, "/api/v1/tenants", nilTenantID)
	if got.body["name"] != "n" || got.body["slug"] != "ns.n-abc" {
		t.Errorf("body = %v", got.body)
	}
	if tenant != (Tenant{ID: "t1", Name: "n", Slug: "ns.n-abc"}) {
		t.Errorf("tenant = %+v", tenant)
	}
}

func TestGetTenant(t *testing.T) {
	c, got := serve(t, http.StatusOK, `{"metadata":{"id":"t1"},"name":"n","slug":"s"}`)
	tenant, err := c.GetTenant(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	assertRequest(t, got, http.MethodGet, "/api/v1/tenants/t1", "t1")
	if tenant.ID != "t1" || tenant.Slug != "s" {
		t.Errorf("tenant = %+v", tenant)
	}
}

func TestCreateAPIToken(t *testing.T) {
	value := apiToken(t, `{"sub":"t1","token_id":"tok-1"}`)
	c, got := serve(t, http.StatusOK, `{"token":"`+value+`"}`)
	token, err := c.CreateAPIToken(context.Background(), "t1", "k8s:uid:1", 720*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	assertRequest(t, got, http.MethodPost, "/api/v1/tenants/t1/api-tokens", "t1")
	if got.body["name"] != "k8s:uid:1" {
		t.Errorf("name = %q", got.body["name"])
	}
	if d, err := time.ParseDuration(got.body["expiresIn"]); err != nil || d != 720*time.Hour {
		t.Errorf("expiresIn = %q", got.body["expiresIn"])
	}
	if token != (Token{ID: "tok-1", Value: value}) {
		t.Errorf("token = %+v", token)
	}
}

func TestCreateAPITokenWithoutTokenID(t *testing.T) {
	c, _ := serve(t, http.StatusOK, `{"token":"`+apiToken(t, `{"sub":"t1"}`)+`"}`)
	if _, err := c.CreateAPIToken(context.Background(), "t1", "n", time.Hour); err == nil {
		t.Fatal("expected error for missing token_id")
	}
}

func TestListAPITokens(t *testing.T) {
	c, got := serve(t, http.StatusOK, `{"rows":[{"metadata":{"id":"tok-1"},"name":"a","expiresAt":"2026-01-02T03:04:05Z"}]}`)
	tokens, err := c.ListAPITokens(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	assertRequest(t, got, http.MethodGet, "/api/v1/tenants/t1/api-tokens", "t1")
	want := TokenInfo{ID: "tok-1", Name: "a", ExpiresAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	if len(tokens) != 1 || tokens[0].ID != want.ID || tokens[0].Name != want.Name || !tokens[0].ExpiresAt.Equal(want.ExpiresAt) {
		t.Errorf("tokens = %+v", tokens)
	}
}

func TestRevokeAPIToken(t *testing.T) {
	c, got := serve(t, http.StatusNoContent, "")
	if err := c.RevokeAPIToken(context.Background(), "t1", "tok-1"); err != nil {
		t.Fatal(err)
	}
	assertRequest(t, got, http.MethodPost, "/api/v1/api-tokens/tok-1", "t1")
}

func TestErrorMapping(t *testing.T) {
	long := strings.Repeat("x", 300)
	tests := []struct {
		name    string
		status  int
		body    string
		want    error
		message string
	}{
		{"401", 401, `{"errors":[{"description":"Not authorized"}]}`, ErrUnauthorized, "Not authorized"},
		{"403", 403, `forbidden`, ErrUnauthorized, "hatchet returned status 403"},
		{"404", 404, `{"errors":[{"description":"not found"}]}`, ErrNotFound, "not found"},
		{"slug taken", 400, `{"errors":[{"description":"Tenant with that slug already exists."}]}`, ErrSlugTaken, "Tenant with that slug already exists."},
		{"other 400", 400, `{"errors":[{"description":"bad"}]}`, nil, "bad"},
		{"5xx ignores body", 500, `{"errors":[{"description":"internal detail"}]}`, nil, "hatchet returned status 500"},
		{"truncated", 400, `{"errors":[{"description":"` + long + `"}]}`, nil, strings.Repeat("x", maxMessageLen) + "..."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := serve(t, tt.status, tt.body)
			_, err := c.CreateTenant(context.Background(), "n", "s")
			var herr *Error
			if !errors.As(err, &herr) {
				t.Fatalf("err = %v, want *Error", err)
			}
			if herr.StatusCode != tt.status || herr.Message != tt.message {
				t.Errorf("got %d %q, want %d %q", herr.StatusCode, herr.Message, tt.status, tt.message)
			}
			for _, sentinel := range []error{ErrUnauthorized, ErrNotFound, ErrSlugTaken} {
				if errors.Is(err, sentinel) != (sentinel == tt.want) {
					t.Errorf("errors.Is(%v) = %v", sentinel, sentinel != tt.want)
				}
			}
		})
	}
}

func TestUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	c := New(srv.URL, fakeSigner{}, "svc", http.DefaultClient)
	_, err := c.GetTenant(context.Background(), "t1")
	var herr *Error
	if !errors.As(err, &herr) || herr.StatusCode != 0 || herr.Message != "hatchet unreachable" || herr.Err == nil {
		t.Fatalf("err = %v, want unreachable *Error", err)
	}
}
