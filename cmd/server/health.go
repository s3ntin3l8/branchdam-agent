package main

import (
	"context"
	"net"
	"net/http"
	"time"
)

// healthURL turns a server listen address (":8080", "0.0.0.0:8080",
// "1.2.3.4:9000") into a loopback /health URL the in-container HEALTHCHECK
// can probe. A bare client.Get("http://" + listenAddr + "/health") breaks
// whenever listenAddr is a wildcard bind address, since nothing listens on
// 0.0.0.0 or :: as a connect target.
func healthURL(listenAddr string) string {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		host, port = "", "8080"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/health"
}

// runHealthcheck probes the local /health and returns a process exit code
// (0 healthy, 1 otherwise).
func runHealthcheck(listenAddr string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL(listenAddr), nil)
	if err != nil {
		return 1
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		return 0
	}
	return 1
}
