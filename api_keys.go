package arxiv

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

const apiKeyPrefix = "arxivgg_"

// ActiveUserAPIKey returns the most recent active API key metadata for a user.
// It never creates or exposes a raw key.
func (c *Cache) ActiveUserAPIKey(ctx context.Context, userID, name string) (*UserAPIKey, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("user ID is required")
	}
	name = normalizeAPIKeyName(name)

	var key UserAPIKey
	err := c.db.WithContext(ctx).
		Where("user_id = ? AND name = ? AND revoked_at IS NULL", userID, name).
		Order("created_at DESC").
		First(&key).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load api key: %w", err)
	}
	return &key, nil
}

// EnsureUserAPIKey returns the most recent active API key metadata for a user.
// If none exists, it creates one and returns the raw key exactly once.
func (c *Cache) EnsureUserAPIKey(ctx context.Context, userID, name string) (*UserAPIKey, string, bool, error) {
	c.apiKeyMu.Lock()
	defer c.apiKeyMu.Unlock()
	return c.ensureUserAPIKey(ctx, userID, name)
}

func (c *Cache) ensureUserAPIKey(ctx context.Context, userID, name string) (*UserAPIKey, string, bool, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, "", false, fmt.Errorf("user ID is required")
	}
	name = normalizeAPIKeyName(name)

	var key UserAPIKey
	err := c.db.WithContext(ctx).
		Where("user_id = ? AND name = ? AND revoked_at IS NULL", userID, name).
		Order("created_at DESC").
		First(&key).Error
	if err == nil {
		return &key, "", false, nil
	}
	if err != nil && err != gorm.ErrRecordNotFound {
		return nil, "", false, fmt.Errorf("load api key: %w", err)
	}

	created, raw, err := c.CreateUserAPIKey(ctx, userID, name)
	if err != nil {
		if loadErr := c.db.WithContext(ctx).
			Where("user_id = ? AND name = ? AND revoked_at IS NULL", userID, name).
			Order("created_at DESC").First(&key).Error; loadErr == nil {
			return &key, "", false, nil
		}
		return nil, "", false, err
	}
	return created, raw, true, nil
}

// CreateUserAPIKey creates a new active API key and returns the raw key once.
func (c *Cache) CreateUserAPIKey(ctx context.Context, userID, name string) (*UserAPIKey, string, error) {
	return c.createUserAPIKey(ctx, c.db.WithContext(ctx), userID, name)
}

// RegenerateUserAPIKey revokes active keys for a name and creates a new one.
func (c *Cache) RegenerateUserAPIKey(ctx context.Context, userID, name string) (*UserAPIKey, string, error) {
	c.apiKeyMu.Lock()
	defer c.apiKeyMu.Unlock()
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, "", fmt.Errorf("user ID is required")
	}
	name = normalizeAPIKeyName(name)

	var created *UserAPIKey
	var raw string
	err := c.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if c.dbType == DBTypePostgres {
			if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtextextended(?, 0))", userID+"\x00"+name).Error; err != nil {
				return fmt.Errorf("lock api key rotation: %w", err)
			}
		}
		now := time.Now().UTC()
		if err := tx.Model(&UserAPIKey{}).
			Where("user_id = ? AND name = ? AND revoked_at IS NULL", userID, name).
			Updates(map[string]any{
				"revoked_at": now,
				"updated_at": now,
			}).Error; err != nil {
			return fmt.Errorf("revoke api keys: %w", err)
		}

		var err error
		created, raw, err = c.createUserAPIKey(ctx, tx, userID, name)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	return created, raw, nil
}

// UserForAPIKey returns the user associated with an active raw API key.
func (c *Cache) UserForAPIKey(ctx context.Context, rawKey string) (*User, *UserAPIKey, error) {
	rawKey = strings.TrimSpace(rawKey)
	if rawKey == "" {
		return nil, nil, gorm.ErrRecordNotFound
	}

	var key UserAPIKey
	result := c.db.WithContext(ctx).
		Where("key_hash = ? AND revoked_at IS NULL", hashAPIKey(rawKey)).
		Limit(1).
		Find(&key)
	if result.Error != nil {
		return nil, nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, nil, gorm.ErrRecordNotFound
	}

	var user User
	if err := c.db.WithContext(ctx).Where("id = ?", key.UserID).First(&user).Error; err != nil {
		return nil, nil, err
	}

	now := time.Now().UTC()
	_ = c.db.WithContext(ctx).Model(&key).Updates(map[string]any{
		"last_used_at": now,
		"updated_at":   now,
	}).Error
	key.LastUsedAt = &now
	key.UpdatedAt = now
	return &user, &key, nil
}

// MaskAPIKey returns a display-safe key preview.
func MaskAPIKey(key *UserAPIKey) string {
	if key == nil {
		return ""
	}
	if key.Prefix == "" && key.LastFour == "" {
		return ""
	}
	if key.LastFour == "" {
		return key.Prefix + "..."
	}
	return key.Prefix + "..." + key.LastFour
}

func (c *Cache) createUserAPIKey(ctx context.Context, db *gorm.DB, userID, name string) (*UserAPIKey, string, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, "", fmt.Errorf("user ID is required")
	}
	name = normalizeAPIKeyName(name)

	raw, err := randomAPIKey()
	if err != nil {
		return nil, "", err
	}
	key := &UserAPIKey{
		ID:       "key_" + mustRandomToken(18),
		UserID:   userID,
		Name:     name,
		KeyHash:  hashAPIKey(raw),
		Prefix:   apiKeyDisplayPrefix(raw),
		LastFour: apiKeyLastFour(raw),
	}
	if err := db.WithContext(ctx).Create(key).Error; err != nil {
		return nil, "", fmt.Errorf("create api key: %w", err)
	}
	return key, raw, nil
}

func normalizeAPIKeyName(name string) string {
	name = trimForStorage(name, 120)
	if name == "" {
		return "Agent access"
	}
	return name
}

func randomAPIKey() (string, error) {
	token, err := randomToken(32)
	if err != nil {
		return "", err
	}
	return apiKeyPrefix + token, nil
}

func hashAPIKey(rawKey string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(rawKey)))
	return hex.EncodeToString(sum[:])
}

func apiKeyDisplayPrefix(rawKey string) string {
	const n = len(apiKeyPrefix) + 6
	if len(rawKey) <= n {
		return rawKey
	}
	return rawKey[:n]
}

func apiKeyLastFour(rawKey string) string {
	if len(rawKey) <= 4 {
		return rawKey
	}
	return rawKey[len(rawKey)-4:]
}
