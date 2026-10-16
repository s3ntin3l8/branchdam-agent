package branchdam

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

// Rotated-key delivery (branchDAM S13, docs/agent-api.md "Key rotation
// delivery"). When an operator rotates this device's key, the server keeps
// the old key working for a grace window and hands the new key out on the
// device's next handshake, sealed so that only the holder of the key that
// request authenticated with can open it. The device stores the new key
// durably, then sends a request with it; that first request adopts it
// server-side and revokes every older key of the pairing at once.
//
// The client side, in order:
//
//  1. Handshake answers with pendingRotation.sealedApiKey.
//  2. OpenRotatedKey opens it with the key the handshake authenticated with.
//  3. The KeyPersister (WithKeyPersister) writes it durably. A failure, a
//     missing persister, or a value that does not open leaves the client on
//     its current key.
//  4. Only then does the client switch its in-memory key, and it adopts the
//     new key at once with a confirming handshake.
//  5. If the new key is refused (401) on that first use, the client goes
//     back to the old key, retries that one request with it, writes the old
//     key back to the store once the server accepts it again, and never
//     tries the refused key id again in this process.

// rotationSealInfo is the HMAC-SHA256 message that derives the rotation
// transport key from the device's current API key. Must match branchDAM's
// pairing.RotationSealInfo byte for byte.
const rotationSealInfo = "branchdam-key-rotation-v1"

// sealedKeyPrefix is the version tag on pendingRotation.sealedApiKey.
const sealedKeyPrefix = "v1:"

// ErrSealedKeyInvalid is returned by OpenRotatedKey for any value that does
// not open: wrong version prefix, bad base64, too short, a GCM tag
// mismatch (wrong current key, tampered value), or a plaintext that cannot
// be an API key. The causes are deliberately not distinguished. Per the
// contract a value that does not open is ignored; the client never falls
// back to anything else.
var ErrSealedKeyInvalid = errors.New("branchdam: sealed rotated key does not open with the current API key")

// OpenRotatedKey opens a pendingRotation.sealedApiKey value with
// currentKey, the API key the handshake that carried it authenticated
// with. The format is "v1:" + standard base64 (with padding) of
// nonce (12 bytes) || ciphertext || GCM tag (16 bytes), AES-256-GCM with no
// associated data, keyed by HMAC-SHA256(key = currentKey, message =
// "branchdam-key-rotation-v1"). This is the standard-library equivalent of
// branchDAM's pairing.OpenRotatedKey.
func OpenRotatedKey(currentKey, sealed string) (string, error) {
	if currentKey == "" || !strings.HasPrefix(sealed, sealedKeyPrefix) {
		return "", ErrSealedKeyInvalid
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(sealed, sealedKeyPrefix))
	if err != nil {
		return "", ErrSealedKeyInvalid
	}
	mac := hmac.New(sha256.New, []byte(currentKey))
	mac.Write([]byte(rotationSealInfo))
	block, err := aes.NewCipher(mac.Sum(nil))
	if err != nil {
		return "", fmt.Errorf("branchdam: rotation cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("branchdam: rotation GCM: %w", err)
	}
	if len(raw) < gcm.NonceSize()+gcm.Overhead() {
		return "", ErrSealedKeyInvalid
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return "", ErrSealedKeyInvalid
	}
	if !validAPIKey(plain) {
		return "", ErrSealedKeyInvalid
	}
	return string(plain), nil
}

// validAPIKey reports whether b can be sent as an X-API-Key header value:
// non-empty, at most 1 KiB, printable ASCII without spaces. A value that
// opened but fails this is treated like one that did not open, so a key
// net/http would refuse to send is never persisted.
func validAPIKey(b []byte) bool {
	if len(b) == 0 || len(b) > 1024 {
		return false
	}
	for _, c := range b {
		if c < 0x21 || c > 0x7e {
			return false
		}
	}
	return true
}

// KeyPersister durably stores a rotated API key, replacing the one the
// client was built with, so a restart comes back on it. The client calls
// it before it switches to the new key, and again with the old key if the
// server refuses the new one (see the package's rotation notes in
// keyrotation.go). A non-nil error means nothing usable was stored: the
// client then stays on its current key. The error is logged at Warn, so it
// should say what the operator has to do.
type KeyPersister func(ctx context.Context, apiKey string) error

// WithKeyPersister lets the client adopt rotated keys from the handshake.
// Without one the client only logs that a rotation is pending: a key that
// lives only in memory would be lost on restart after the server has
// already revoked the old one.
func WithKeyPersister(p KeyPersister) Option {
	return func(c *Client) { c.persistKey = p }
}

// keyAdoption is the client's state between switching to a rotated key and
// the server's first answer to a request made with it.
type keyAdoption struct {
	newKey string
	oldKey string
	keyID  int64
}

// currentKey returns the API key the next request should use.
func (c *Client) currentKey() string {
	c.keyMu.Lock()
	defer c.keyMu.Unlock()
	return c.apiKey
}

// observeAuth runs after every response that reached the server, with the
// key the request used and the response status. It completes or rolls
// back a pending key adoption and returns a key to retry the same request
// with, or "" for no retry. Callers retry at most once; a request whose
// body cannot be replayed (Upload) ignores the result, and the next
// request uses the reverted key.
func (c *Client) observeAuth(ctx context.Context, usedKey string, status int) string {
	ok := status >= 200 && status < 300
	unauthorized := status == http.StatusUnauthorized
	if !ok && !unauthorized {
		return ""
	}

	var restore, retry string
	c.keyMu.Lock()
	switch {
	case c.adopting != nil && usedKey == c.adopting.newKey:
		a := c.adopting
		c.adopting = nil
		if ok {
			slog.Info("branchdam: the server accepted the rotated API key; the previous key is now revoked", "keyId", a.keyID)
			break
		}
		// The new key was refused on its first use. Go back to the old one
		// for this process and retry this request with it; it is written
		// back to the store once the server accepts it again.
		if c.refusedKeyIDs == nil {
			c.refusedKeyIDs = map[int64]struct{}{}
		}
		c.refusedKeyIDs[a.keyID] = struct{}{}
		if c.apiKey == a.newKey { // pragma: allowlist secret
			c.apiKey = a.oldKey // pragma: allowlist secret
		}
		c.restoreKey = a.oldKey
		retry = a.oldKey
		slog.Warn("branchdam: the server refused the rotated API key on its first use; falling back to the previous key", "keyId", a.keyID)
	case c.restoreKey != "" && usedKey == c.restoreKey:
		if ok {
			restore = c.restoreKey
			c.restoreKey = ""
		} else {
			c.restoreKey = ""
			slog.Error("branchdam: the server refused both the rotated API key and the previous one; re-pair this device from branchDAM's Companion Pairing page")
		}
	case unauthorized && usedKey != c.apiKey:
		// This request started before the client switched keys (a
		// concurrent adoption or fallback) and was refused with the
		// stale key: retry it once with the current one.
		retry = c.apiKey
	}
	c.keyMu.Unlock()

	if restore != "" && c.persistKey != nil {
		if err := c.persistKey(ctx, restore); err != nil {
			slog.Error("branchdam: could not write the previous API key back after the rotated key was refused; the stored key is the refused one, so re-pair this device before restarting the agent", "err", err)
			c.keyMu.Lock()
			if c.restoreKey == "" && c.apiKey == restore { // pragma: allowlist secret
				c.restoreKey = restore // try again after the next accepted request
			}
			c.keyMu.Unlock()
		} else {
			slog.Info("branchdam: restored the previous API key in the key store after the rotated key was refused")
		}
	}
	return retry
}

// adoptRotation acts on a handshake response's pendingRotation: open the
// sealed key with usedKey (the key that handshake authenticated with),
// store it, switch to it, and adopt it at once with a second handshake,
// whose response it returns. It returns nil whenever it did not get that
// far; the caller then keeps the first response.
func (c *Client) adoptRotation(ctx context.Context, req HandshakeRequest, usedKey string, rot *PendingRotationHint) *HandshakeResponse {
	logAttrs := []any{"keyId", rot.KeyID, "previousKeyExpiresAtUnix", rot.PreviousKeyExpiresAtUnix}
	if rot.SealedAPIKey == "" {
		slog.Warn("branchdam: the server rotated this device's API key but cannot deliver it (no BRANCHDAM_SECRET_KEY on the server, or a rotation from before delivery existed); re-pair this device with `branchdam-agent pair <branchdam-url>` before the current key expires", logAttrs...)
		return nil
	}
	if c.persistKey == nil {
		slog.Warn("branchdam: the server has a rotated API key for this device; this command does not store keys, so it stays on the current one (the tray, ingest and queue-drain adopt it)", logAttrs...)
		return nil
	}
	// One adoption at a time per client. A concurrent handshake that finds
	// one in progress leaves the rotation to it.
	if !c.adoptMu.TryLock() {
		return nil
	}
	defer c.adoptMu.Unlock()

	c.keyMu.Lock()
	_, refused := c.refusedKeyIDs[rot.KeyID]
	stale := c.apiKey != usedKey || c.adopting != nil // pragma: allowlist secret
	c.keyMu.Unlock()
	if refused {
		slog.Warn("branchdam: the server offered a rotated API key it already refused once; staying on the current key -- re-pair this device if this persists", logAttrs...)
		return nil
	}
	if stale {
		return nil
	}

	newKey, err := OpenRotatedKey(usedKey, rot.SealedAPIKey)
	if err != nil {
		slog.Warn("branchdam: ignoring a rotated API key that does not open with the current key", append(logAttrs, "err", err)...)
		return nil
	}
	if newKey == usedKey {
		return nil
	}
	if err := c.persistKey(ctx, newKey); err != nil {
		slog.Warn("branchdam: could not store the rotated API key; staying on the current key", append(logAttrs, "err", err)...)
		return nil
	}

	c.keyMu.Lock()
	if c.apiKey != usedKey { // pragma: allowlist secret
		c.keyMu.Unlock()
		return nil
	}
	c.apiKey = newKey // pragma: allowlist secret
	c.adopting = &keyAdoption{newKey: newKey, oldKey: usedKey, keyID: rot.KeyID}
	c.keyMu.Unlock()
	slog.Info("branchdam: stored the rotated API key; switching to it", logAttrs...)

	// Adopt it now rather than on whatever request happens next: a
	// handshake is replayable (unlike an upload), so a refusal can fall back
	// and retry, and its response confirms currentKeyId.
	var confirm HandshakeResponse
	confirmKey, err := c.do(ctx, http.MethodPost, handshakePath, req, &confirm)
	if err != nil {
		slog.Warn("branchdam: could not confirm the rotated API key yet; the next request adopts it", append(logAttrs, "err", err)...)
		return nil
	}
	if confirmKey == newKey && confirm.CurrentKeyID != 0 && confirm.CurrentKeyID != rot.KeyID {
		slog.Warn("branchdam: the server reports a different current key id after adopting the rotated key", "keyId", rot.KeyID, "currentKeyId", confirm.CurrentKeyID)
	}
	return &confirm
}
