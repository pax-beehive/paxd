package daemonstore

import "context"

func (s *Store) SaveArtifactPublishJob(
	ctx context.Context,
	job ArtifactPublishJob,
) error {
	result := s.db.WithContext(ctx).
		Model(&ArtifactPublishJob{}).
		Where("publication_id = ?", job.PublicationID).
		Updates(map[string]any{
			"remote_id":         job.RemoteID,
			"content_type":      job.ContentType,
			"size_bytes":        job.SizeBytes,
			"sha256":            job.SHA256,
			"status":            job.Status,
			"artifact_id":       job.ArtifactID,
			"upload_id":         job.UploadID,
			"resumable_url":     job.ResumableURL,
			"uploaded_bytes":    job.UploadedBytes,
			"ticket_expires_at": job.TicketExpiresAt,
			"error_code":        job.ErrorCode,
			"error_message":     job.ErrorMessage,
			"updated_at":        job.UpdatedAt,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}
