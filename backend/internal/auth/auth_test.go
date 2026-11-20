package auth

import (
	"bytes"
	"context"
	"encoding/base32"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// RFC 6238 appendix B, SHA-1 rows (truncated to 6 digits).
func TestTOTPRFCVectors(t *testing.T) {
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte("12345678901234567890"))
	cases := []struct {
		unix int64
		code string
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1111111111, "050471"},
		{1234567890, "005924"},
		{2000000000, "279037"},
	}
	for _, c := range cases {
		step, ok := VerifyTOTP(secret, c.code, time.Unix(c.unix, 0))
		if !ok || step != c.unix/30 {
			t.Errorf("t=%d code=%s: ok=%v step=%d", c.unix, c.code, ok, step)
		}
	}
	if _, ok := VerifyTOTP(secret, "000000", time.Unix(59, 0)); ok {
		t.Error("wrong code accepted")
	}
	// Two steps away is outside the skew window.
	if _, ok := VerifyTOTP(secret, "287082", time.Unix(59+60, 0)); ok {
		t.Error("stale code accepted")
	}
}

func TestPassword(t *testing.T) {
	ctx := context.Background()
	if _, err := HashPassword(ctx, "short"); err != ErrWeakPassword {
		t.Fatalf("want ErrWeakPassword, got %v", err)
	}
	if _, err := HashPassword(ctx, strings.Repeat("x", MaxPasswordLen+1)); err != ErrPasswordTooLong {
		t.Fatalf("want ErrPasswordTooLong, got %v", err)
	}
	// The minimum counts characters, the maximum bytes: four CJK
	// characters are twelve bytes but only four characters, and one
	// hundred are three hundred bytes.
	if _, err := HashPassword(ctx, strings.Repeat("漢", 4)); err != ErrWeakPassword {
		t.Fatalf("short multibyte password: want ErrWeakPassword, got %v", err)
	}
	if _, err := HashPassword(ctx, strings.Repeat("漢", 100)); err != ErrPasswordTooLong {
		t.Fatalf("long multibyte password: want ErrPasswordTooLong, got %v", err)
	}
	if _, err := HashPassword(ctx, strings.Repeat("漢", 12)); err != nil {
		t.Fatalf("12-character multibyte password rejected: %v", err)
	}
	h, err := HashPassword(ctx, "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := VerifyPassword(ctx, h, "correct horse battery"); !ok || err != nil {
		t.Fatalf("verify: %v %v", ok, err)
	}
	if ok, _ := VerifyPassword(ctx, h, "correct horse battery!"); ok {
		t.Fatal("wrong password accepted")
	}
	if ok, err := VerifyPassword(ctx, h, strings.Repeat("x", MaxPasswordLen+1)); ok || err != ErrPasswordTooLong {
		t.Fatalf("over-long verify: %v %v", ok, err)
	}
}

func TestSealer(t *testing.T) {
	s, err := NewSealer(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := s.Seal("JBSWY3DPEHPK3PXP")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Open(sealed)
	if err != nil || got != "JBSWY3DPEHPK3PXP" {
		t.Fatalf("open: %q %v", got, err)
	}
	sealed[len(sealed)-1] ^= 1
	if _, err := s.Open(sealed); err == nil {
		t.Fatal("tampered ciphertext opened")
	}
}

func TestFailureLimiter(t *testing.T) {
	l := NewFailureLimiter(2, time.Hour)
	l.Fail("k")
	if !l.Allowed("k") {
		t.Fatal("locked after 1 failure")
	}
	l.Fail("k")
	if l.Allowed("k") {
		t.Fatal("not locked after 2 failures")
	}
	l.Reset("k")
	if !l.Allowed("k") {
		t.Fatal("still locked after reset")
	}
}

func TestHashConcurrencyBounded(t *testing.T) {
	// Occupy every slot; a hash must then wait until one is released.
	for range maxConcurrentHashes {
		hashSlots <- struct{}{}
	}
	done := make(chan struct{})
	go func() {
		HashPassword(context.Background(), "long-enough-password")
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("hash ran with every slot taken")
	case <-time.After(100 * time.Millisecond):
	}
	for range maxConcurrentHashes {
		<-hashSlots
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("hash did not run after slots were released")
	}
}

func TestHashQueueBoundedAndCancellable(t *testing.T) {
	// Fill every running slot and every queue place.
	for range maxConcurrentHashes {
		hashSlots <- struct{}{}
	}
	for range maxConcurrentHashes + maxQueuedHashes {
		hashQueue <- struct{}{}
	}
	if _, err := HashPassword(context.Background(), "long-enough-password"); !errors.Is(err, ErrHashBusy) {
		t.Fatalf("full queue: want ErrHashBusy, got %v", err)
	}
	for range maxConcurrentHashes + maxQueuedHashes {
		<-hashQueue
	}

	// A queued caller whose context ends gives up instead of hashing.
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := HashPassword(ctx, "long-enough-password")
		errc <- err
	}()
	// Wait until the caller is actually queued, then cancel.
	deadline := time.Now().Add(5 * time.Second)
	for len(hashQueue) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled waiter: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled waiter did not return")
	}
	for range maxConcurrentHashes {
		<-hashSlots
	}
}

// In-flight attempts count towards the limit: a concurrent burst cannot
// all pass Acquire before any of them fails.
func TestFailureLimiterCountsInflight(t *testing.T) {
	l := NewFailureLimiter(2, time.Hour)
	if !l.Acquire("k") {
		t.Fatal("first attempt refused")
	}
	if !l.Acquire("k") {
		t.Fatal("second attempt refused")
	}
	if l.Acquire("k") {
		t.Fatal("third concurrent attempt allowed")
	}
	if l.Allowed("k") {
		t.Fatal("Allowed ignores in-flight attempts")
	}
	l.Release("k", false)
	if !l.Acquire("k") {
		t.Fatal("attempt after a non-failure release refused")
	}
	l.Release("k", true)
	l.Release("k", true)
	if l.Acquire("k") {
		t.Fatal("attempt allowed after two failures")
	}
}

func TestFailureLimiterResetAndCleanup(t *testing.T) {
	l := NewFailureLimiter(2, time.Hour)
	// A successful attempt leaves no entry behind.
	if !l.Acquire("ok") {
		t.Fatal("attempt refused")
	}
	l.Release("ok", false)
	if len(l.entries) != 0 {
		t.Fatalf("successful attempt left entries: %v", l.entries)
	}

	// Reset (a successful login) must not lose other in-flight attempts:
	// they still count towards the limit, and a later failure is recorded.
	if !l.Acquire("k") {
		t.Fatal("first attempt refused")
	}
	if !l.Acquire("k") {
		t.Fatal("second attempt refused")
	}
	l.Reset("k")
	if l.Acquire("k") {
		t.Fatal("in-flight attempts forgotten by Reset")
	}
	l.Release("k", false) // the successful attempt itself
	l.Release("k", true)  // the other in-flight attempt fails
	if e := l.entries["k"]; e == nil || e.count != 1 || e.inflight != 0 {
		t.Fatalf("failure after Reset not recorded: %+v", e)
	}
	l.Reset("k")
	if len(l.entries) != 0 {
		t.Fatalf("Reset with nothing in flight left entries: %v", l.entries)
	}
}

func TestFailureLimiterBoundedAndSweep(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := NewFailureLimiter(2, 15*time.Minute)
	l.now = func() time.Time { return now }
	l.maxEntries = 5
	l.sweepInterval = time.Minute

	// Inserting more distinct keys than maxEntries stays capped at maxEntries,
	// evicting the oldest entries first.
	for i := range 12 {
		now = now.Add(time.Second)
		l.Fail(fmt.Sprintf("k%d", i))
	}
	if len(l.entries) != 5 || l.order.Len() != 5 {
		t.Fatalf("entries=%d order=%d, want 5", len(l.entries), l.order.Len())
	}
	for i := range 7 {
		if _, ok := l.entries[fmt.Sprintf("k%d", i)]; ok {
			t.Fatalf("oldest key k%d was not evicted", i)
		}
	}
	for i := 7; i < 12; i++ {
		if _, ok := l.entries[fmt.Sprintf("k%d", i)]; !ok {
			t.Fatalf("recent key k%d was unexpectedly evicted", i)
		}
	}

	// Sweeps are throttled to sweepInterval rather than running on every failure.
	firstSweep := l.lastSweep
	now = firstSweep.Add(30 * time.Second)
	l.Fail("within-interval")
	if !l.lastSweep.Equal(firstSweep) {
		t.Fatalf("sweep ran before sweepInterval: lastSweep=%v, firstSweep=%v", l.lastSweep, firstSweep)
	}

	// Once past window and sweepInterval, expired entries are swept cleanly.
	now = now.Add(20 * time.Minute)
	l.Fail("fresh")
	if !l.lastSweep.Equal(now) {
		t.Fatalf("sweep did not run after sweepInterval: lastSweep=%v, now=%v", l.lastSweep, now)
	}
	if len(l.entries) != 1 || l.order.Len() != 1 {
		t.Fatalf("expired entries not swept: entries=%d order=%d", len(l.entries), l.order.Len())
	}
	if _, ok := l.entries["fresh"]; !ok {
		t.Fatal("fresh entry missing after sweep")
	}

	// In-flight entries are not evicted to make room for new keys, and Acquire
	// fails closed if every slot in the cap is actively in flight.
	l2 := NewFailureLimiter(2, 15*time.Minute)
	l2.now = func() time.Time { return now }
	l2.maxEntries = 2
	if !l2.Acquire("inflight-1") {
		t.Fatal("inflight-1 acquire failed")
	}
	l2.Fail("evictable")
	l2.Fail("newer")
	if _, ok := l2.entries["inflight-1"]; !ok {
		t.Fatal("in-flight entry was evicted")
	}
	if _, ok := l2.entries["evictable"]; ok {
		t.Fatal("evictable entry was not evicted")
	}
	if !l2.Acquire("inflight-2") {
		t.Fatal("inflight-2 acquire failed")
	}
	if l2.Acquire("overflow") {
		t.Fatal("Acquire succeeded despite all maxEntries slots being in flight")
	}
	if len(l2.entries) != 2 {
		t.Fatalf("cap exceeded: len=%d", len(l2.entries))
	}
}
