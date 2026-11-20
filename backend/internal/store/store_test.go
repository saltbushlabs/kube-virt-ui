package store

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestInviteSingleUse(t *testing.T) {
	ctx, s, now := context.Background(), open(t), time.Now()
	inv, err := s.CreateInvite(ctx, "h1", Invite{Namespace: "team-a", Role: "owner", CreatedAt: now, ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.RedeemInviteNewUser(ctx, inv, User{Username: "alice", PasswordHash: "x", TOTPSecret: []byte("s")}, []string{"r1"}, now)
	if err != nil {
		t.Fatal(err)
	}
	ms, _ := s.Memberships(ctx, u.ID)
	if len(ms) != 1 || ms[0].Namespace != "team-a" || ms[0].Role != "owner" {
		t.Fatalf("memberships: %+v", ms)
	}
	// Second redemption of the same invite must fail and must not create a user.
	_, err = s.RedeemInviteNewUser(ctx, inv, User{Username: "mallory", PasswordHash: "x", TOTPSecret: []byte("s")}, nil, now)
	if !errors.Is(err, ErrInviteUnusable) {
		t.Fatalf("want ErrInviteUnusable, got %v", err)
	}
	if _, err := s.UserByUsername(ctx, "mallory"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("user created despite failed claim: %v", err)
	}
}

func TestInviteExpiredAndUsernameTaken(t *testing.T) {
	ctx, s, now := context.Background(), open(t), time.Now()
	expired, _ := s.CreateInvite(ctx, "h1", Invite{CreatedAt: now, ExpiresAt: now.Add(-time.Second)})
	if _, err := s.RedeemInviteNewUser(ctx, expired, User{Username: "a", TOTPSecret: []byte{}}, nil, now); !errors.Is(err, ErrInviteUnusable) {
		t.Fatalf("expired: %v", err)
	}
	a, _ := s.CreateInvite(ctx, "h2", Invite{CreatedAt: now, ExpiresAt: now.Add(time.Hour)})
	b, _ := s.CreateInvite(ctx, "h3", Invite{CreatedAt: now, ExpiresAt: now.Add(time.Hour)})
	if _, err := s.RedeemInviteNewUser(ctx, a, User{Username: "bob", TOTPSecret: []byte{}}, nil, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedeemInviteNewUser(ctx, b, User{Username: "BOB", TOTPSecret: []byte{}}, nil, now); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("case-insensitive duplicate: %v", err)
	}
	if inv, _ := s.InviteByTokenHash(ctx, "h3"); !inv.Usable(now) {
		t.Fatal("invite consumed by failed redemption")
	}
}

func TestTOTPReplayAndRecoveryCodes(t *testing.T) {
	ctx, s, now := context.Background(), open(t), time.Now()
	inv, _ := s.CreateInvite(ctx, "h", Invite{CreatedAt: now, ExpiresAt: now.Add(time.Hour)})
	u, _ := s.RedeemInviteNewUser(ctx, inv, User{Username: "c", TOTPSecret: []byte{}}, []string{"r1"}, now)
	if ok, _ := s.AdvanceTOTPStep(ctx, u.ID, 100); !ok {
		t.Fatal("first use rejected")
	}
	if ok, _ := s.AdvanceTOTPStep(ctx, u.ID, 100); ok {
		t.Fatal("replay accepted")
	}
	if ok, _ := s.UseRecoveryCode(ctx, u.ID, "r1", now); !ok {
		t.Fatal("recovery code rejected")
	}
	if ok, _ := s.UseRecoveryCode(ctx, u.ID, "r1", now); ok {
		t.Fatal("recovery code reused")
	}
}

func TestSetInvitePendingTOTPConditional(t *testing.T) {
	ctx, s, now := context.Background(), open(t), time.Now()
	inv, err := s.CreateInvite(ctx, "h-totp", Invite{CreatedAt: now, ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	set, err := s.SetInvitePendingTOTP(ctx, inv.ID, []byte("first-secret"))
	if err != nil || !set {
		t.Fatalf("first SetInvitePendingTOTP: set=%v err=%v", set, err)
	}
	set, err = s.SetInvitePendingTOTP(ctx, inv.ID, []byte("second-secret"))
	if err != nil || set {
		t.Fatalf("second SetInvitePendingTOTP: want (false, nil), got (%v, %v)", set, err)
	}
	got, err := s.InviteByTokenHash(ctx, "h-totp")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.PendingTOTP, []byte("first-secret")) {
		t.Fatalf("pending_totp overwritten: got %q, want %q", got.PendingTOTP, "first-secret")
	}
}

func TestSQLiteDSNPragmaOrdering(t *testing.T) {
	dsn := sqliteDSN("/tmp/test.db")
	busyIdx := strings.Index(dsn, "_pragma=busy_timeout(5000)")
	walIdx := strings.Index(dsn, "_pragma=journal_mode(WAL)")
	fkIdx := strings.Index(dsn, "_pragma=foreign_keys(1)")
	if busyIdx < 0 || walIdx < 0 || fkIdx < 0 {
		t.Fatalf("missing expected pragmas in DSN: %q", dsn)
	}
	if busyIdx >= walIdx || busyIdx >= fkIdx {
		t.Fatalf("busy_timeout must precede other pragmas in DSN: %q", dsn)
	}

	s := open(t)
	var busy, fk int
	var journal string
	if err := s.db.QueryRow(`PRAGMA busy_timeout`).Scan(&busy); err != nil || busy != 5000 {
		t.Fatalf("busy_timeout: %d %v", busy, err)
	}
	if err := s.db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
		t.Fatalf("foreign_keys: %d %v", fk, err)
	}
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil || strings.ToLower(journal) != "wal" {
		t.Fatalf("journal_mode: %q %v", journal, err)
	}
}
