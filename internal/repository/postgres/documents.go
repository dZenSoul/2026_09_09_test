package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"documents/internal/domain"
	"documents/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const documentColumns = `
    d.id::text, d.owner_id::text, d.name, d.mime, d.is_file, d.is_public,
    d.json_data, d.storage_key, d.size_bytes, d.created_at, d.version,
    COALESCE(array_agg(gu.login ORDER BY gu.login)
        FILTER (WHERE gu.login IS NOT NULL), ARRAY[]::text[])`

const documentJoins = `
    FROM documents d
    LEFT JOIN document_grants dg ON dg.document_id = d.id
    LEFT JOIN users gu ON gu.id = dg.user_id`

func (s *Store) CreateDocument(ctx context.Context, input domain.Document) (domain.Document, error) {
	if _, err := uuid.Parse(input.OwnerID); err != nil || strings.TrimSpace(input.Name) == "" {
		return domain.Document{}, fmt.Errorf("create document: %w", repository.ErrInvalidArgument)
	}
	if input.ID == "" {
		input.ID = uuid.NewString()
	} else if _, err := uuid.Parse(input.ID); err != nil {
		return domain.Document{}, fmt.Errorf("create document: %w", repository.ErrInvalidArgument)
	}
	if input.IsFile && (input.MIME == "" || input.StorageKey == "" || input.SizeBytes < 0) {
		return domain.Document{}, fmt.Errorf("create document: %w", repository.ErrInvalidArgument)
	}
	if !input.IsFile && input.StorageKey != "" {
		return domain.Document{}, fmt.Errorf("create document: %w", repository.ErrInvalidArgument)
	}
	if input.JSON != nil && !json.Valid(input.JSON) {
		return domain.Document{}, fmt.Errorf("create document: %w", repository.ErrInvalidArgument)
	}

	var result domain.Document
	err := s.WithinTransaction(ctx, func(txCtx context.Context) error {
		db := s.executor(txCtx)
		var ownerLogin string
		if err := db.QueryRow(txCtx, "SELECT login FROM users WHERE id = $1", input.OwnerID).Scan(&ownerLogin); err != nil {
			return classify("find document owner", err)
		}

		grants := uniqueNonOwnerLogins(input.Grants, ownerLogin)
		row := db.QueryRow(txCtx, `
            INSERT INTO documents (
                id, owner_id, name, mime, is_file, is_public, json_data,
                storage_key, size_bytes, version
            ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 1)
            RETURNING id::text, owner_id::text, name, mime, is_file, is_public,
                      json_data, storage_key, size_bytes, created_at, version`,
			input.ID, input.OwnerID, input.Name, nullableString(input.MIME),
			input.IsFile, input.IsPublic, nullableJSON(input.JSON),
			fileStorageKey(input), fileSize(input),
		)
		if err := scanDocumentCore(row, &result); err != nil {
			return classify("create document", err)
		}

		if len(grants) != 0 {
			tag, err := db.Exec(txCtx, `
                INSERT INTO document_grants (document_id, user_id)
                SELECT $1, id FROM users WHERE login = ANY($2::text[])
                ON CONFLICT (document_id, user_id) DO NOTHING`, result.ID, grants)
			if err != nil {
				return classify("create document grants", err)
			}
			if tag.RowsAffected() != int64(len(grants)) {
				return fmt.Errorf("create document grants: %w", repository.ErrNotFound)
			}
		}
		result.Grants = grants
		return nil
	})
	if err != nil {
		return domain.Document{}, err
	}
	return result, nil
}

func (s *Store) ByID(ctx context.Context, id string) (domain.Document, error) {
	if _, err := uuid.Parse(id); err != nil {
		return domain.Document{}, fmt.Errorf("find document: %w", repository.ErrInvalidArgument)
	}
	query := "SELECT " + documentColumns + documentJoins + `
        WHERE d.id = $1
        GROUP BY d.id`
	return scanFullDocument(s.executor(ctx).QueryRow(ctx, query, id), "find document")
}

func (s *Store) ByIDAccessible(ctx context.Context, id, requesterID string) (domain.Document, error) {
	if _, err := uuid.Parse(id); err != nil {
		return domain.Document{}, fmt.Errorf("find accessible document: %w", repository.ErrInvalidArgument)
	}
	if _, err := uuid.Parse(requesterID); err != nil {
		return domain.Document{}, fmt.Errorf("find accessible document: %w", repository.ErrInvalidArgument)
	}
	query := "SELECT " + documentColumns + `,
        (d.owner_id = $2 OR d.is_public OR EXISTS (
            SELECT 1 FROM document_grants access_grant
            WHERE access_grant.document_id = d.id AND access_grant.user_id = $2
        )) AS accessible` + documentJoins + `
        WHERE d.id = $1
        GROUP BY d.id`
	var document domain.Document
	var accessible bool
	if err := scanDocumentWithAccess(s.executor(ctx).QueryRow(ctx, query, id, requesterID), &document, &accessible); err != nil {
		return domain.Document{}, classify("find accessible document", err)
	}
	if !accessible {
		return domain.Document{}, fmt.Errorf("find accessible document: %w", repository.ErrForbidden)
	}
	return document, nil
}

func (s *Store) List(ctx context.Context, requesterID string, filter domain.DocumentFilter) ([]domain.Document, error) {
	if _, err := uuid.Parse(requesterID); err != nil {
		return nil, fmt.Errorf("list documents: %w", repository.ErrInvalidArgument)
	}
	if filter.Limit < 0 {
		return nil, fmt.Errorf("list documents: %w", repository.ErrInvalidArgument)
	}

	db := s.executor(ctx)
	ownerID := requesterID
	if filter.OwnerLogin != "" {
		if err := db.QueryRow(ctx, "SELECT id::text FROM users WHERE login = $1", filter.OwnerLogin).Scan(&ownerID); err != nil {
			return nil, classify("find list owner", err)
		}
	}

	args := []any{ownerID, requesterID}
	filterSQL, value, err := buildDocumentFilter(filter.Key, filter.Value)
	if err != nil {
		return nil, err
	}
	where := ` WHERE d.owner_id = $1
        AND ($1::uuid = $2::uuid OR d.is_public OR EXISTS (
            SELECT 1 FROM document_grants access_grant
            WHERE access_grant.document_id = d.id AND access_grant.user_id = $2
        ))`
	if filterSQL != "" {
		args = append(args, value)
		where += " AND " + filterSQL + fmt.Sprintf(" $%d", len(args))
	}
	query := "SELECT " + documentColumns + documentJoins + where + `
        GROUP BY d.id
        ORDER BY d.name ASC, d.created_at DESC, d.id ASC`
	if filter.Limit > 0 {
		args = append(args, filter.Limit)
		query += fmt.Sprintf(" LIMIT $%d", len(args))
	}

	rows, err := db.Query(ctx, query, args...)
	if err != nil {
		return nil, classify("list documents", err)
	}
	documents, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Document, error) {
		var document domain.Document
		err := scanDocumentFullRow(row, &document)
		return document, err
	})
	if err != nil {
		return nil, classify("read document list", err)
	}
	return documents, nil
}

func (s *Store) Delete(ctx context.Context, id, ownerID string) (domain.Document, error) {
	if _, err := uuid.Parse(id); err != nil {
		return domain.Document{}, fmt.Errorf("delete document: %w", repository.ErrInvalidArgument)
	}
	if _, err := uuid.Parse(ownerID); err != nil {
		return domain.Document{}, fmt.Errorf("delete document: %w", repository.ErrInvalidArgument)
	}
	var deleted domain.Document
	err := s.WithinTransaction(ctx, func(txCtx context.Context) error {
		var actualOwner string
		if err := s.executor(txCtx).QueryRow(txCtx,
			"SELECT owner_id::text FROM documents WHERE id = $1 FOR UPDATE", id).Scan(&actualOwner); err != nil {
			return classify("find document to delete", err)
		}
		if actualOwner != ownerID {
			return fmt.Errorf("delete document: %w", repository.ErrForbidden)
		}
		var err error
		deleted, err = s.ByID(txCtx, id)
		if err != nil {
			return err
		}
		if deleted.IsFile {
			if _, err := s.executor(txCtx).Exec(txCtx, `
                INSERT INTO blob_cleanup_tasks (storage_key)
                VALUES ($1)
                ON CONFLICT (storage_key) DO NOTHING`, deleted.StorageKey); err != nil {
				return classify("enqueue blob cleanup", err)
			}
		}
		tag, err := s.executor(txCtx).Exec(txCtx, "DELETE FROM documents WHERE id = $1", id)
		if err != nil {
			return classify("delete document", err)
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("delete document: %w", repository.ErrNotFound)
		}
		return nil
	})
	if err != nil {
		return domain.Document{}, err
	}
	return deleted, nil
}

func buildDocumentFilter(key, raw string) (string, any, error) {
	if key != "" && raw == "" {
		return "", nil, fmt.Errorf("document filter: %w", repository.ErrInvalidArgument)
	}
	switch key {
	case "":
		if raw != "" {
			return "", nil, fmt.Errorf("document filter: %w", repository.ErrInvalidArgument)
		}
		return "", nil, nil
	case "id":
		value, err := uuid.Parse(raw)
		if err != nil {
			return "", nil, fmt.Errorf("document filter: %w", repository.ErrInvalidArgument)
		}
		return "d.id =", value.String(), nil
	case "name":
		return "d.name =", raw, nil
	case "mime":
		return "d.mime =", raw, nil
	case "file", "public":
		if raw != "true" && raw != "false" {
			return "", nil, fmt.Errorf("document filter: %w", repository.ErrInvalidArgument)
		}
		value, _ := strconv.ParseBool(raw)
		column := "d.is_file ="
		if key == "public" {
			column = "d.is_public ="
		}
		return column, value, nil
	case "created":
		value, err := time.ParseInLocation("2006-01-02 15:04:05", raw, time.UTC)
		if err != nil {
			value, err = time.Parse(time.RFC3339, raw)
		}
		if err != nil {
			return "", nil, fmt.Errorf("document filter: %w", repository.ErrInvalidArgument)
		}
		// The public representation intentionally has second precision. Compare
		// at that same precision so a value copied from a list response matches
		// the document even when PostgreSQL stored fractional seconds.
		return "date_trunc('second', d.created_at) =", value.UTC().Truncate(time.Second), nil
	default:
		return "", nil, fmt.Errorf("document filter: %w", repository.ErrInvalidArgument)
	}
}

func scanFullDocument(row pgx.Row, operation string) (domain.Document, error) {
	var document domain.Document
	if err := scanDocumentFullRow(row, &document); err != nil {
		return domain.Document{}, classify(operation, err)
	}
	return document, nil
}

func scanDocumentFullRow(row pgx.Row, document *domain.Document) error {
	var mime, storageKey sql.NullString
	var size sql.NullInt64
	if err := row.Scan(
		&document.ID, &document.OwnerID, &document.Name, &mime,
		&document.IsFile, &document.IsPublic, &document.JSON, &storageKey,
		&size, &document.CreatedAt, &document.Version, &document.Grants,
	); err != nil {
		return err
	}
	finishDocumentScan(document, mime, storageKey, size)
	return nil
}

func scanDocumentWithAccess(row pgx.Row, document *domain.Document, accessible *bool) error {
	var mime, storageKey sql.NullString
	var size sql.NullInt64
	if err := row.Scan(
		&document.ID, &document.OwnerID, &document.Name, &mime,
		&document.IsFile, &document.IsPublic, &document.JSON, &storageKey,
		&size, &document.CreatedAt, &document.Version, &document.Grants, accessible,
	); err != nil {
		return err
	}
	finishDocumentScan(document, mime, storageKey, size)
	return nil
}

func scanDocumentCore(row pgx.Row, document *domain.Document) error {
	var mime, storageKey sql.NullString
	var size sql.NullInt64
	if err := row.Scan(
		&document.ID, &document.OwnerID, &document.Name, &mime,
		&document.IsFile, &document.IsPublic, &document.JSON, &storageKey,
		&size, &document.CreatedAt, &document.Version,
	); err != nil {
		return err
	}
	finishDocumentScan(document, mime, storageKey, size)
	return nil
}

func finishDocumentScan(document *domain.Document, mime, storageKey sql.NullString, size sql.NullInt64) {
	if mime.Valid {
		document.MIME = mime.String
	}
	if storageKey.Valid {
		document.StorageKey = storageKey.String
	}
	if size.Valid {
		document.SizeBytes = size.Int64
	}
	document.CreatedAt = document.CreatedAt.UTC()
	if document.Grants == nil {
		document.Grants = []string{}
	}
}

func uniqueNonOwnerLogins(logins []string, owner string) []string {
	seen := make(map[string]struct{}, len(logins))
	result := make([]string, 0, len(logins))
	for _, login := range logins {
		if login == "" || login == owner {
			continue
		}
		if _, exists := seen[login]; exists {
			continue
		}
		seen[login] = struct{}{}
		result = append(result, login)
	}
	return result
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableJSON(value json.RawMessage) any {
	if value == nil {
		return nil
	}
	return []byte(value)
}

func fileStorageKey(document domain.Document) any {
	if !document.IsFile {
		return nil
	}
	return document.StorageKey
}

func fileSize(document domain.Document) any {
	if !document.IsFile {
		return nil
	}
	return document.SizeBytes
}

type documentRepository struct{ *Store }

func (r documentRepository) Create(ctx context.Context, document domain.Document) (domain.Document, error) {
	return r.CreateDocument(ctx, document)
}

func (s *Store) Documents() repository.DocumentRepository { return documentRepository{s} }
