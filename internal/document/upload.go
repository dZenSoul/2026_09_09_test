package document

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"path/filepath"
	"strings"
	"unicode"

	"documents/internal/blob"
	"documents/internal/domain"
	"documents/internal/repository"

	"github.com/google/uuid"
)

const storageKeyBytes = 32

type Config struct {
	MaxFileBytes  int64
	MaxGrantItems int
	MaxListLimit  int
	Rand          io.Reader
}

type service struct {
	documents repository.DocumentRepository
	users     repository.UserRepository
	blobs     blob.Storage
	maxFile   int64
	maxGrants int
	maxList   int
	rand      io.Reader
}

func NewService(documents repository.DocumentRepository, users repository.UserRepository, blobs blob.Storage, cfg Config) (Service, error) {
	if documents == nil || users == nil || blobs == nil || cfg.MaxFileBytes <= 0 || cfg.MaxGrantItems <= 0 {
		return nil, fmt.Errorf("configure document service: %w", domain.ErrInvalidArgument)
	}
	random := cfg.Rand
	if random == nil {
		random = rand.Reader
	}
	maxList := cfg.MaxListLimit
	if maxList <= 0 {
		maxList = 100
	}
	return &service{
		documents: documents, users: users, blobs: blobs,
		maxFile: cfg.MaxFileBytes, maxGrants: cfg.MaxGrantItems, maxList: maxList, rand: random,
	}, nil
}

func (s *service) Upload(ctx context.Context, requester domain.User, input Upload) (domain.Document, error) {
	document := input.Document
	if requester.ID == "" || requester.Login == "" || !validName(document.Name) {
		return domain.Document{}, domain.ErrInvalidArgument
	}
	if len(document.Grants) > s.maxGrants {
		return domain.Document{}, domain.ErrInvalidArgument
	}
	if document.JSON != nil && !json.Valid(document.JSON) {
		return domain.Document{}, domain.ErrInvalidArgument
	}

	if document.IsFile {
		if input.File == nil || document.MIME == "" {
			return domain.Document{}, domain.ErrInvalidArgument
		}
	} else {
		if input.File != nil || document.JSON == nil {
			return domain.Document{}, domain.ErrInvalidArgument
		}
	}
	if document.MIME != "" {
		normalized, err := normalizeMIME(document.MIME)
		if err != nil {
			return domain.Document{}, domain.ErrInvalidArgument
		}
		document.MIME = normalized
	}

	grants := uniqueGrants(document.Grants, requester.Login)
	existing, err := s.users.ExistingLogins(ctx, grants)
	if err != nil {
		return domain.Document{}, fmt.Errorf("validate document grants: %w", err)
	}
	if !sameLoginSet(grants, existing) {
		return domain.Document{}, domain.ErrInvalidArgument
	}

	id, err := uuid.NewRandomFromReader(s.rand)
	if err != nil {
		return domain.Document{}, fmt.Errorf("generate document id: %w", err)
	}
	document.ID = id.String()
	document.OwnerID = requester.ID
	document.Grants = grants
	document.StorageKey = ""
	document.SizeBytes = 0

	if document.IsFile {
		key, err := randomStorageKey(s.rand)
		if err != nil {
			return domain.Document{}, err
		}
		document.StorageKey = key
		limited := &io.LimitedReader{R: input.File, N: s.maxFile + 1}
		size, putErr := s.blobs.Put(ctx, key, limited)
		if putErr != nil {
			return domain.Document{}, fmt.Errorf("store document blob: %w", putErr)
		}
		if size > s.maxFile {
			cleanupErr := s.blobs.Delete(context.WithoutCancel(ctx), key)
			if cleanupErr != nil && !errors.Is(cleanupErr, blob.ErrNotFound) {
				return domain.Document{}, fmt.Errorf("reject oversized blob and clean up: %w", errors.Join(domain.ErrInvalidArgument, cleanupErr))
			}
			return domain.Document{}, domain.ErrInvalidArgument
		}
		document.SizeBytes = size
	}

	created, err := s.documents.Create(ctx, document)
	if err == nil {
		return created, nil
	}
	if document.IsFile {
		cleanupErr := s.blobs.Delete(context.WithoutCancel(ctx), document.StorageKey)
		if cleanupErr != nil && !errors.Is(cleanupErr, blob.ErrNotFound) {
			return domain.Document{}, fmt.Errorf("create document and clean up blob: %w", errors.Join(mapRepositoryError(err), cleanupErr))
		}
	}
	return domain.Document{}, mapRepositoryError(err)
}

func validName(name string) bool {
	if strings.TrimSpace(name) == "" || name == "." || name == ".." || strings.Contains(name, "..") || filepath.Base(name) != name || strings.ContainsAny(name, `/\\`) {
		return false
	}
	for _, char := range name {
		if unicode.IsControl(char) {
			return false
		}
	}
	return true
}

func normalizeMIME(value string) (string, error) {
	mediaType, parameters, err := mime.ParseMediaType(value)
	if err != nil || mediaType == "" || !strings.Contains(mediaType, "/") {
		return "", domain.ErrInvalidArgument
	}
	return mime.FormatMediaType(mediaType, parameters), nil
}

func uniqueGrants(input []string, owner string) []string {
	seen := make(map[string]struct{}, len(input))
	result := make([]string, 0, len(input))
	for _, login := range input {
		if login == owner {
			continue
		}
		if _, found := seen[login]; found {
			continue
		}
		seen[login] = struct{}{}
		result = append(result, login)
	}
	return result
}

func sameLoginSet(want, got []string) bool {
	if len(want) != len(got) {
		return false
	}
	set := make(map[string]struct{}, len(got))
	for _, login := range got {
		set[login] = struct{}{}
	}
	for _, login := range want {
		if _, found := set[login]; !found {
			return false
		}
	}
	return true
}

func randomStorageKey(random io.Reader) (string, error) {
	value := make([]byte, storageKeyBytes)
	if _, err := io.ReadFull(random, value); err != nil {
		return "", fmt.Errorf("generate document storage key: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func mapRepositoryError(err error) error {
	switch {
	case errors.Is(err, repository.ErrInvalidArgument), errors.Is(err, repository.ErrConflict):
		return domain.ErrInvalidArgument
	case errors.Is(err, repository.ErrNotFound):
		return domain.ErrNotFound
	case errors.Is(err, repository.ErrForbidden):
		return domain.ErrForbidden
	default:
		return fmt.Errorf("document repository: %w", err)
	}
}

var _ Service = (*service)(nil)
