package tlscert

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"slices"
	"time"

	"golang.org/x/crypto/acme"
)

// account is an ACME account, one per directory.
type account struct {
	KeyPEM string `json:"key"`
}

func (s *Service) directory(set Settings) string {
	switch {
	case s.opts.Directory != "":
		return s.opts.Directory
	case set.Staging:
		return LetsEncryptStaging
	}
	return LetsEncrypt
}

// client returns an ACME client with this directory's account key, making
// and saving one the first time.
func (s *Service) client(dir string) (*acme.Client, error) {
	accounts := map[string]account{}
	if _, err := s.opts.Secrets.Get(secretAccounts, &accounts); err != nil {
		return nil, err
	}
	var key crypto.Signer
	if a, ok := accounts[dir]; ok {
		block, _ := pem.Decode([]byte(a.KeyPEM))
		if block == nil {
			return nil, errors.New("the saved ACME account key is damaged")
		}
		k, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		key = k
	} else {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		der, err := x509.MarshalECPrivateKey(k)
		if err != nil {
			return nil, err
		}
		accounts[dir] = account{KeyPEM: string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))}
		if err := s.opts.Secrets.Set(secretAccounts, accounts); err != nil {
			return nil, err
		}
		key = k
	}
	return &acme.Client{Key: key, DirectoryURL: dir, HTTPClient: s.opts.HTTPClient, UserAgent: "hlm"}, nil
}

// obtain gets a certificate for set.Domain, proving control of it with a TXT
// record at _acme-challenge.<domain>.
func (s *Service) obtain(ctx context.Context, set Settings) (stored, error) {
	dir := s.directory(set)
	c, err := s.client(dir)
	if err != nil {
		return stored{}, err
	}
	acct := &acme.Account{}
	if set.Email != "" {
		acct.Contact = []string{"mailto:" + set.Email}
	}
	if _, err := c.Register(ctx, acct, acme.AcceptTOS); err != nil && !errors.Is(err, acme.ErrAccountAlreadyExists) {
		return stored{}, fmt.Errorf("register with the CA: %w", err)
	}
	order, err := c.AuthorizeOrder(ctx, acme.DomainIDs(set.Domain))
	if err != nil {
		return stored{}, fmt.Errorf("order a certificate: %w", err)
	}
	for _, u := range order.AuthzURLs {
		if err := s.authorize(ctx, c, u); err != nil {
			return stored{}, err
		}
	}
	finalize := order.FinalizeURL
	if order, err = c.WaitOrder(ctx, order.URI); err != nil {
		return stored{}, fmt.Errorf("wait for the order: %w", err)
	}
	// Some CAs don't repeat the finalize URL when the order is read again.
	if order.FinalizeURL != "" {
		finalize = order.FinalizeURL
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return stored{}, err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: []string{set.Domain}}, key)
	if err != nil {
		return stored{}, err
	}
	ders, _, err := c.CreateOrderCert(ctx, finalize, csr, true)
	if err != nil {
		// CreateOrderCert follows the finalize answer's Location header, which
		// RFC 8555 doesn't require and some CAs (like Pebble) leave out. The
		// order is still the one we have, so wait for it and fetch the
		// certificate from there.
		done, werr := c.WaitOrder(ctx, order.URI)
		if werr != nil || done.Status != acme.StatusValid || done.CertURL == "" {
			return stored{}, fmt.Errorf("get the certificate: %w", err)
		}
		if ders, err = c.FetchCert(ctx, done.CertURL, true); err != nil {
			return stored{}, fmt.Errorf("download the certificate: %w", err)
		}
	}
	var chain bytes.Buffer
	for _, der := range ders {
		pem.Encode(&chain, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return stored{}, err
	}
	cert, err := leaf(chain.Bytes())
	if err != nil {
		return stored{}, err
	}
	if err := cert.VerifyHostname(set.Domain); err != nil {
		return stored{}, err
	}
	return stored{
		Domain: set.Domain, CertPEM: chain.String(),
		KeyPEM:   string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})),
		NotAfter: cert.NotAfter, NotBefore: cert.NotBefore, Issued: s.opts.Now().UTC(), Issuer: issuerName(cert), Staging: set.Staging,
	}, nil
}

// authorize answers one authorization's DNS-01 challenge.
func (s *Service) authorize(ctx context.Context, c *acme.Client, url string) error {
	z, err := c.GetAuthorization(ctx, url)
	if err != nil {
		return fmt.Errorf("read the authorization: %w", err)
	}
	if z.Status == acme.StatusValid {
		return nil
	}
	i := slices.IndexFunc(z.Challenges, func(ch *acme.Challenge) bool { return ch.Type == "dns-01" })
	if i < 0 {
		return errors.New("the CA didn't offer a DNS challenge")
	}
	ch := z.Challenges[i]
	value, err := c.DNS01ChallengeRecord(ch.Token)
	if err != nil {
		return err
	}
	name := "_acme-challenge." + z.Identifier.Value
	remove, err := s.opts.DNS.SetTXT(ctx, name, value)
	if err != nil {
		return fmt.Errorf("make the DNS record %s: %w", name, err)
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := remove(cctx); err != nil {
			s.opts.Log.Warn("remove the ACME DNS record", "name", name, "err", err)
		}
	}()
	if err := s.waitForTXT(ctx, name, value); err != nil {
		return err
	}
	if _, err := c.Accept(ctx, ch); err != nil {
		return fmt.Errorf("answer the challenge: %w", err)
	}
	if _, err := c.WaitAuthorization(ctx, z.URI); err != nil {
		return fmt.Errorf("the CA couldn't check the DNS record: %w", err)
	}
	return nil
}

// waitForTXT waits until the resolver sees the record, so the CA is likely
// to as well.
func (s *Service) waitForTXT(ctx context.Context, name, value string) error {
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, s.opts.Resolver)
	}}
	deadline := time.Now().Add(3 * time.Minute)
	for {
		qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		txt, _ := r.LookupTXT(qctx, name)
		cancel()
		if slices.Contains(txt, value) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the DNS record %s didn't show up at %s within 3 minutes", name, s.opts.Resolver)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}
