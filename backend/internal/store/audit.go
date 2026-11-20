package store

import (
	"context"
	"database/sql"
	"time"
)

type AuditEntry struct {
	At        time.Time `json:"at"`
	ActorID   *int64    `json:"actorId"`
	Actor     string    `json:"actor"`
	Action    string    `json:"action"`
	Namespace string    `json:"namespace"`
	Target    string    `json:"target"`
	IP        string    `json:"ip"`
}

func (s *Store) Audit(ctx context.Context, e AuditEntry) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit (at, actor_id, actor, action, namespace, target, ip) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		unix(e.At), e.ActorID, e.Actor, e.Action, e.Namespace, e.Target, e.IP)
	return err
}

func (s *Store) PurgeAudit(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM audit WHERE at < ?`, unix(before))
	return err
}

func (s *Store) ListAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT at, actor_id, actor, action, namespace, target, ip FROM audit ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var at int64
		var actorID sql.NullInt64
		if err := rows.Scan(&at, &actorID, &e.Actor, &e.Action, &e.Namespace, &e.Target, &e.IP); err != nil {
			return nil, err
		}
		e.At = time.Unix(at, 0)
		if actorID.Valid {
			e.ActorID = &actorID.Int64
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
