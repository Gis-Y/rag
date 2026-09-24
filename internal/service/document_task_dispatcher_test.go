package service

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"pai-smart-go/internal/model"
	"pai-smart-go/pkg/tasks"
)

type dispatcherOutbox struct {
	entry          model.DocumentTaskOutbox
	file           model.FileUpload
	acked, retried bool
	retryAt        time.Time
	ackErr         error
}

func (o *dispatcherOutbox) Claim(_ context.Context, token string, _ time.Time) (*model.DocumentTaskOutbox, *model.FileUpload, error) {
	o.entry.LeaseToken = token
	return &o.entry, &o.file, nil
}
func (o *dispatcherOutbox) Acknowledge(_ context.Context, id, owner uint, token string) error {
	o.acked = o.ackErr == nil
	return o.ackErr
}
func (o *dispatcherOutbox) Retry(_ context.Context, id, owner uint, token, message string, at time.Time) error {
	o.retried = true
	o.retryAt = at
	return nil
}

type dispatcherProducer func(context.Context, tasks.FileProcessingTask) error

func (f dispatcherProducer) ProduceFileTask(ctx context.Context, task tasks.FileProcessingTask) error {
	return f(ctx, task)
}

func TestOutboxDispatcherKeepsFailedDeliveryAndOnlySendsSQLIdentity(t *testing.T) {
	for _, mode := range []string{"success", "broker failure", "ack failure"} {
		t.Run(mode, func(t *testing.T) {
			outbox := &dispatcherOutbox{entry: model.DocumentTaskOutbox{DocumentID: 12, UserID: 7, Attempts: 100}, file: model.FileUpload{ID: 12, UserID: 7, Status: 1, FileMD5: "digest", FileName: "file.txt", OrgTag: "TEAM", IsPublic: true}}
			if mode == "ack failure" {
				outbox.ackErr = errors.New("database unavailable")
			}
			calls := 0
			before := time.Now()
			delivered, err := dispatchDocumentTask(context.Background(), outbox, dispatcherProducer(func(ctx context.Context, task tasks.FileProcessingTask) error {
				calls++
				want := tasks.FileProcessingTask{DocumentID: 12, UserID: 7, FileMD5: "digest", FileName: "file.txt", OrgTag: "TEAM", IsPublic: true}
				if !reflect.DeepEqual(task, want) {
					t.Fatalf("wrong identity sent: %+v", task)
				}
				if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 10*time.Second {
					t.Fatal("unbounded broker call")
				}
				if mode == "broker failure" {
					return errors.New("broker unavailable")
				}
				return nil
			}))
			if calls != 1 || (err == nil) != (mode == "success") || outbox.acked != (mode == "success") {
				t.Fatalf("delivery/ack outcome: %t %v %+v", delivered, err, outbox)
			}
			if outbox.retried != (mode == "broker failure") {
				t.Fatal("failed delivery was discarded")
			}
			if outbox.retried && (outbox.retryAt.Before(before) || outbox.retryAt.After(time.Now().Add(time.Minute))) {
				t.Fatal("retry backoff is not bounded")
			}
		})
	}
}
