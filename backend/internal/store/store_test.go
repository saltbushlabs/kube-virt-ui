package store

import (
	"context"
	"errors"
	"path/filepath"
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

func TestPurgeAudit(t *testing.T) {
	ctx, s := context.Background(), open(t)
	now := time.Unix(1_700_000_000, 0)
	cutoff := now.Add(-90 * 24 * time.Hour)

	for _, e := range []AuditEntry{
		{At: cutoff.Add(-time.Second), Actor: "cli", Action: "old"},
		{At: cutoff, Actor: "cli", Action: "boundary"},
		{At: now, Actor: "cli", Action: "recent"},
	} {
		if err := s.Audit(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PurgeAudit(ctx, cutoff); err != nil {
		t.Fatal(err)
	}
	entries, err := s.ListAudit(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Action != "recent" || entries[1].Action != "boundary" {
		t.Fatalf("remaining audit entries: %+v", entries)
	}
}
