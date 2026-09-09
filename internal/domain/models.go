// Package domain contains transport- and persistence-independent data types.
package domain

import (
	"encoding/json"
	"time"
)

type User struct {
	ID        string
	Login     string
	CreatedAt time.Time
}

type Session struct {
	ID        string
	UserID    string
	TokenHash []byte
	CreatedAt time.Time
	ExpiresAt time.Time
	RevokedAt *time.Time
}

type Document struct {
	ID         string
	OwnerID    string
	Name       string
	MIME       string
	IsFile     bool
	IsPublic   bool
	JSON       json.RawMessage
	StorageKey string
	SizeBytes  int64
	CreatedAt  time.Time
	UpdatedAt  time.Time
	Version    int64
	Grants     []string
}

type DocumentFilter struct {
	OwnerLogin string
	Key        string
	Value      string
	Limit      int
}
