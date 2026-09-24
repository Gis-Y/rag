// Package tasks defines the structure for tasks that are sent to Kafka.
package tasks

import "errors"

// FileProcessingTask represents the data structure for a file processing job.
type FileProcessingTask struct {
	DocumentID uint   `json:"document_id,omitempty"`
	FileMD5    string `json:"file_md5"`
	FileName   string `json:"file_name"`
	UserID     uint   `json:"user_id"`
	OrgTag     string `json:"org_tag"`
	IsPublic   bool   `json:"is_public"`
}

// PersistedProcessingFailure marks an error whose terminal outcome is already
// durable in SQL (including a task invalidated by an authoritative tombstone).
// Kafka may commit only errors carrying this marker.
type PersistedProcessingFailure struct{ Err error }

func (e *PersistedProcessingFailure) Error() string { return e.Err.Error() }
func (e *PersistedProcessingFailure) Unwrap() error { return e.Err }

func MarkProcessingFailurePersisted(err error) error {
	if err == nil || IsProcessingFailurePersisted(err) {
		return err
	}
	return &PersistedProcessingFailure{Err: err}
}

func IsProcessingFailurePersisted(err error) bool {
	var persisted *PersistedProcessingFailure
	return errors.As(err, &persisted)
}
