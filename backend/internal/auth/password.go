// Package auth holds credential primitives: password hashing, TOTP,
// secret sealing, opaque tokens and failure rate limiting.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters from RFC 9106 section 4 (second recommended option).
const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // KiB
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16

	// MinPasswordLen is counted in characters (runes); MaxPasswordLen in
	// bytes, since it bounds the input an unauthenticated caller can make
	// the server hash. Real passwords and passphrases are far below it.
	MinPasswordLen = 12
	MaxPasswordLen = 256

	// maxConcurrentHashes bounds Argon2 memory to this many times
	// argonMemory. Login is unauthenticated, so without a bound a burst of
	// parallel attempts allocates 64 MiB each and gets the pod OOM-killed.
	maxConcurrentHashes = 2
	// maxQueuedHashes bounds how many callers may wait for a hash slot.
	// Beyond it callers get ErrHashBusy at once instead of piling up
	// goroutines behind an ever-growing backlog.
	maxQueuedHashes = 8
)

var (
	hashSlots = make(chan struct{}, maxConcurrentHashes)
	hashQueue = make(chan struct{}, maxConcurrentHashes+maxQueuedHashes)
)

// ErrHashBusy reports that every hash slot and queue place is taken.
var ErrHashBusy = errors.New("password hashing is busy, try again shortly")

var ErrPasswordTooLong = fmt.Errorf("password must be at most %d bytes", MaxPasswordLen)

// idKey is argon2.IDKey limited to maxConcurrentHashes at a time, with at
// most maxQueuedHashes callers waiting. A caller whose ctx ends while it
// waits (e.g. the HTTP client disconnected) gives up instead of computing
// a hash nobody will read.
func idKey(ctx context.Context, password, salt []byte, time, memory uint32, threads uint8, keyLen uint32) ([]byte, error) {
	select {
	case hashQueue <- struct{}{}:
	default:
		return nil, ErrHashBusy
	}
	defer func() { <-hashQueue }()
	select {
	case hashSlots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-hashSlots }()
	return argon2.IDKey(password, salt, time, memory, threads, keyLen), nil
}

var ErrWeakPassword = fmt.Errorf("password must be at least %d characters", MinPasswordLen)

// HashPassword returns a PHC-formatted Argon2id hash.
func HashPassword(ctx context.Context, password string) (string, error) {
	if utf8.RuneCountInString(password) < MinPasswordLen {
		return "", ErrWeakPassword
	}
	if len(password) > MaxPasswordLen {
		return "", ErrPasswordTooLong
	}
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := idKey(ctx, []byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	if err != nil {
		return "", err
	}
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// VerifyPassword checks password against a hash produced by HashPassword.
func VerifyPassword(ctx context.Context, hash, password string) (bool, error) {
	if len(password) > MaxPasswordLen {
		return false, ErrPasswordTooLong
	}
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errors.New("unsupported password hash")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, errors.New("unsupported argon2 version")
	}
	var memory, time uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil {
		return false, err
	}
	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return false, err
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil {
		return false, err
	}
	got, err := idKey(ctx, []byte(password), salt, time, memory, threads, uint32(len(want))) // #nosec G115 -- key length from our own hash format, 32 bytes
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// dummyHash is verified against when a username does not exist so that
// login timing does not reveal which usernames are registered.
var dummyHash, _ = HashPassword(context.Background(), "not-a-real-password-placeholder")

// BurnPasswordCheck spends the same time as a real VerifyPassword call.
// It returns the queueing errors VerifyPassword can return (hash busy,
// caller's context ended, password too long) so the caller can answer the
// same way it would for a real account.
func BurnPasswordCheck(ctx context.Context, password string) error {
	_, err := VerifyPassword(ctx, dummyHash, password)
	return err
}
