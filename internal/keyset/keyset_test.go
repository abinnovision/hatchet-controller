package keyset

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/tink-crypto/tink-go/v2/insecurecleartextkeyset"
	"github.com/tink-crypto/tink-go/v2/jwt"
	"github.com/tink-crypto/tink-go/v2/keyset"
)

// hatchetVerify mirrors Hatchet's InsecureHandleFromBytes and ValidateExchangeToken.
func hatchetVerify(t *testing.T, publicB64, token string) (*jwt.VerifiedJWT, error) {
	t.Helper()
	jsonBytes := make([]byte, base64.RawStdEncoding.DecodedLen(len(publicB64)))
	if _, err := base64.RawStdEncoding.Decode(jsonBytes, []byte(publicB64)); err != nil {
		t.Fatalf("decoding public keyset: %v", err)
	}
	h, err := insecurecleartextkeyset.Read(keyset.NewJSONReader(bytes.NewReader(jsonBytes)))
	if err != nil {
		t.Fatalf("reading public keyset: %v", err)
	}
	verifier, err := jwt.NewVerifier(h)
	if err != nil {
		t.Fatalf("creating verifier: %v", err)
	}
	issuer, audience := Issuer, Audience
	validator, err := jwt.NewValidator(&jwt.ValidatorOpts{
		ExpectedAudience:      &audience,
		ExpectedIssuer:        &issuer,
		FixedNow:              time.Now(),
		ExpectIssuedInThePast: true,
	})
	if err != nil {
		t.Fatalf("creating validator: %v", err)
	}
	return verifier.VerifyAndDecode(token, validator)
}

func mustGenerate(t *testing.T) []byte {
	t.Helper()
	private, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return private
}

func TestSignVerifiesLikeHatchet(t *testing.T) {
	private := mustGenerate(t)
	public, err := PublicKeysetBase64(private)
	if err != nil {
		t.Fatalf("PublicKeysetBase64: %v", err)
	}
	if strings.ContainsAny(public, "\n=") {
		t.Fatalf("public keyset must be single-line RawStd base64: %q", public)
	}

	signer, err := NewSigner(private, Issuer, Audience)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	now := time.Now()
	token, err := signer.Sign("user-1", "tenant-1", now)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	v, err := hatchetVerify(t, public, token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if sub, _ := v.Subject(); sub != "user-1" {
		t.Errorf("sub = %q", sub)
	}
	if tid, _ := v.StringClaim("tenant_id"); tid != "tenant-1" {
		t.Errorf("tenant_id = %q", tid)
	}
	if iat, _ := v.IssuedAt(); !iat.Equal(now.Add(-30 * time.Second).Truncate(time.Second)) {
		t.Errorf("iat = %v", iat)
	}
	if exp, _ := v.ExpiresAt(); !exp.Equal(now.Add(5 * time.Minute).Truncate(time.Second)) {
		t.Errorf("exp = %v", exp)
	}
}

func TestForeignKeysetFails(t *testing.T) {
	foreign, err := PublicKeysetBase64(mustGenerate(t))
	if err != nil {
		t.Fatalf("PublicKeysetBase64: %v", err)
	}
	signer, err := NewSigner(mustGenerate(t), Issuer, Audience)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	token, err := signer.Sign("user-1", "tenant-1", time.Now())
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := hatchetVerify(t, foreign, token); err == nil {
		t.Fatal("expected verification with a foreign keyset to fail")
	}
}
