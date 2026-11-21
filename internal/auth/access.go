package auth

import (
	"context"
	"errors"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ValidUUID guards path ids so malformed ones are a 404, not a Postgres cast error.
func ValidUUID(s string) bool { return uuidPattern.MatchString(s) }

type Access struct {
	CanView bool
	CanEdit bool
}

// MeetingAccess applies BR-002 (view: admin, owner, participant) and BR-003
// (edit: admin, owner). A missing meeting yields no access, so callers return 404 for both.
func MeetingAccess(ctx context.Context, pool *pgxpool.Pool, u *User, meetingID string) (Access, error) {
	if !ValidUUID(meetingID) {
		return Access{}, nil
	}
	var ownerID string
	var participant bool
	err := pool.QueryRow(ctx, `
		SELECT m.owner_id,
		       EXISTS (SELECT 1 FROM meeting_participants p WHERE p.meeting_id = m.id AND p.user_id = $2)
		FROM meetings m WHERE m.id = $1`, meetingID, u.ID).Scan(&ownerID, &participant)
	if errors.Is(err, pgx.ErrNoRows) {
		return Access{}, nil
	}
	if err != nil {
		return Access{}, err
	}
	edit := u.IsAdmin() || ownerID == u.ID
	return Access{CanView: edit || participant, CanEdit: edit}, nil
}
