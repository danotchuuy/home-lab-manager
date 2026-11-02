package auth

// Client secrets made by hlm, so setting up a provider doesn't need its
// command line to make one.

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"fmt"
	"strings"
)

// Authelia's own defaults for a client secret's digest
// (authelia crypto hash generate pbkdf2 --variant sha512).
const (
	autheliaIterations = 310000
	autheliaSaltLen    = 16
	autheliaKeyLen     = 64
)

// ClientSecret is a new client secret, with its digest for Authelia's
// configuration. Other providers take the secret itself.
type ClientSecret struct {
	Secret         string `json:"secret"`
	AutheliaDigest string `json:"autheliaDigest"`
}

// GenerateClientSecret makes a random client secret: 64 URL-safe
// characters (384 bits).
func GenerateClientSecret() (ClientSecret, error) {
	b := make([]byte, 48)
	if _, err := rand.Read(b); err != nil {
		return ClientSecret{}, err
	}
	secret := base64.RawURLEncoding.EncodeToString(b)
	digest, err := autheliaDigest(secret)
	if err != nil {
		return ClientSecret{}, err
	}
	return ClientSecret{Secret: secret, AutheliaDigest: digest}, nil
}

// autheliaDigest hashes a secret the way Authelia stores client secrets:
// $pbkdf2-sha512$<iterations>$<salt>$<key>, with salt and key in passlib's
// "adapted base64" (unpadded, "." for "+").
func autheliaDigest(secret string) (string, error) {
	salt := make([]byte, autheliaSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha512.New, secret, salt, autheliaIterations, autheliaKeyLen)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("$pbkdf2-sha512$%d$%s$%s", autheliaIterations, ab64(salt), ab64(key)), nil
}

func ab64(b []byte) string {
	return strings.ReplaceAll(base64.RawStdEncoding.EncodeToString(b), "+", ".")
}
