package auth

import (
	"encoding/json"
	"fmt"
	"time"
)

// sessionPayload is the wire form stored in Redis.
//
// Field names are deliberately terse to keep the Redis value small; a session
// is read on every authenticated request.
type sessionPayload struct {
	UID int64  `json:"uid"`
	IID int64  `json:"iid"`
	ROL string `json:"rol"`
	EML string `json:"eml"`
	NM  string `json:"nm"`
	TH  string `json:"th"`
	IAT int64  `json:"iat"`
	EXP int64  `json:"exp"`
}

func encodeSession(sess Session) (string, error) {
	b, err := json.Marshal(sessionPayload{
		UID: sess.UserID,
		IID: sess.InstitutionID,
		ROL: sess.Role,
		EML: sess.Email,
		NM:  sess.DisplayName,
		TH:  sess.TokenHash,
		IAT: sess.IssuedAt.UTC().Unix(),
		EXP: sess.ExpiresAt.UTC().Unix(),
	})
	if err != nil {
		return "", fmt.Errorf("auth: encode session: %w", err)
	}
	return string(b), nil
}

func decodeSession(encoded string) (Session, error) {
	var p sessionPayload
	if err := json.Unmarshal([]byte(encoded), &p); err != nil {
		return Session{}, fmt.Errorf("auth: decode session: %w", err)
	}
	if p.UID == 0 || p.TH == "" {
		return Session{}, ErrSessionNotFound
	}
	return Session{
		UserID:        p.UID,
		InstitutionID: p.IID,
		Role:          p.ROL,
		Email:         p.EML,
		DisplayName:   p.NM,
		TokenHash:     p.TH,
		IssuedAt:      time.Unix(p.IAT, 0).UTC(),
		ExpiresAt:     time.Unix(p.EXP, 0).UTC(),
	}, nil
}
