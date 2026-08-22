package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthURL(t *testing.T) {
	cases := map[string]string{
		":8080":            "http://127.0.0.1:8080/health",
		"0.0.0.0:8080":     "http://127.0.0.1:8080/health",
		"1.2.3.4:9000":     "http://1.2.3.4:9000/health",
		"not-a-valid-addr": "http://127.0.0.1:8080/health",
	}
	for in, want := range cases {
		if got := healthURL(in); got != want {
			t.Errorf("healthURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRunHealthcheckOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if got := runHealthcheck(srv.Listener.Addr().String()); got != 0 {
		t.Errorf("runHealthcheck = %d, want 0", got)
	}
}

func TestRunHealthcheckUnreachable(t *testing.T) {
	if got := runHealthcheck("127.0.0.1:1"); got != 1 {
		t.Errorf("runHealthcheck against closed port = %d, want 1", got)
	}
}
