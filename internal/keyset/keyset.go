// Package keyset generates the Tink ES256 signing keyset and signs exchange JWTs for Hatchet.
package keyset

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/tink-crypto/tink-go/v2/insecurecleartextkeyset"
	"github.com/tink-crypto/tink-go/v2/jwt"
	"github.com/tink-crypto/tink-go/v2/keyset"
)

const (
	// Issuer is the `iss` claim Hatchet expects (SERVER_AUTH_CONTROL_PLANE_EXCHANGE_TOKEN_ISSUER).
	Issuer = "hatchet-controller"
	// Audience is the `aud` claim Hatchet expects (SERVER_AUTH_CONTROL_PLANE_EXCHANGE_TOKEN_AUDIENCE).
	Audience = "hatchet"

	// PrivateKey is the data key of the signing key Secret (Tink cleartext JSON keyset).
	PrivateKey = "keyset.json"
	// PublicKey is the data key of the public keyset Secret (base64 RawStd single-line JSON).
	PublicKey = "keyset"
)

// Generate returns a new private Tink ES256 JWT keyset as cleartext JSON.
func Generate() ([]byte, error) {
	h, err := keyset.NewHandle(jwt.ES256Template())
	if err != nil {
		return nil, fmt.Errorf("generating keyset: %w", err)
	}
	return writeJSON(h)
}

// PublicKeysetBase64 derives the public keyset from a private keyset and encodes it
// as base64 RawStd of the single-line Tink JSON, as Hatchet reads it from its env.
func PublicKeysetBase64(private []byte) (string, error) {
	h, err := readJSON(private)
	if err != nil {
		return "", err
	}
	pub, err := h.Public()
	if err != nil {
		return "", fmt.Errorf("deriving public keyset: %w", err)
	}
	b, err := writeJSON(pub)
	if err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(b), nil
}

// Signer mints short-lived exchange JWTs.
type Signer struct {
	issuer   string
	audience string
	signer   jwt.Signer
}

// NewSigner parses a private keyset and returns a Signer for the given issuer and audience.
func NewSigner(private []byte, issuer, audience string) (*Signer, error) {
	h, err := readJSON(private)
	if err != nil {
		return nil, err
	}
	s, err := jwt.NewSigner(h)
	if err != nil {
		return nil, fmt.Errorf("creating JWT signer: %w", err)
	}
	return &Signer{issuer: issuer, audience: audience, signer: s}, nil
}

// Sign returns a JWT with sub=subject, tenant_id=tenantID, iat=now-30s and exp=now+5m.
func (s *Signer) Sign(subject, tenantID string, now time.Time) (string, error) {
	iat := now.Add(-30 * time.Second)
	exp := now.Add(5 * time.Minute)
	raw, err := jwt.NewRawJWT(&jwt.RawJWTOptions{
		Issuer:       &s.issuer,
		Audience:     &s.audience,
		Subject:      &subject,
		IssuedAt:     &iat,
		ExpiresAt:    &exp,
		CustomClaims: map[string]any{"tenant_id": tenantID},
	})
	if err != nil {
		return "", fmt.Errorf("building JWT: %w", err)
	}
	return s.signer.SignAndEncode(raw)
}

func readJSON(b []byte) (*keyset.Handle, error) {
	h, err := insecurecleartextkeyset.Read(keyset.NewJSONReader(bytes.NewReader(b)))
	if err != nil {
		return nil, fmt.Errorf("reading keyset: %w", err)
	}
	return h, nil
}

func writeJSON(h *keyset.Handle) ([]byte, error) {
	var buf bytes.Buffer
	if err := insecurecleartextkeyset.Write(h, keyset.NewJSONWriter(&buf)); err != nil {
		return nil, fmt.Errorf("writing keyset: %w", err)
	}
	return buf.Bytes(), nil
}
