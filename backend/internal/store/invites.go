package store

import (
	"context"
	"database/sql"
	"time"
)

type Invite struct {
	ID          int64      `json:"id"`
	Email       string     `json:"email"`
	Namespace   string     `json:"namespace"` // empty for admin-only invites
	Role        string     `json:"role"`
	MakeAdmin   bool       `json:"makeAdmin"`
	PendingTOTP []byte     `json:"-"` // sealed secret shown during redemption
	CreatedBy   *int64     `json:"createdBy"`
	CreatedAt   time.Time  `json:"createdAt"`
	ExpiresAt   time.Time  `json:"expiresAt"`
	UsedAt      *time.Time `json:"usedAt"`
	UsedBy      *int64     `json:"usedBy"`
	RevokedAt   *time.Time `json:"revokedAt"`
}

func (i Invite) Usable(now time.Time) bool {
	return i.UsedAt == nil && i.RevokedAt == nil && now.Before(i.ExpiresAt)
}

const inviteCols = `id, email, namespace, role, make_admin, pending_totp, created_by, created_at, expires_at, used_at, used_by, revoked_at`

func scanInvite(row scanner) (Invite, error) {
	var i Invite
	var createdBy, usedBy, usedAt, revokedAt sql.NullInt64
	var created, expires int64
	err := row.Scan(&i.ID, &i.Email, &i.Namespace, &i.Role, &i.MakeAdmin, &i.PendingTOTP,
		&createdBy, &created, &expires, &usedAt, &usedBy, &revokedAt)
	if createdBy.Valid {
		i.CreatedBy = &createdBy.Int64
	}
	if usedBy.Valid {
		i.UsedBy = &usedBy.Int64
	}
	i.CreatedAt, i.ExpiresAt = time.Unix(created, 0), time.Unix(expires, 0)
	i.UsedAt, i.RevokedAt = fromUnix(usedAt), fromUnix(revokedAt)
	return i, notFound(err)
}

func (s *Store) CreateInvite(ctx context.Context, tokenHash string, i Invite) (Invite, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO invites (token_hash, email, namespace, role, make_admin, created_by, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		tokenHash, i.Email, i.Namespace, i.Role, i.MakeAdmin, i.CreatedBy, unix(i.CreatedAt), unix(i.ExpiresAt))
	if err != nil {
		return Invite{}, err
	}
	i.ID, _ = res.LastInsertId()
	return i, nil
}

func (s *Store) InviteByTokenHash(ctx context.Context, tokenHash string) (Invite, error) {
	return scanInvite(s.db.QueryRowContext(ctx, `SELECT `+inviteCols+` FROM invites WHERE token_hash = ?`, tokenHash))
}

func (s *Store) ListInvites(ctx context.Context) ([]Invite, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+inviteCols+` FROM invites ORDER BY id DESC LIMIT 500`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Invite
	for rows.Next() {
		i, err := scanInvite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// SetInvitePendingTOTP stores the sealed pending TOTP secret for invite id
// only if none is set yet. It reports whether the update took effect.
func (s *Store) SetInvitePendingTOTP(ctx context.Context, id int64, sealed []byte) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE invites SET pending_totp = ? WHERE id = ? AND pending_totp IS NULL`, sealed, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *Store) RevokeInvite(ctx context.Context, id int64, now time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE invites SET revoked_at = ? WHERE id = ? AND used_at IS NULL AND revoked_at IS NULL`, unix(now), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrInviteUnusable
	}
	return nil
}

// claimInvite marks an invite used. The conditional update is what makes
// invites single-use even under concurrent redemption attempts.
func claimInvite(ctx context.Context, tx *sql.Tx, inviteID, userID int64, now time.Time) error {
	res, err := tx.ExecContext(ctx,
		`UPDATE invites SET used_at = ?, used_by = ?
		 WHERE id = ? AND used_at IS NULL AND revoked_at IS NULL AND expires_at > ?`,
		unix(now), userID, inviteID, unix(now))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrInviteUnusable
	}
	return nil
}

// RedeemInviteNewUser creates u and its membership from invite i atomically.
func (s *Store) RedeemInviteNewUser(ctx context.Context, i Invite, u User, recoveryHashes []string, now time.Time) (User, error) {
	u.IsAdmin = i.MakeAdmin
	u.CreatedAt = now
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		if u, err = insertUser(ctx, tx, u, recoveryHashes); err != nil {
			return err
		}
		if err := claimInvite(ctx, tx, i.ID, u.ID, now); err != nil {
			return err
		}
		if i.Namespace != "" {
			return upsertMembership(ctx, tx, Membership{UserID: u.ID, Namespace: i.Namespace, Role: i.Role}, now)
		}
		return nil
	})
	return u, err
}

// AcceptInvite applies invite i to an existing user.
func (s *Store) AcceptInvite(ctx context.Context, i Invite, userID int64, now time.Time) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if err := claimInvite(ctx, tx, i.ID, userID, now); err != nil {
			return err
		}
		if i.MakeAdmin {
			if _, err := tx.ExecContext(ctx, `UPDATE users SET is_admin = 1 WHERE id = ?`, userID); err != nil {
				return err
			}
		}
		if i.Namespace != "" {
			return upsertMembership(ctx, tx, Membership{UserID: userID, Namespace: i.Namespace, Role: i.Role}, now)
		}
		return nil
	})
}
