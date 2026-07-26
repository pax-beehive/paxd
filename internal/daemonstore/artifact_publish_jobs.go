package daemonstore

import (
	"context"
	"errors"

	"gorm.io/gorm"
)

func (s *Store) CreateArtifactPublishJob(
	ctx context.Context,
	job ArtifactPublishJob,
) error {
	return s.db.WithContext(ctx).Create(&job).Error
}

func (s *Store) GetArtifactPublishJob(
	ctx context.Context,
	publicationID string,
) (ArtifactPublishJob, error) {
	var job ArtifactPublishJob
	err := s.db.WithContext(ctx).
		Where("publication_id = ?", publicationID).
		First(&job).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ArtifactPublishJob{}, ErrNotFound
	}
	return job, err
}

func (s *Store) ListUnfinishedArtifactPublishJobs(
	ctx context.Context,
) ([]ArtifactPublishJob, error) {
	var jobs []ArtifactPublishJob
	err := s.db.WithContext(ctx).
		Where("status NOT IN ?", []string{"available", "failed"}).
		Order("created_at ASC").
		Find(&jobs).Error
	return jobs, err
}
