package internal

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	ErrNotFound        = errors.New("not found")
	ErrInvalidPassword = errors.New("invalid password")
)

type Store struct {
	DataDir string
	TTL     time.Duration
}

type Metadata struct {
	ID           string    `json:"id"`
	PasswordHash string    `json:"password_hash,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
	DataPolicy   string    `json:"data_policy,omitempty"`
	Size         int64     `json:"size"`
	ContentType  string    `json:"content_type"`
}

type Entry struct {
	Meta Metadata
	File *os.File
}

func NewStore(dataDir string, ttl time.Duration) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}

	return &Store{
		DataDir: dataDir,
		TTL:     ttl,
	}, nil
}

func (s *Store) Create(r io.Reader, contentType string, usePassword bool, permanent bool) (Metadata, string, error) {
	id, path, err := s.reservePath()
	if err != nil {
		return Metadata{}, "", err
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return Metadata{}, "", err
	}

	size, copyErr := io.Copy(file, r)
	closeErr := file.Close()

	if copyErr != nil {
		_ = os.Remove(path)
		return Metadata{}, "", copyErr
	}

	if closeErr != nil {
		_ = os.Remove(path)
		return Metadata{}, "", closeErr
	}

	var password string
	var passwordHash string

	if usePassword {
		password, err = generatePassword(8)
		if err != nil {
			_ = os.Remove(path)
			return Metadata{}, "", err
		}
		passwordHash = hashPassword(password)
	}

	now := time.Now().UTC()

	dataPolicy := "temporary"
	expiresAt := now.Add(s.TTL)

	if permanent {
		dataPolicy = "permanent"
		expiresAt = time.Time{}
	}

	meta := Metadata{
		ID:           id,
		PasswordHash: passwordHash,
		CreatedAt:    now,
		ExpiresAt:    expiresAt,
		DataPolicy:   dataPolicy,
		Size:         size,
		ContentType:  contentType,
	}

	if err := s.writeMetadata(meta); err != nil {
		_ = os.Remove(path)
		return Metadata{}, "", err
	}

	return meta, password, nil
}

func (s *Store) Open(id string, password string) (*Entry, error) {
	if !validID(id) {
		return nil, ErrNotFound
	}

	path := s.path(id)

	meta, err := s.readMetadata(id)
	if err != nil {
		return nil, ErrNotFound
	}

	if isExpired(meta, time.Now().UTC()) {
		_ = os.Remove(path)
		_ = os.Remove(metaPath(path))
		return nil, ErrNotFound
	}

	if meta.PasswordHash != "" {
		if strings.TrimSpace(password) == "" || hashPassword(password) != meta.PasswordHash {
			return nil, ErrInvalidPassword
		}
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, ErrNotFound
	}

	return &Entry{
		Meta: meta,
		File: file,
	}, nil
}

func (s *Store) CleanupExpired() error {
	entries, err := os.ReadDir(s.DataDir)
	if err != nil {
		return err
	}

	now := time.Now().UTC()

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()

		if strings.HasSuffix(name, ".json") {
			continue
		}

		if !validID(name) {
			continue
		}

		meta, err := s.readMetadata(name)
		if err != nil {
			continue
		}

		if isExpired(meta, now) {
			path := s.path(name)
			_ = os.Remove(path)
			_ = os.Remove(metaPath(path))
		}
	}

	return nil
}

func (s *Store) reservePath() (string, string, error) {
	for i := 0; i < 100; i++ {
		id, err := randomString(idAlphabet, 5)
		if err != nil {
			return "", "", err
		}

		path := s.path(id)

		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return id, path, nil
		}
	}

	return "", "", errors.New("failed to reserve random id")
}

func (s *Store) path(id string) string {
	return filepath.Join(s.DataDir, id)
}

func (s *Store) writeMetadata(meta Metadata) error {
	path := s.path(meta.ID)
	metaFile := metaPath(path)

	tmp := metaFile + ".tmp"

	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}

	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}

	return os.Rename(tmp, metaFile)
}

func (s *Store) readMetadata(id string) (Metadata, error) {
	var meta Metadata

	data, err := os.ReadFile(metaPath(s.path(id)))
	if err != nil {
		return meta, err
	}

	if err := json.Unmarshal(data, &meta); err != nil {
		return meta, err
	}

	return meta, nil
}

func isExpired(meta Metadata, now time.Time) bool {
	if strings.EqualFold(meta.DataPolicy, "permanent") {
		return false
	}

	if meta.ExpiresAt.IsZero() {
		return false
	}

	return now.After(meta.ExpiresAt)
}

func validID(id string) bool {
	if len(id) != 5 {
		return false
	}

	for _, r := range id {
		if r >= 'a' && r <= 'z' {
			continue
		}
		if r >= 'A' && r <= 'Z' {
			continue
		}
		if r >= '0' && r <= '9' {
			continue
		}
		return false
	}

	return true
}

func hashPassword(password string) string {
	sum := sha256.Sum256([]byte(password))
	return hex.EncodeToString(sum[:])
}

func generatePassword(length int) (string, error) {
	if length < 4 {
		length = 8
	}

	upper := "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	lower := "abcdefghijklmnopqrstuvwxyz"
	digits := "0123456789"
	special := "!@#$%^&*_-+=?{}[]"
	all := upper + lower + digits + special

	result := make([]byte, 0, length)

	a, err := randomChar(upper)
	if err != nil {
		return "", err
	}
	result = append(result, a)

	a, err = randomChar(lower)
	if err != nil {
		return "", err
	}
	result = append(result, a)

	a, err = randomChar(digits)
	if err != nil {
		return "", err
	}
	result = append(result, a)

	a, err = randomChar(special)
	if err != nil {
		return "", err
	}
	result = append(result, a)

	for len(result) < length {
		a, err = randomChar(all)
		if err != nil {
			return "", err
		}
		result = append(result, a)
	}

	if err := shuffleBytes(result); err != nil {
		return "", err
	}

	return string(result), nil
}

func randomChar(alphabet string) (byte, error) {
	n, err := randomIndex(len(alphabet))
	if err != nil {
		return 0, err
	}

	return alphabet[n], nil
}

func randomString(alphabet string, length int) (string, error) {
	result := make([]byte, length)

	for i := range result {
		ch, err := randomChar(alphabet)
		if err != nil {
			return "", err
		}
		result[i] = ch
	}

	return string(result), nil
}

func randomIndex(max int) (int, error) {
	if max <= 0 {
		return 0, errors.New("invalid max")
	}

	var b [1]byte

	for {
		if _, err := rand.Read(b[:]); err != nil {
			return 0, err
		}

		limit := 256 - (256 % max)
		if int(b[0]) < limit {
			return int(b[0]) % max, nil
		}
	}
}

func shuffleBytes(data []byte) error {
	for i := len(data) - 1; i > 0; i-- {
		j, err := randomIndex(i + 1)
		if err != nil {
			return err
		}

		data[i], data[j] = data[j], data[i]
	}

	return nil
}

const idAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
