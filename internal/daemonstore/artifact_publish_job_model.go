package daemonstore

import "time"

type ArtifactPublishJob struct {
	PublicationID   string    `gorm:"primaryKey;type:text"`
	AgentID         string    `gorm:"type:text;not null"`
	SessionID       string    `gorm:"type:text;not null"`
	SourceFilename  string    `gorm:"type:text;not null"`
	DisplayTitle    string    `gorm:"type:text;not null;default:''"`
	ContentType     string    `gorm:"type:text;not null;default:''"`
	SpoolPath       string    `gorm:"type:text;not null"`
	SizeBytes       int64     `gorm:"not null;default:0"`
	SHA256          string    `gorm:"type:text;not null;default:''"`
	Status          string    `gorm:"type:text;not null;index:idx_artifact_publish_jobs_status_created,priority:1"`
	ArtifactID      string    `gorm:"type:text;not null;default:''"`
	UploadID        string    `gorm:"type:text;not null;default:''"`
	ResumableURL    string    `gorm:"type:text;not null;default:''"`
	UploadedBytes   int64     `gorm:"not null;default:0"`
	TicketExpiresAt string    `gorm:"type:text;not null;default:''"`
	ErrorCode       string    `gorm:"type:text;not null;default:''"`
	ErrorMessage    string    `gorm:"type:text;not null;default:''"`
	CreatedAt       time.Time `gorm:"column:created_at;not null;index:idx_artifact_publish_jobs_status_created,priority:2"`
	UpdatedAt       time.Time `gorm:"column:updated_at;not null"`
}

func (ArtifactPublishJob) TableName() string { return "artifact_publish_jobs" }
