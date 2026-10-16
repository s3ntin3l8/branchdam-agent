package branchdam

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const uploadOKBody = `{"nodeUuid":"n1","status":"UPLOADED","bytesWritten":3,"blake3Hash":"","relativePath":"2026/a.jpg"}`

// TestUploadIgnoresJSONClientTimeout: the JSON calls' client carries a
// total timeout; uploads must not, or every file that takes longer than
// it to send and process fails. Here the JSON client allows 200ms and
// the server takes 600ms to answer.
func TestUploadIgnoresJSONClientTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		time.Sleep(600 * time.Millisecond)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(uploadOKBody))
	}))
	defer srv.Close()

	c := New(srv.URL, "k", WithHTTPClient(&http.Client{Timeout: 200 * time.Millisecond}))
	if _, err := c.Upload(context.Background(), strings.NewReader("abc"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
}

// TestUploadClientHasNoTotalTimeout pins the default upload client's
// shape: no total Timeout, a TLS 1.2 floor, and per-phase deadlines.
func TestUploadClientHasNoTotalTimeout(t *testing.T) {
	c := New("https://branchdam.example.com", "k")
	if c.uploadClient == nil || c.uploadClient == c.httpClient {
		t.Fatal("Upload must use its own client")
	}
	if c.uploadClient.Timeout != 0 {
		t.Errorf("upload client Timeout = %s, want 0 (none)", c.uploadClient.Timeout)
	}
	tr, ok := c.uploadClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("upload Transport = %T", c.uploadClient.Transport)
	}
	if tr.TLSClientConfig == nil || tr.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Error("upload transport lacks the TLS 1.2 floor")
	}
	if tr.ResponseHeaderTimeout != DefaultUploadResponseTimeout || tr.TLSHandshakeTimeout == 0 {
		t.Errorf("upload transport deadlines: header=%s handshake=%s", tr.ResponseHeaderTimeout, tr.TLSHandshakeTimeout)
	}
	if c.uploadIdleTimeout != DefaultUploadIdleTimeout {
		t.Errorf("idle timeout = %s, want %s", c.uploadIdleTimeout, DefaultUploadIdleTimeout)
	}
}

// trickleReader yields n chunks of size bytes, sleeping gap before each.
type trickleReader struct {
	n, size int
	gap     time.Duration
}

func (r *trickleReader) Read(p []byte) (int, error) {
	if r.n == 0 {
		return 0, io.EOF
	}
	time.Sleep(r.gap)
	r.n--
	if len(p) > r.size {
		p = p[:r.size]
	}
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

// TestUploadIdleTimeout: a body that keeps moving may take as long as it
// needs; one that stops for the idle timeout fails with ErrUploadStalled.
func TestUploadIdleTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(uploadOKBody))
	}))
	defer srv.Close()
	c := New(srv.URL, "k", WithUploadIdleTimeout(300*time.Millisecond))

	// Above UploadSignMaxBytes, so the body streams through the transport
	// instead of being read up front for signing.
	big := UploadSignMaxBytes + 1024
	t.Run("steady body outlasting the idle timeout succeeds", func(t *testing.T) {
		body := io.MultiReader(&trickleReader{n: 8, size: 1, gap: 100 * time.Millisecond}, io.LimitReader(zeroReader{}, big-8))
		if _, err := c.Upload(context.Background(), body, UploadOptions{SizeBytes: big}); err != nil {
			t.Fatalf("Upload: %v", err)
		}
	})
	t.Run("stalled body fails", func(t *testing.T) {
		// The server stops reading: once the socket buffers fill, the
		// transport can't take another body byte -- the real stall.
		release := make(chan struct{})
		stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-release
		}))
		t.Cleanup(stalled.Close)
		t.Cleanup(func() { close(release) })
		sc := New(stalled.URL, "k", WithUploadIdleTimeout(300*time.Millisecond))
		start := time.Now()
		_, err := sc.Upload(context.Background(), io.LimitReader(zeroReader{}, 4*big), UploadOptions{SizeBytes: 4 * big})
		if !errors.Is(err, ErrUploadStalled) {
			t.Fatalf("err = %v, want ErrUploadStalled", err)
		}
		if el := time.Since(start); el > 10*time.Second {
			t.Errorf("stall detected after %s", el)
		}
	})
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// TestUploadSignsSmallBodies: a body up to UploadSignMaxBytes carries the
// same X-Timestamp/X-Nonce/X-Signature as every other agent request,
// keyed by the raw API key over method, path, nonce, timestamp and body.
func TestUploadSignsSmallBodies(t *testing.T) {
	const key = "0123456789abcdef0123456789abcdef"
	var mu sync.Mutex
	var gotSig, wantSig, gotLen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mac := hmac.New(sha256.New, []byte(key))
		mac.Write([]byte(r.Method + "\n" + r.URL.RequestURI() + "\n" + r.Header.Get("X-Nonce") + "\n" + r.Header.Get("X-Timestamp") + "\n"))
		mac.Write(body)
		mu.Lock()
		gotSig, wantSig, gotLen = r.Header.Get("X-Signature"), hex.EncodeToString(mac.Sum(nil)), r.Header.Get("Content-Length")
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(uploadOKBody))
	}))
	defer srv.Close()

	c := New(srv.URL, key)
	if _, err := c.Upload(context.Background(), strings.NewReader("photo bytes"), UploadOptions{Filename: "a.jpg"}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotSig == "" || gotSig != wantSig {
		t.Errorf("X-Signature = %q, want %q", gotSig, wantSig)
	}
	if gotLen != "11" {
		t.Errorf("Content-Length = %q, want 11", gotLen)
	}
}

// TestUploadLargeBodyStreamsUnsigned: above UploadSignMaxBytes the body is
// streamed (never buffered) and sent with the API key only.
func TestUploadLargeBodyStreamsUnsigned(t *testing.T) {
	size := UploadSignMaxBytes + 1
	var gotSig, gotLen string
	var gotN int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotN, _ = io.Copy(io.Discard, r.Body)
		gotSig, gotLen = r.Header.Get("X-Signature"), r.Header.Get("Content-Length")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(uploadOKBody))
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	if _, err := c.Upload(context.Background(), io.LimitReader(zeroReader{}, size), UploadOptions{SizeBytes: size}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if gotSig != "" {
		t.Errorf("large body signed: %q", gotSig)
	}
	if gotN != size || gotLen == "" {
		t.Errorf("server got %d bytes, Content-Length %q; want %d", gotN, gotLen, size)
	}
}

// TestUploadRejectsServerHashMismatch and an invalid Blake3Hash option.
func TestUploadHashChecks(t *testing.T) {
	sent := strings.Repeat("a", 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.Header.Get("X-Blake3-Hash") != sent {
			t.Errorf("X-Blake3-Hash = %q", r.Header.Get("X-Blake3-Hash"))
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"nodeUuid":"n","status":"UPLOADED","blake3Hash":"` + strings.Repeat("b", 64) + `"}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "k")

	_, err := c.Upload(context.Background(), bytes.NewReader([]byte("x")), UploadOptions{Blake3Hash: strings.ToUpper(sent)})
	if !errors.Is(err, ErrUploadHashMismatch) {
		t.Errorf("err = %v, want ErrUploadHashMismatch", err)
	}
	if _, err := c.Upload(context.Background(), bytes.NewReader([]byte("x")), UploadOptions{Blake3Hash: "nothex"}); err == nil {
		t.Error("invalid Blake3Hash accepted")
	}
}
