package testenv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	// PostgresImage is the database image used with hatchet-lite.
	PostgresImage = "postgres:15.6"
	// HatchetLiteImage is the hatchet-lite image without tag.
	HatchetLiteImage = "ghcr.io/hatchet-dev/hatchet/hatchet-lite"

	adminEmail    = "admin@example.com"
	adminPassword = "Admin123!!"
)

// Hatchet is a running hatchet-lite with its Postgres.
type Hatchet struct {
	// APIURL is the mapped HTTP base URL.
	APIURL string
	// GRPCHostPort is the mapped gRPC address.
	GRPCHostPort string
	// AdminEmail and AdminPassword log in the seeded admin.
	AdminEmail    string
	AdminPassword string
	// AdminUserID is the seeded admin's UUID, read via SQL.
	AdminUserID string

	network  *testcontainers.DockerNetwork
	postgres testcontainers.Container
	lite     testcontainers.Container
}

// HatchetOptions configures StartHatchet.
type HatchetOptions struct {
	// Version is the hatchet-lite image tag.
	Version string
	// PublicKeyset is the base64 public exchange keyset (SERVER_AUTH_CONTROL_PLANE_EXCHANGE_TOKEN_JWT_PUBLIC_KEYSET).
	PublicKeyset string
	// ServiceUserID, if set, is inserted as a verified user. HatchetInstance.spec.serviceUserID is
	// immutable and the instance must exist before Hatchet boots (it generates the keyset), so the
	// service user cannot be the seeded admin with its random UUID.
	ServiceUserID string
}

// StartHatchet starts postgres and hatchet-lite on a shared network and waits for /api/ready.
// On error all started containers are terminated.
func StartHatchet(ctx context.Context, opts HatchetOptions) (h *Hatchet, err error) {
	h = &Hatchet{AdminEmail: adminEmail, AdminPassword: adminPassword}
	defer func() {
		if err != nil {
			_ = h.Terminate(context.Background())
			h = nil
		}
	}()

	h.network, err = network.New(ctx)
	if err != nil {
		return h, fmt.Errorf("creating network: %w", err)
	}

	h.postgres, err = testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:          PostgresImage,
			Env:            map[string]string{"POSTGRES_USER": "hatchet", "POSTGRES_PASSWORD": "hatchet", "POSTGRES_DB": "hatchet"},
			Networks:       []string{h.network.Name},
			NetworkAliases: map[string][]string{h.network.Name: {"postgres"}},
			WaitingFor: wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(2 * time.Minute),
		},
		Started: true,
	})
	if err != nil {
		return h, fmt.Errorf("starting postgres: %w", err)
	}

	h.lite, err = testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        HatchetLiteImage + ":" + opts.Version,
			ExposedPorts: []string{"8888/tcp", "7070/tcp"},
			Networks:     []string{h.network.Name},
			Env: map[string]string{
				"DATABASE_URL":                                     "postgresql://hatchet:hatchet@postgres:5432/hatchet?sslmode=disable",
				"SERVER_AUTH_COOKIE_DOMAIN":                        "localhost",
				"SERVER_AUTH_COOKIE_INSECURE":                      "t",
				"SERVER_GRPC_INSECURE":                             "t",
				"SERVER_GRPC_BIND_ADDRESS":                         "0.0.0.0",
				"SERVER_GRPC_BROADCAST_ADDRESS":                    "localhost:7070",
				"SERVER_URL":                                       "http://localhost:8888",
				"SERVER_AUTH_SET_EMAIL_VERIFIED":                   "t",
				"ADMIN_EMAIL":                                      adminEmail,
				"ADMIN_PASSWORD":                                   adminPassword,
				"SERVER_AUTH_CONTROL_PLANE_EXCHANGE_TOKEN_ENABLED": "t",
				"SERVER_AUTH_CONTROL_PLANE_EXCHANGE_TOKEN_JWT_PUBLIC_KEYSET": opts.PublicKeyset,
				"SERVER_AUTH_CONTROL_PLANE_EXCHANGE_TOKEN_ISSUER":            "hatchet-controller",
				"SERVER_AUTH_CONTROL_PLANE_EXCHANGE_TOKEN_AUDIENCE":          "hatchet",
			},
			WaitingFor: wait.ForHTTP("/api/ready").WithPort("8888/tcp").
				WithStartupTimeout(4 * time.Minute),
		},
		Started: true,
	})
	if err != nil {
		return h, fmt.Errorf("starting hatchet-lite %s: %w\n%s", opts.Version, err, logTail(ctx, h.lite))
	}

	host, err := h.lite.Host(ctx)
	if err != nil {
		return h, err
	}
	apiPort, err := h.lite.MappedPort(ctx, "8888/tcp")
	if err != nil {
		return h, err
	}
	grpcPort, err := h.lite.MappedPort(ctx, "7070/tcp")
	if err != nil {
		return h, err
	}
	h.APIURL = fmt.Sprintf("http://%s:%s", host, apiPort.Port())
	h.GRPCHostPort = fmt.Sprintf("%s:%s", host, grpcPort.Port())

	h.AdminUserID, err = h.SQL(ctx, fmt.Sprintf(`SELECT id FROM "User" WHERE email='%s' AND "deletedAt" IS NULL`, adminEmail))
	if err != nil {
		return h, fmt.Errorf("reading admin user id: %w", err)
	}
	if opts.ServiceUserID != "" {
		if _, err = h.SQL(ctx, fmt.Sprintf(
			`INSERT INTO "User" (id, email, "emailVerified") VALUES ('%s', 'hatchet-controller@example.com', true)`,
			opts.ServiceUserID)); err != nil {
			return h, fmt.Errorf("inserting service user: %w", err)
		}
	}
	return h, nil
}

// logTail returns the last lines of the container log for error messages.
func logTail(ctx context.Context, c testcontainers.Container) string {
	if c == nil {
		return ""
	}
	r, err := c.Logs(ctx)
	if err != nil {
		return ""
	}
	b, _ := io.ReadAll(r)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return strings.Join(lines[max(0, len(lines)-30):], "\n")
}

// Terminate removes the containers and the network.
func (h *Hatchet) Terminate(ctx context.Context) error {
	var errs []error
	for _, c := range []testcontainers.Container{h.lite, h.postgres} {
		if c != nil {
			errs = append(errs, c.Terminate(ctx))
		}
	}
	if h.network != nil {
		errs = append(errs, h.network.Remove(ctx))
	}
	return errors.Join(errs...)
}

// SQL runs a query with psql in the postgres container and returns the unaligned output.
func (h *Hatchet) SQL(ctx context.Context, query string) (string, error) {
	code, out, err := h.postgres.Exec(ctx, []string{"psql", "-U", "hatchet", "-d", "hatchet", "-v", "ON_ERROR_STOP=1", "-tAc", query}, tcexec.Multiplexed())
	if err != nil {
		return "", err
	}
	b, err := io.ReadAll(out)
	if err != nil {
		return "", err
	}
	s := string(b)
	if code != 0 {
		return "", fmt.Errorf("psql exit %d: %s", code, s)
	}
	return strings.TrimSpace(s), nil
}

// AddMember adds a user to a tenant, as a Hatchet admin would.
func (h *Hatchet) AddMember(ctx context.Context, tenantID, userID string) error {
	_, err := h.SQL(ctx, fmt.Sprintf(
		`INSERT INTO "TenantMember" (id, "tenantId", "userId", role) VALUES (gen_random_uuid(), '%s', '%s', 'ADMIN')`,
		tenantID, userID))
	return err
}

// AcceptInvite registers a user with the invited email, whose address Hatchet marks verified, and accepts the invite as that user.
func (h *Hatchet) AcceptInvite(ctx context.Context, email, inviteID string) error {
	resp, err := h.post(ctx, "/api/v1/users/register", nil, map[string]string{"name": "Invitee", "email": email, "password": adminPassword})
	if err != nil {
		return fmt.Errorf("register: %w", err)
	}
	if _, err := h.post(ctx, "/api/v1/users/invites/accept", resp.Cookies(), map[string]string{"invite": inviteID}); err != nil {
		return fmt.Errorf("accepting invite: %w", err)
	}
	return nil
}

// CreateTenantAsAdmin logs in the seeded admin with a cookie and creates a tenant.
func (h *Hatchet) CreateTenantAsAdmin(ctx context.Context, name, slug string) (string, error) {
	resp, err := h.post(ctx, "/api/v1/users/login", nil, map[string]string{"email": h.AdminEmail, "password": h.AdminPassword})
	if err != nil {
		return "", fmt.Errorf("login: %w", err)
	}
	cookies := resp.Cookies()
	resp, err = h.post(ctx, "/api/v1/tenants", cookies, map[string]string{"name": name, "slug": slug})
	if err != nil {
		return "", fmt.Errorf("creating tenant: %w", err)
	}
	var tenant struct {
		Metadata struct {
			ID string `json:"id"`
		} `json:"metadata"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tenant); err != nil {
		return "", err
	}
	return tenant.Metadata.ID, nil
}

func (h *Hatchet) post(ctx context.Context, path string, cookies []*http.Cookie, body any) (*http.Response, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.APIURL+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("POST %s: %d %s", path, resp.StatusCode, msg)
	}
	data, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(data))
	return resp, err
}

// APITokenStatus calls a tenant-scoped REST endpoint with an API token and returns the status code.
func (h *Hatchet) APITokenStatus(ctx context.Context, tenantID, token string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.APIURL+"/api/v1/tenants/"+tenantID+"/workflows", nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}
