package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/config"
)

const deepLinkTestURL = "branchdam://?server=https%3A%2F%2Fdam.example.com&key=01234567890123456789012345678901&agent=dev-x123"

type deepLinkHarness struct {
	dialogs      [][]string
	answer       int // exit code for "question" dialogs
	forwardCalls int
	forwardErr   error
	pairCalls    int
	pairErr      error
	cfgPath      string
}

func newDeepLinkHarness(t *testing.T) *deepLinkHarness {
	t.Helper()
	h := &deepLinkHarness{answer: dialogExitOK, forwardErr: errNoTray, cfgPath: filepath.Join(t.TempDir(), "config.yaml")}
	stubTrayDialog(t, func(_ context.Context, args ...string) (string, int, error) {
		h.dialogs = append(h.dialogs, args)
		return "", h.answer, nil
	})
	origF, origP := deepLinkForward, deepLinkPairConfig
	deepLinkForward = func(string, string) error { h.forwardCalls++; return h.forwardErr }
	deepLinkPairConfig = func(_, _, _, _ string, _ time.Duration, _ config.Config) error {
		h.pairCalls++
		return h.pairErr
	}
	t.Cleanup(func() { deepLinkForward, deepLinkPairConfig = origF, origP })
	return h
}

func (h *deepLinkHarness) run() int {
	return runPairCmd([]string{"-deeplink", "-config", h.cfgPath, deepLinkTestURL})
}

func (h *deepLinkHarness) kinds() string {
	var k []string
	for _, d := range h.dialogs {
		k = append(k, d[1])
	}
	return strings.Join(k, ",")
}

func TestDeepLinkPairForwardsToRunningTray(t *testing.T) {
	h := newDeepLinkHarness(t)
	h.forwardErr = nil
	if rc := h.run(); rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	if h.forwardCalls != 1 || h.pairCalls != 0 || len(h.dialogs) != 0 {
		t.Errorf("forward=%d pair=%d dialogs=%v; the tray owns confirmation, this process must do nothing else", h.forwardCalls, h.pairCalls, h.dialogs)
	}
}

func TestDeepLinkPairTrayRejectionDoesNotFallBackToLocalWrite(t *testing.T) {
	h := newDeepLinkHarness(t)
	h.forwardErr = errors.New("the running tray rejected the request (503)")
	if rc := h.run(); rc == 0 {
		t.Fatal("rc = 0, want failure")
	}
	if h.pairCalls != 0 {
		t.Error("a tray rejection must not fall back to a local pair")
	}
	if h.kinds() != "error" {
		t.Errorf("dialogs = %s, want one error dialog", h.kinds())
	}
}

func TestDeepLinkPairLocalFallbackConfirmsThenPairs(t *testing.T) {
	h := newDeepLinkHarness(t)
	if rc := h.run(); rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	if h.pairCalls != 1 {
		t.Errorf("pairCalls = %d, want 1", h.pairCalls)
	}
	if h.kinds() != "question,notify" {
		t.Errorf("dialogs = %s, want confirmation then success notice", h.kinds())
	}
	if !strings.Contains(strings.Join(h.dialogs[0], " "), "dam.example.com") {
		t.Error("confirmation must show the server")
	}
}

func TestDeepLinkPairDeclinedWritesNothing(t *testing.T) {
	h := newDeepLinkHarness(t)
	h.answer = dialogExitCanceled
	if rc := h.run(); rc == 0 {
		t.Error("rc = 0, want non-zero on decline")
	}
	if h.pairCalls != 0 {
		t.Error("declined confirmation must not pair")
	}
	if _, err := os.Stat(h.cfgPath); err == nil {
		t.Error("declined confirmation must not create config")
	}
}

func TestDeepLinkPairNoDialogFailsClosed(t *testing.T) {
	h := newDeepLinkHarness(t)
	h.answer = dialogExitFailed // dialog backend could not render
	if rc := h.run(); rc == 0 || h.pairCalls != 0 {
		t.Errorf("rc=%d pairCalls=%d, want failure and no pair without a working dialog", rc, h.pairCalls)
	}
}

func TestDeepLinkPairInvalidURLNeverLeaksKey(t *testing.T) {
	h := newDeepLinkHarness(t)
	bad := "branchdm://?server=https%3A%2F%2Fdam.example.com&key=01234567890123456789012345678901"
	if rc := runPairCmd([]string{"-deeplink", "-config", h.cfgPath, bad}); rc == 0 {
		t.Fatal("rc = 0, want failure")
	}
	for _, d := range h.dialogs {
		if strings.Contains(strings.Join(d, " "), "01234567890123456789012345678901") {
			t.Errorf("error dialog leaks the API key: %v", d)
		}
	}
	if h.pairCalls != 0 || h.forwardCalls != 0 {
		t.Error("an invalid URL must not reach the tray or pair")
	}
}
