// JWT/JWKS verification helpers (stdlib only). The control-plane api issues
// RS256 JWTs and publishes the public key at /.well-known/jwks.json. These
// helpers let a test fetch JWKS, build the RSA public key from the JWK n/e
// params, and verify a JWT signature — without pulling in a JWT dependency.
package kindtest

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
)

// JWKS is a minimal JSON Web Key Set.
type JWKS struct {
	Keys []JWK `json:"keys"`
}

// JWK is a single RSA public key in JWKS form.
type JWK struct {
	Kty string `json:"kty"`
	Use string `json:"use,omitempty"`
	Kid string `json:"kid"`
	Alg string `json:"alg,omitempty"`
	N   string `json:"n"` // modulus, base64url
	E   string `json:"e"` // exponent, base64url
}

// ParseJWKS parses a JWKS document.
func ParseJWKS(b []byte) (*JWKS, error) {
	var j JWKS
	if err := json.Unmarshal(b, &j); err != nil {
		return nil, fmt.Errorf("parse jwks: %w", err)
	}
	return &j, nil
}

// PublicKey returns the RSA public key for the given kid. If kid is empty, the
// first RSA key is returned.
func (j *JWKS) PublicKey(kid string) (*rsa.PublicKey, error) {
	for _, k := range j.Keys {
		if k.Kty != "RSA" {
			continue
		}
		if kid == "" || k.Kid == kid {
			return k.rsaPublicKey()
		}
	}
	return nil, fmt.Errorf("jwks: no rsa key for kid %q", kid)
}

func (k JWK) rsaPublicKey() (*rsa.PublicKey, error) {
	n, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		return nil, fmt.Errorf("decode n: %w", err)
	}
	e, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil, fmt.Errorf("decode e: %w", err)
	}
	return &rsa.PublicKey{
		N: new(big.Int).SetBytes(n),
		E: int(new(big.Int).SetBytes(e).Int64()),
	}, nil
}

// VerifyRS256 verifies an RS256 JWT's signature against pub and returns the
// payload (claims) as raw JSON. Only the signature is verified — callers must
// check exp/iss/aud themselves.
func VerifyRS256(token string, pub *rsa.PublicKey) ([]byte, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("jwt: expected 3 parts, got %d", len(parts))
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("decode signature: %w", err)
	}
	signed := parts[0] + "." + parts[1]
	hash := sha256.Sum256([]byte(signed))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, hash[:], sig); err != nil {
		return nil, fmt.Errorf("verify signature: %w", err)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}
	return payload, nil
}
