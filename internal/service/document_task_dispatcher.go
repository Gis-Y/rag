package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"pai-smart-go/internal/model"
	"pai-smart-go/pkg/log"
	"pai-smart-go/pkg/tasks"
)

type TaskProducer interface {
	ProduceFileTask(context.Context, tasks.FileProcessingTask) error
}

type DocumentTaskOutboxStore interface {
	Claim(context.Context, string, time.Time) (*model.DocumentTaskOutbox, *model.FileUpload, error)
	Acknowledge(context.Context, uint, uint, string) error
	Retry(context.Context, uint, uint, string, string, time.Time) error
}

// RunDocumentTaskDispatcher delivers durable jobs independently of upload requests.
// ponytail: delivery is at least once; a lost broker acknowledgement can reprocess
// a document, while versioned publication still keeps incomplete attempts hidden.
func RunDocumentTaskDispatcher(ctx context.Context, outbox DocumentTaskOutboxStore, producer TaskProducer) {
	if outbox == nil || producer == nil {
		panic("document task dispatcher requires outbox and producer")
	}
	for ctx.Err() == nil {
		delivered, err := dispatchDocumentTask(ctx, outbox, producer)
		if err != nil && ctx.Err() == nil {
			log.Errorf("document outbox delivery failed: %v", err)
		}
		if delivered && err == nil {
			continue
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func dispatchDocumentTask(ctx context.Context, outbox DocumentTaskOutboxStore, producer TaskProducer) (bool, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return false, err
	}
	claimCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	entry, file, err := outbox.Claim(claimCtx, hex.EncodeToString(random[:]), time.Now())
	cancel()
	if err != nil || entry == nil {
		return false, err
	}
	if file == nil || file.Status != 1 || file.ID != entry.DocumentID || file.UserID != entry.UserID {
		return false, fmt.Errorf("outbox returned an invalid document identity")
	}
	publishCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err = producer.ProduceFileTask(publishCtx, tasks.FileProcessingTask{
		DocumentID: file.ID, FileMD5: file.FileMD5, FileName: file.FileName,
		UserID: file.UserID, OrgTag: file.OrgTag, IsPublic: file.IsPublic,
	})
	cancel()
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err != nil {
		delay := time.Second * time.Duration(1<<min(max(entry.Attempts, 1), 6))
		retryErr := outbox.Retry(finishCtx, entry.DocumentID, entry.UserID, entry.LeaseToken, err.Error(), time.Now().Add(min(delay, time.Minute)))
		if retryErr != nil {
			return false, fmt.Errorf("delivery failed: %v; recording retry: %w", err, retryErr)
		}
		return false, err
	}
	return true, outbox.Acknowledge(finishCtx, entry.DocumentID, entry.UserID, entry.LeaseToken)
}
