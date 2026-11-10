package snapshot

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	// ErrSnapshotWORMLocked is returned when attempting to delete or prune a WORM-locked snapshot
	ErrSnapshotWORMLocked = errors.New("cannot delete WORM-locked snapshot: retention policy or legal hold active")

	// ErrTamperedManifest is returned when a snapshot manifest cryptographic HMAC signature does not match
	ErrTamperedManifest = errors.New("security alert: manifest cryptographic HMAC mismatch - possible tampering or corruption")
)

// WORMMode defines the immutability enforcement level
type WORMMode string

const (
	// WORMModeCompliance enforces strict retention that cannot be shortened, disabled, or deleted until RetainUntil passes
	WORMModeCompliance WORMMode = "compliance"

	// WORMModeGovernance provides standard retention protection against accidental pruning
	WORMModeGovernance WORMMode = "governance"
)

// WORMMetadata defines enterprise Write Once Read Many (WORM) parameters
type WORMMetadata struct {
	Enabled       bool      `json:"enabled"`
	Mode          WORMMode  `json:"mode"`
	LockedAt      time.Time `json:"locked_at"`
	RetainUntil   time.Time `json:"retain_until"`
	RetentionDays int       `json:"retention_days"`
	LegalHold     bool      `json:"legal_hold"`
	ManifestHMAC  string    `json:"manifest_hmac"`
}

// CanonicalString generates the canonical representation of snapshot fields for HMAC signing
func (m *SnapshotManifest) CanonicalString() string {
	var lockedAtUnix, retainUntilUnix int64
	if !m.WORM.LockedAt.IsZero() {
		lockedAtUnix = m.WORM.LockedAt.Unix()
	}
	if !m.WORM.RetainUntil.IsZero() {
		retainUntilUnix = m.WORM.RetainUntil.Unix()
	}

	return fmt.Sprintf("%s|%d|%s|%d|%d|%d|%d|%t|%s|%t|%d",
		m.SnapshotID,
		m.CreatedAt.Unix(),
		m.NodeName,
		m.TotalFiles,
		m.TotalBytes,
		lockedAtUnix,
		retainUntilUnix,
		m.WORM.LegalHold,
		m.WORM.Mode,
		m.WORM.Enabled,
		m.WORM.RetentionDays,
	)
}

// ComputeHMAC generates a cryptographic HMAC-SHA256 signature over the canonical manifest fields
func (m *SnapshotManifest) ComputeHMAC(key string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(m.CanonicalString()))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyHMAC validates the manifest HMAC-SHA256 signature using constant-time comparison
func (m *SnapshotManifest) VerifyHMAC(key string) bool {
	if m.WORM.ManifestHMAC == "" {
		return false
	}
	expectedSig := m.ComputeHMAC(key)
	return hmac.Equal([]byte(strings.ToLower(m.WORM.ManifestHMAC)), []byte(strings.ToLower(expectedSig)))
}

// IsWORMLocked determines if a snapshot manifest is currently protected against deletion
func (m *SnapshotManifest) IsWORMLocked(now time.Time) bool {
	if !m.WORM.Enabled {
		return false
	}
	if m.WORM.LegalHold {
		return true
	}
	return now.Before(m.WORM.RetainUntil)
}

// IsWORMLocked on SnapshotSummary determines if a summary item is currently protected against deletion
func (s SnapshotSummary) IsWORMLocked(now time.Time) bool {
	if !s.WORMEnabled {
		return false
	}
	if s.WORMLegalHold {
		return true
	}
	return now.Before(s.WORMRetainUntil)
}

// CheckClockSkew evaluates the difference between local time and remote trusted time.
// If skew exceeds maxSkew, it returns true (anomaly detected) and the absolute skew duration.
func CheckClockSkew(localTime, remoteTime time.Time, maxSkew time.Duration) (bool, time.Duration) {
	skew := localTime.Sub(remoteTime)
	if skew < 0 {
		skew = -skew
	}
	if maxSkew <= 0 {
		maxSkew = 1 * time.Hour
	}
	return skew > maxSkew, skew
}
