package s3

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// The "GET Bucket (List Objects)" example from AWS's Signature Version 4
// documentation for S3.
func TestSignAWSExample(t *testing.T) {
	c, err := New(Settings{
		Endpoint: "https://s3.amazonaws.com", Bucket: "examplebucket", VirtualHost: true,
		AccessKey: "AKIAIOSFODNN7EXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
	})
	if err != nil {
		t.Fatal(err)
	}
	c.now = func() time.Time { return time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC) }
	u := c.objectURL("", url.Values{"max-keys": {"2"}, "prefix": {"J"}})
	req, _ := http.NewRequest(http.MethodGet, u.String(), nil)
	c.sign(req, nil)
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;x-amz-content-sha256;x-amz-date, " +
		"Signature=34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7"
	if got := req.Header.Get("Authorization"); got != want {
		t.Errorf("Authorization =\n%s\nwant\n%s", got, want)
	}
}

func TestObjectURL(t *testing.T) {
	c, _ := New(Settings{Endpoint: "seaweed.lan:8333", Bucket: "games", AccessKey: "a", SecretKey: "b"})
	u := c.objectURL("hlm/my server/a+b.tar.gz", url.Values{"partNumber": {"1"}, "uploadId": {"x y"}})
	if got, want := u.String(), "https://seaweed.lan:8333/games/hlm/my%20server/a%2Bb.tar.gz?partNumber=1&uploadId=x%20y"; got != want {
		t.Errorf("URL = %s, want %s", got, want)
	}
	if _, err := New(Settings{Endpoint: "http://h:8333/games", Bucket: "games", AccessKey: "a", SecretKey: "b"}); err == nil {
		t.Error("an endpoint with a path was accepted")
	}
}

// fakeS3 is a path-style S3 server with just enough of the API for Upload,
// List, Get and Delete.
type fakeS3 struct {
	mu        sync.Mutex
	objects   map[string][]byte
	uploads   map[string]map[int][]byte
	meta      map[string]string
	failPart  int
	aborted   int
	partSizes []int
}

func newFake(t *testing.T) (*fakeS3, *Client) {
	f := &fakeS3{objects: map[string][]byte{}, uploads: map[string]map[int][]byte{}, meta: map[string]string{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := New(Settings{Endpoint: srv.URL, Bucket: "b", AccessKey: "a", SecretKey: "s"})
	if err != nil {
		t.Fatal(err)
	}
	return f, c
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=a/") {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/b/")
	q := r.URL.Query()
	body, _ := io.ReadAll(r.Body)
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/b":
		fmt.Fprint(w, "<ListBucketResult>")
		for k, v := range f.objects {
			if strings.HasPrefix(k, q.Get("prefix")) {
				fmt.Fprintf(w, "<Contents><Key>%s</Key><Size>%d</Size><LastModified>2026-01-02T03:04:05.000Z</LastModified></Contents>", k, len(v))
			}
		}
		fmt.Fprint(w, "<IsTruncated>false</IsTruncated></ListBucketResult>")
	case r.Method == http.MethodPost && q.Has("uploads"):
		f.uploads["u1"] = map[int][]byte{}
		f.meta[key] = r.Header.Get("X-Amz-Meta-Hlm-Server")
		fmt.Fprint(w, "<InitiateMultipartUploadResult><UploadId>u1</UploadId></InitiateMultipartUploadResult>")
	case r.Method == http.MethodPut && q.Has("partNumber"):
		var n int
		fmt.Sscan(q.Get("partNumber"), &n)
		if n == f.failPart {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, "<Error><Code>InternalError</Code><Message>disk full</Message></Error>")
			return
		}
		f.uploads[q.Get("uploadId")][n] = body
		f.partSizes = append(f.partSizes, len(body))
		w.Header().Set("ETag", fmt.Sprintf(`"etag%d"`, n))
	case r.Method == http.MethodPost && q.Has("uploadId"):
		var done struct {
			Parts []completedPart `xml:"Part"`
		}
		xml.Unmarshal(body, &done)
		var all []byte
		for i, p := range done.Parts {
			if p.PartNumber != i+1 || p.ETag != fmt.Sprintf(`"etag%d"`, i+1) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			all = append(all, f.uploads[q.Get("uploadId")][p.PartNumber]...)
		}
		f.objects[key] = all
		fmt.Fprint(w, "<CompleteMultipartUploadResult/>")
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		f.aborted++
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut:
		f.objects[key] = body
	case r.Method == http.MethodGet:
		data, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, "<Error><Code>NoSuchKey</Code></Error>")
			return
		}
		w.Write(data)
	case r.Method == http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotImplemented)
	}
}

func TestUploadInParts(t *testing.T) {
	f, c := newFake(t)
	ctx := context.Background()
	data := bytes.Repeat([]byte("0123456789abcdef"), (PartSize*2+1000)/16)
	size, err := c.Upload(ctx, "hlm/srv/a.tar.gz", bytes.NewReader(data), "application/gzip", map[string]string{"Hlm-Server": "srv"})
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(data)) || !bytes.Equal(f.objects["hlm/srv/a.tar.gz"], data) {
		t.Errorf("stored %d bytes, want %d", len(f.objects["hlm/srv/a.tar.gz"]), len(data))
	}
	if len(f.partSizes) != 3 || f.partSizes[0] != PartSize || f.meta["hlm/srv/a.tar.gz"] != "srv" {
		t.Errorf("parts = %v, meta = %v", f.partSizes, f.meta)
	}

	objs, err := c.List(ctx, "hlm/srv/")
	if err != nil || len(objs) != 1 || objs[0].Size != int64(len(data)) {
		t.Errorf("List = %v, %v", objs, err)
	}
	rc, _, err := c.Get(ctx, "hlm/srv/a.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data) {
		t.Error("Get returned different data")
	}
	if err := c.Delete(ctx, "hlm/srv/a.tar.gz"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Get(ctx, "hlm/srv/a.tar.gz"); !IsNotFound(err) {
		t.Errorf("Get after Delete: %v", err)
	}
}

func TestUploadEmpty(t *testing.T) {
	f, c := newFake(t)
	if _, err := c.Upload(context.Background(), "empty", strings.NewReader(""), "", nil); err != nil {
		t.Fatal(err)
	}
	if data, ok := f.objects["empty"]; !ok || len(data) != 0 {
		t.Errorf("empty object: %q %v", data, ok)
	}
}

func TestUploadFailureAborts(t *testing.T) {
	f, c := newFake(t)
	f.failPart = 2
	data := make([]byte, PartSize+1)
	_, err := c.Upload(context.Background(), "x", bytes.NewReader(data), "", nil)
	var s3Err *Error
	if !errors.As(err, &s3Err) || s3Err.Message != "disk full" {
		t.Errorf("err = %v", err)
	}
	if f.aborted != 1 || len(f.objects) != 0 {
		t.Errorf("aborted = %d, objects = %d", f.aborted, len(f.objects))
	}

	// A reader that fails aborts too, with the reader's error.
	f.failPart = 0
	boom := errors.New("archive failed")
	_, err = c.Upload(context.Background(), "y", io.MultiReader(bytes.NewReader(data), errReader{boom}), "", nil)
	if !errors.Is(err, boom) || f.aborted != 2 {
		t.Errorf("err = %v, aborted = %d", err, f.aborted)
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }
