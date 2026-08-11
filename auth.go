package arxiv

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net/mail"
	"strings"
	"time"

	"gorm.io/gorm"
)

const maxLoginCodeAttempts = 6

// NormalizeEmail returns a stable, lower-case mailbox for account lookup.
func NormalizeEmail(email string) (string, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return "", fmt.Errorf("email is required")
	}
	addr, err := mail.ParseAddress(email)
	if err != nil {
		return "", fmt.Errorf("invalid email address")
	}
	normalized := strings.ToLower(strings.TrimSpace(addr.Address))
	if normalized == "" || !strings.Contains(normalized, "@") {
		return "", fmt.Errorf("invalid email address")
	}
	return normalized, nil
}

// CreateLoginCode creates a short-lived one-time code for email login.
func (c *Cache) CreateLoginCode(ctx context.Context, email string, ttl time.Duration) (*LoginCode, string, error) {
	normalized, err := NormalizeEmail(email)
	if err != nil {
		return nil, "", err
	}
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}

	code, err := randomDigits(6)
	if err != nil {
		return nil, "", err
	}
	salt, err := randomToken(18)
	if err != nil {
		return nil, "", err
	}

	loginCode := &LoginCode{
		ID:        "lc_" + mustRandomToken(18),
		Email:     normalized,
		CodeSalt:  salt,
		CodeHash:  hashLoginCode(salt, code),
		ExpiresAt: time.Now().UTC().Add(ttl),
	}
	if err := c.db.WithContext(ctx).Create(loginCode).Error; err != nil {
		return nil, "", fmt.Errorf("create login code: %w", err)
	}
	return loginCode, code, nil
}

// ConsumeLoginCode verifies a code and returns the corresponding user,
// creating a free account on first successful login.
func (c *Cache) ConsumeLoginCode(ctx context.Context, email, code string) (*User, error) {
	normalized, err := NormalizeEmail(email)
	if err != nil {
		return nil, err
	}
	code = strings.TrimSpace(code)
	if len(code) != 6 {
		return nil, fmt.Errorf("invalid or expired code")
	}

	var codes []LoginCode
	err = c.db.WithContext(ctx).
		Where("email = ? AND used_at IS NULL AND expires_at > ?", normalized, time.Now().UTC()).
		Order("created_at DESC").
		Limit(5).
		Find(&codes).Error
	if err != nil {
		return nil, fmt.Errorf("load login codes: %w", err)
	}
	if len(codes) == 0 {
		return nil, fmt.Errorf("invalid or expired code")
	}

	now := time.Now().UTC()
	var matched *LoginCode
	for i := range codes {
		candidate := &codes[i]
		if candidate.Attempts >= maxLoginCodeAttempts {
			continue
		}
		expected := hashLoginCode(candidate.CodeSalt, code)
		if subtle.ConstantTimeCompare([]byte(candidate.CodeHash), []byte(expected)) == 1 {
			matched = candidate
			break
		}
		if err := c.db.WithContext(ctx).
			Model(&LoginCode{}).
			Where("id = ? AND used_at IS NULL AND attempts < ?", candidate.ID, maxLoginCodeAttempts).
			Update("attempts", gorm.Expr("attempts + 1")).Error; err != nil {
			return nil, fmt.Errorf("update login attempts: %w", err)
		}
	}
	if matched == nil {
		return nil, fmt.Errorf("invalid or expired code")
	}

	consume := c.db.WithContext(ctx).
		Model(&LoginCode{}).
		Where("id = ? AND used_at IS NULL AND expires_at > ? AND attempts < ?", matched.ID, now, maxLoginCodeAttempts).
		Updates(map[string]any{
			"used_at":  now,
			"attempts": gorm.Expr("attempts + 1"),
		})
	if consume.Error != nil {
		return nil, fmt.Errorf("mark login code used: %w", consume.Error)
	}
	if consume.RowsAffected != 1 {
		return nil, fmt.Errorf("invalid or expired code")
	}

	user, err := c.FindOrCreateUser(ctx, normalized, "", "", true, "email", now)
	if err != nil {
		return nil, err
	}
	return user, nil
}

// CreateUserSession creates a server-side session and returns the raw cookie token.
func (c *Cache) CreateUserSession(ctx context.Context, userID, ip, userAgent string, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		ttl = 30 * 24 * time.Hour
	}
	rawToken, err := randomToken(36)
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	session := &UserSession{
		ID:         "sess_" + mustRandomToken(18),
		UserID:     userID,
		TokenHash:  hashSessionToken(rawToken),
		UserAgent:  trimForStorage(userAgent, 512),
		IP:         trimForStorage(ip, 128),
		ExpiresAt:  now.Add(ttl),
		LastSeenAt: now,
	}
	if err := c.db.WithContext(ctx).Create(session).Error; err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	return rawToken, nil
}

// UserForSessionToken returns the signed-in user for a raw session token.
func (c *Cache) UserForSessionToken(ctx context.Context, rawToken string) (*User, error) {
	rawToken = strings.TrimSpace(rawToken)
	if rawToken == "" {
		return nil, gorm.ErrRecordNotFound
	}

	var session UserSession
	now := time.Now().UTC()
	err := c.db.WithContext(ctx).
		Where("token_hash = ? AND expires_at > ?", hashSessionToken(rawToken), now).
		First(&session).Error
	if err != nil {
		return nil, err
	}

	var user User
	if err := c.db.WithContext(ctx).Where("id = ?", session.UserID).First(&user).Error; err != nil {
		return nil, err
	}

	_ = c.db.WithContext(ctx).Model(&session).Updates(map[string]any{
		"last_seen_at": now,
		"updated_at":   now,
	}).Error
	return &user, nil
}

// RevokeUserSession deletes a server-side session by raw cookie token.
func (c *Cache) RevokeUserSession(ctx context.Context, rawToken string) error {
	rawToken = strings.TrimSpace(rawToken)
	if rawToken == "" {
		return nil
	}
	return c.db.WithContext(ctx).Where("token_hash = ?", hashSessionToken(rawToken)).Delete(&UserSession{}).Error
}

// FindOrCreateUser returns an existing account or creates a new free account.
func (c *Cache) FindOrCreateUser(ctx context.Context, email, name, pictureURL string, emailVerified bool, authProvider string, now time.Time) (*User, error) {
	email, err := NormalizeEmail(email)
	if err != nil {
		return nil, err
	}
	name = trimForStorage(name, 200)
	pictureURL = trimForStorage(pictureURL, 1024)
	authProvider = trimForStorage(authProvider, 64)
	if authProvider == "" {
		authProvider = "email"
	}

	var user User
	err = c.db.WithContext(ctx).Where("email = ?", email).First(&user).Error
	if err == nil {
		user.LastLoginAt = &now
		if user.Plan == "" {
			user.Plan = "free"
		}
		if name != "" {
			user.Name = name
		}
		if pictureURL != "" {
			user.PictureURL = pictureURL
		}
		if emailVerified {
			user.EmailVerified = true
		}
		user.AuthProvider = authProvider
		if err := c.db.WithContext(ctx).Save(&user).Error; err != nil {
			return nil, fmt.Errorf("update user login: %w", err)
		}
		return &user, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("find user: %w", err)
	}

	user = User{
		ID:            "usr_" + mustRandomToken(18),
		Email:         email,
		Name:          name,
		PictureURL:    pictureURL,
		EmailVerified: emailVerified,
		AuthProvider:  authProvider,
		Plan:          "free",
		LastLoginAt:   &now,
	}
	if err := c.db.WithContext(ctx).Create(&user).Error; err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}
	return &user, nil
}

func hashLoginCode(salt, code string) string {
	sum := sha256.Sum256([]byte(salt + "\x00" + code))
	return hex.EncodeToString(sum[:])
}

func hashSessionToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func randomDigits(n int) (string, error) {
	var b strings.Builder
	for i := 0; i < n; i++ {
		v, err := rand.Int(rand.Reader, big.NewInt(10))
		if err != nil {
			return "", err
		}
		b.WriteByte(byte('0' + v.Int64()))
	}
	return b.String(), nil
}

func randomToken(n int) (string, error) {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func mustRandomToken(n int) string {
	token, err := randomToken(n)
	if err != nil {
		panic(err)
	}
	return token
}

func trimForStorage(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n]
}
