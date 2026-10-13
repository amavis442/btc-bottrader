package coinbase

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Signer maakt per verzoek een JWT voor de Coinbase Advanced Trade API (CDP API-key, ES256).
//
// Opbouw volgens de documentatie van Coinbase:
//   - header: alg ES256, typ JWT, kid = key name, nonce = willekeurig
//   - claims: iss "cdp", sub = key name, nbf = nu, exp = nu + 120 s,
//     uri = "<METHODE> <host><pad>" (zonder query-string)
type Signer struct {
	keyName string
	key     *ecdsa.PrivateKey
	now     func() time.Time
}

// NewSigner leest de key name ("organizations/{org_id}/apiKeys/{key_id}") en de private key (PEM).
// Een PEM die op één regel staat met "\n" erin (zoals in JSON of een omgevingsvariabele) wordt ook
// geaccepteerd.
func NewSigner(keyName, privateKeyPEM string) (*Signer, error) {
	if !strings.HasPrefix(keyName, "organizations/") || !strings.Contains(keyName, "/apiKeys/") {
		return nil, errors.New("coinbase: key name moet de vorm organizations/{org_id}/apiKeys/{key_id} hebben")
	}
	key, err := parsePrivateKey(strings.ReplaceAll(strings.TrimSpace(privateKeyPEM), `\n`, "\n"))
	if err != nil {
		return nil, err
	}
	return &Signer{keyName: keyName, key: key, now: time.Now}, nil
}

func parsePrivateKey(data string) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(data))
	if block == nil {
		return nil, errors.New("coinbase: private key is geen PEM; maak de key aan met ECDSA (Ed25519 wordt niet ondersteund)")
	}
	var key *ecdsa.PrivateKey
	switch block.Type {
	case "EC PRIVATE KEY":
		k, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("coinbase: private key ongeldig: %w", err)
		}
		key = k
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("coinbase: private key ongeldig: %w", err)
		}
		ec, ok := k.(*ecdsa.PrivateKey)
		if !ok {
			return nil, errors.New("coinbase: private key is geen ECDSA-key; maak de key aan met ECDSA")
		}
		key = ec
	default:
		return nil, fmt.Errorf("coinbase: onverwacht PEM-type %q", block.Type)
	}
	if key.Curve != elliptic.P256() {
		return nil, errors.New("coinbase: private key moet een P-256-key zijn (ES256)")
	}
	return key, nil
}

// Token maakt een JWT voor één verzoek. host is bijv. "api.coinbase.com", path het volledige pad
// zonder query-string ("/api/v3/brokerage/accounts").
func (s *Signer) Token(method, host, path string) (string, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	now := s.now().Unix()
	header := map[string]string{"alg": "ES256", "typ": "JWT", "kid": s.keyName, "nonce": hex.EncodeToString(nonce)}
	claims := map[string]any{
		"iss": "cdp", "sub": s.keyName, "nbf": now, "exp": now + 120,
		"uri": method + " " + host + path,
	}

	h, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	c, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	signingInput := enc.EncodeToString(h) + "." + enc.EncodeToString(c)

	digest := sha256.Sum256([]byte(signingInput))
	r, sig, err := ecdsa.Sign(rand.Reader, s.key, digest[:])
	if err != nil {
		return "", err
	}
	// JWS ES256: R en S elk als 32 bytes achter elkaar (geen ASN.1).
	raw := make([]byte, 64)
	r.FillBytes(raw[:32])
	sig.FillBytes(raw[32:])
	return signingInput + "." + enc.EncodeToString(raw), nil
}
