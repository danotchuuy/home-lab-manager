// Package s3 is a small client for S3-compatible object storage (AWS S3,
// SeaweedFS, MinIO, Garage, ...), using only the standard library.
//
// Requests are signed with AWS Signature Version 4. Buckets are addressed
// path-style (http://host:8333/bucket/key) unless virtual-hosted style is
// asked for, since self-hosted servers such as SeaweedFS expect path-style.
package s3

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Settings say where and how to reach a bucket.
type Settings struct {
	// Endpoint is the server's base URL, e.g. http://seaweed.lan:8333.
	Endpoint  string `json:"endpoint"`
	Region    string `json:"region,omitempty"` // us-east-1 if empty
	Bucket    string `json:"bucket"`
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey"`
	// VirtualHost addresses the bucket as bucket.host instead of host/bucket.
	VirtualHost bool `json:"virtualHost,omitempty"`
	// CACert is PEM certificates to trust for an https:// endpoint, on top of
	// the system's, e.g. a private CA that signed the server's certificate.
	CACert string `json:"caCert,omitempty"`
}

// CertInfo describes a certificate in Settings.CACert.
type CertInfo struct {
	Subject  string    `json:"subject"`
	Issuer   string    `json:"issuer"`
	NotAfter time.Time `json:"notAfter"`
	IsCA     bool      `json:"isCA"`
}

// ParseCACert reads PEM certificates. It fails if there are none, or if
// anything else is in the text (such as a private key, which must never be
// stored here).
func ParseCACert(text string) ([]*x509.Certificate, error) {
	rest := []byte(strings.TrimSpace(text))
	var certs []*x509.Certificate
	for len(bytes.TrimSpace(rest)) > 0 {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return nil, errors.New("the CA certificate isn't valid PEM (it should start with -----BEGIN CERTIFICATE-----)")
		}
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("the CA certificate field holds a %s, not a certificate; only paste certificates there", strings.ToLower(block.Type))
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("read the CA certificate: %w", err)
		}
		certs = append(certs, cert)
	}
	if len(certs) == 0 {
		return nil, errors.New("no certificate found in the CA certificate field")
	}
	return certs, nil
}

// DescribeCACert summarizes the certificates in PEM text, for showing which
// CA is trusted. It returns nil for text that doesn't parse.
func DescribeCACert(text string) []CertInfo {
	certs, err := ParseCACert(text)
	if err != nil {
		return nil
	}
	out := make([]CertInfo, 0, len(certs))
	for _, c := range certs {
		out = append(out, CertInfo{Subject: c.Subject.String(), Issuer: c.Issuer.String(), NotAfter: c.NotAfter, IsCA: c.IsCA})
	}
	return out
}

// CertError is a TLS failure caused by the server's certificate, such as one
// signed by a CA that isn't trusted.
type CertError struct {
	Err error
}

func (e *CertError) Error() string {
	var unknown x509.UnknownAuthorityError
	var host x509.HostnameError
	switch {
	case errors.As(e.Err, &unknown):
		return "the S3 server's certificate is signed by a CA that isn't trusted; add that CA's certificate (or check that the right one is set)"
	case errors.As(e.Err, &host):
		return "the S3 server's certificate doesn't match the endpoint's address: " + host.Error()
	}
	return "the S3 server's certificate was rejected: " + e.Err.Error()
}

func (e *CertError) Unwrap() error { return e.Err }

func certErr(err error) error {
	var unknown x509.UnknownAuthorityError
	var host x509.HostnameError
	var invalid x509.CertificateInvalidError
	var verify *tls.CertificateVerificationError
	if errors.As(err, &unknown) || errors.As(err, &host) || errors.As(err, &invalid) || errors.As(err, &verify) {
		return &CertError{Err: err}
	}
	return err
}

// Client talks to one bucket.
type Client struct {
	s    Settings
	base *url.URL
	http *http.Client
	now  func() time.Time
}

// New checks the settings and returns a client for them.
func New(s Settings) (*Client, error) {
	s.Endpoint = strings.TrimRight(strings.TrimSpace(s.Endpoint), "/")
	if s.Endpoint == "" {
		return nil, errors.New("an endpoint URL is required")
	}
	if !strings.Contains(s.Endpoint, "://") {
		s.Endpoint = "https://" + s.Endpoint
	}
	u, err := url.Parse(s.Endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("invalid endpoint URL %q", s.Endpoint)
	}
	if u.Path != "" && u.Path != "/" {
		return nil, errors.New("the endpoint URL should be just the server's address, without a path; put the bucket name in Bucket")
	}
	u.Path = ""
	if s.Bucket = strings.TrimSpace(s.Bucket); s.Bucket == "" || strings.ContainsAny(s.Bucket, "/ ") {
		return nil, errors.New("a bucket name (without slashes) is required")
	}
	if s.AccessKey == "" || s.SecretKey == "" {
		return nil, errors.New("an access key and secret key are required")
	}
	if s.Region == "" {
		s.Region = "us-east-1"
	}
	hc := &http.Client{}
	if strings.TrimSpace(s.CACert) != "" {
		certs, err := ParseCACert(s.CACert)
		if err != nil {
			return nil, err
		}
		if u.Scheme != "https" {
			return nil, errors.New("a CA certificate only applies to https:// endpoints")
		}
		// The system's CAs stay trusted, so a public certificate still works.
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		for _, c := range certs {
			pool.AddCert(c)
		}
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
		hc.Transport = tr
	}
	return &Client{s: s, base: u, http: hc, now: time.Now}, nil
}

// Error is an error response from the server.
type Error struct {
	Status  int
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

func (e *Error) Error() string {
	switch {
	case e.Code != "" && e.Message != "":
		return fmt.Sprintf("S3: %s: %s", e.Code, e.Message)
	case e.Code != "":
		return "S3: " + e.Code
	}
	return fmt.Sprintf("S3: HTTP %d", e.Status)
}

// IsNotFound reports whether err is a missing object or bucket.
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

// objectURL returns the URL for key ("" for the bucket itself) with an
// escaped path that matches what gets signed.
func (c *Client) objectURL(key string, q url.Values) *url.URL {
	u := *c.base
	p := "/" + key
	switch {
	case c.s.VirtualHost:
		u.Host = c.s.Bucket + "." + u.Host
	case key == "":
		p = "/" + c.s.Bucket // the bucket itself
	default:
		p = "/" + c.s.Bucket + p
	}
	u.Path = p
	u.RawPath = escapePath(p)
	u.RawQuery = canonicalQuery(q)
	return &u
}

// do sends a signed request. body may be nil; its SHA-256 is signed, so it
// is kept in memory (callers send at most one multipart part at a time).
func (c *Client) do(ctx context.Context, method, key string, q url.Values, header http.Header, body []byte) (*http.Response, error) {
	u := c.objectURL(key, q)
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.ContentLength = int64(len(body))
	if body == nil {
		req.Body = http.NoBody
	}
	for k, v := range header {
		req.Header[k] = v
	}
	c.sign(req, body)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, certErr(err)
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		e := &Error{Status: resp.StatusCode}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = xml.Unmarshal(data, e)
		return nil, e
	}
	return resp, nil
}

func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, s string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(s))
	return h.Sum(nil)
}

// sign adds an AWS Signature Version 4 Authorization header.
func (c *Client) sign(req *http.Request, body []byte) {
	t := c.now().UTC()
	amzDate := t.Format("20060102T150405Z")
	day := t.Format("20060102")
	payload := hashHex(body)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payload)

	// Sign the host and every x-amz-* and content-* header.
	headers := map[string]string{"host": req.URL.Host}
	for k, v := range req.Header {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-amz-") || strings.HasPrefix(lk, "content-") {
			headers[lk] = strings.TrimSpace(strings.Join(v, ","))
		}
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + headers[k] + "\n")
	}
	signed := strings.Join(names, ";")

	canonical := strings.Join([]string{
		req.Method,
		req.URL.EscapedPath(),
		req.URL.RawQuery,
		canonHeaders.String(),
		signed,
		payload,
	}, "\n")
	scope := day + "/" + c.s.Region + "/s3/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hashHex([]byte(canonical))

	key := hmacSHA256([]byte("AWS4"+c.s.SecretKey), day)
	key = hmacSHA256(key, c.s.Region)
	key = hmacSHA256(key, "s3")
	key = hmacSHA256(key, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(key, toSign))
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		c.s.AccessKey, scope, signed, sig))
}

// escape percent-encodes s as SigV4 requires: everything but unreserved
// characters, and '/' too unless keepSlash.
func escape(s string, keepSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' ||
			ch == '-' || ch == '_' || ch == '.' || ch == '~' || ch == '/' && keepSlash {
			b.WriteByte(ch)
		} else {
			fmt.Fprintf(&b, "%%%02X", ch)
		}
	}
	return b.String()
}

func escapePath(p string) string { return escape(p, true) }

func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vals := append([]string(nil), q[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			parts = append(parts, escape(k, false)+"="+escape(v, false))
		}
	}
	return strings.Join(parts, "&")
}

// Object is an entry in a bucket listing.
type Object struct {
	Key          string    `json:"key"`
	Size         int64     `json:"size"`
	LastModified time.Time `json:"lastModified"`
}

// List returns every object whose key starts with prefix, sorted by key.
func (c *Client) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := c.do(ctx, http.MethodGet, "", q, nil, nil)
		if err != nil {
			return nil, err
		}
		var res struct {
			Contents []struct {
				Key          string    `xml:"Key"`
				Size         int64     `xml:"Size"`
				LastModified time.Time `xml:"LastModified"`
			} `xml:"Contents"`
			IsTruncated           bool   `xml:"IsTruncated"`
			NextContinuationToken string `xml:"NextContinuationToken"`
		}
		err = xml.NewDecoder(resp.Body).Decode(&res)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read bucket listing: %w", err)
		}
		for _, o := range res.Contents {
			out = append(out, Object{Key: o.Key, Size: o.Size, LastModified: o.LastModified})
		}
		if !res.IsTruncated || res.NextContinuationToken == "" {
			break
		}
		token = res.NextContinuationToken
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// Put uploads a small object in one request.
func (c *Client) Put(ctx context.Context, key string, data []byte, contentType string) error {
	h := http.Header{}
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	sum := md5.Sum(data)
	h.Set("Content-Md5", base64.StdEncoding.EncodeToString(sum[:]))
	resp, err := c.do(ctx, http.MethodPut, key, nil, h, data)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// Get opens an object for reading. The caller must close it.
func (c *Client) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	resp, err := c.do(ctx, http.MethodGet, key, nil, nil, nil)
	if err != nil {
		return nil, 0, err
	}
	return resp.Body, resp.ContentLength, nil
}

// Delete removes an object. Deleting a missing object is not an error.
func (c *Client) Delete(ctx context.Context, key string) error {
	resp, err := c.do(ctx, http.MethodDelete, key, nil, nil, nil)
	if IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// PartSize is the size of each part of a multipart upload. With S3's limit
// of 10,000 parts, objects can be up to about 160 GB.
const PartSize = 16 << 20

const maxParts = 10_000

// partTimeout bounds each request of an upload, so a stalled connection
// fails the upload instead of hanging it.
const partTimeout = 15 * time.Minute

// Upload stores everything read from r under key, in parts of PartSize, so
// objects of unknown size can be streamed. meta is stored as x-amz-meta-*
// headers. An upload that fails is aborted, so no parts are left behind.
// It returns the object's size.
func (c *Client) Upload(ctx context.Context, key string, r io.Reader, contentType string, meta map[string]string) (int64, error) {
	h := http.Header{}
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	for k, v := range meta {
		h.Set("X-Amz-Meta-"+k, v)
	}
	rctx, cancel := context.WithTimeout(ctx, partTimeout)
	resp, err := c.do(rctx, http.MethodPost, key, url.Values{"uploads": {""}}, h, nil)
	if err != nil {
		cancel()
		return 0, err
	}
	var created struct {
		UploadID string `xml:"UploadId"`
	}
	err = xml.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	cancel()
	if err != nil || created.UploadID == "" {
		return 0, fmt.Errorf("start upload: no upload ID in the response (%v)", err)
	}

	size, err := c.uploadParts(ctx, key, created.UploadID, r)
	if err != nil {
		// Use a fresh context: ctx may be the reason it failed.
		actx, acancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer acancel()
		if resp, aerr := c.do(actx, http.MethodDelete, key, url.Values{"uploadId": {created.UploadID}}, nil, nil); aerr == nil {
			resp.Body.Close()
		}
		return 0, err
	}
	return size, nil
}

type completedPart struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

func (c *Client) uploadParts(ctx context.Context, key, uploadID string, r io.Reader) (int64, error) {
	var (
		parts []completedPart
		size  int64
		buf   = make([]byte, PartSize)
	)
	for n := 1; ; n++ {
		read, rerr := io.ReadFull(r, buf)
		if rerr != nil && rerr != io.EOF && rerr != io.ErrUnexpectedEOF {
			return 0, rerr
		}
		// An empty object still needs one (empty) part.
		if read > 0 || n == 1 {
			if n > maxParts {
				return 0, fmt.Errorf("the backup is larger than %d GB, the most one upload can hold", int64(PartSize)*maxParts>>30)
			}
			etag, err := c.uploadPart(ctx, key, uploadID, n, buf[:read])
			if err != nil {
				return 0, fmt.Errorf("upload part %d: %w", n, err)
			}
			parts = append(parts, completedPart{PartNumber: n, ETag: etag})
			size += int64(read)
		}
		if rerr != nil {
			break
		}
	}

	body, err := xml.Marshal(struct {
		XMLName xml.Name        `xml:"CompleteMultipartUpload"`
		Parts   []completedPart `xml:"Part"`
	}{Parts: parts})
	if err != nil {
		return 0, err
	}
	rctx, cancel := context.WithTimeout(ctx, partTimeout)
	defer cancel()
	resp, err := c.do(rctx, http.MethodPost, key, url.Values{"uploadId": {uploadID}}, http.Header{"Content-Type": {"application/xml"}}, body)
	if err != nil {
		return 0, fmt.Errorf("finish upload: %w", err)
	}
	defer resp.Body.Close()
	// S3 can report a failure with status 200 once it has started replying.
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if bytes.Contains(data, []byte("<Error>")) {
		e := &Error{Status: resp.StatusCode}
		_ = xml.Unmarshal(data, e)
		return 0, fmt.Errorf("finish upload: %w", e)
	}
	return size, nil
}

func (c *Client) uploadPart(ctx context.Context, key, uploadID string, n int, data []byte) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, partTimeout)
	defer cancel()
	q := url.Values{"partNumber": {strconv.Itoa(n)}, "uploadId": {uploadID}}
	resp, err := c.do(ctx, http.MethodPut, key, q, nil, data)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	etag := resp.Header.Get("ETag")
	if etag == "" {
		return "", errors.New("the server didn't return an ETag")
	}
	return etag, nil
}
