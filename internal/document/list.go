package document

import (
	"context"
	"strconv"
	"time"

	"documents/internal/domain"

	"github.com/google/uuid"
)

const documentTimeFormat = "2006-01-02 15:04:05"

func (s *service) List(ctx context.Context, requester domain.User, filter domain.DocumentFilter) ([]domain.Document, error) {
	if requester.ID == "" || requester.Login == "" || filter.Limit < 0 || filter.Limit > s.maxList {
		return nil, domain.ErrInvalidArgument
	}
	if err := validateDocumentFilter(filter.Key, filter.Value); err != nil {
		return nil, err
	}

	documents, err := s.documents.List(ctx, requester.ID, filter)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	if documents == nil {
		documents = []domain.Document{}
	}
	for index := range documents {
		documents[index].CreatedAt = documents[index].CreatedAt.UTC()
		if documents[index].Grants == nil {
			documents[index].Grants = []string{}
		}
	}
	return documents, nil
}

func validateDocumentFilter(key, value string) error {
	if key == "" {
		if value != "" {
			return domain.ErrInvalidArgument
		}
		return nil
	}
	if value == "" {
		return domain.ErrInvalidArgument
	}

	switch key {
	case "id":
		if _, err := uuid.Parse(value); err != nil {
			return domain.ErrInvalidArgument
		}
	case "name", "mime":
		return nil
	case "file", "public":
		if _, err := strconv.ParseBool(value); err != nil || (value != "true" && value != "false") {
			return domain.ErrInvalidArgument
		}
	case "created":
		if _, err := time.ParseInLocation(documentTimeFormat, value, time.UTC); err != nil {
			if _, err = time.Parse(time.RFC3339, value); err != nil {
				return domain.ErrInvalidArgument
			}
		}
	default:
		return domain.ErrInvalidArgument
	}
	return nil
}
