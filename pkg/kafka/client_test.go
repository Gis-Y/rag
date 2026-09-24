package kafka

import (
	"context"
	"errors"
	"pai-smart-go/internal/config"
	"pai-smart-go/pkg/log"
	"pai-smart-go/pkg/tasks"
	"testing"
	"time"

	kafkago "github.com/segmentio/kafka-go"
)

func TestProducerWaitsForBrokerAcknowledgement(t *testing.T) {
	producer := NewProducer(config.KafkaConfig{Brokers: "localhost:9092", Topic: "file-processing"})
	defer producer.Close()
	if producer.writer.RequiredAcks != kafkago.RequireAll || producer.writer.Async {
		t.Fatal("outbox delivery must wait for synchronous broker acknowledgement")
	}
}

type testProcessor struct {
	calls  int
	passAt int
}

type processorFunc func(context.Context, tasks.FileProcessingTask) error

func (f processorFunc) Process(ctx context.Context, task tasks.FileProcessingTask) error {
	return f(ctx, task)
}

type scriptedConsumerReader struct {
	message        kafkago.Message
	firstFetchErr  error
	fetched        bool
	fetches        int
	commitFailures int
	commitAttempts int
	commits        int
	cancel         context.CancelFunc
}

func (r *scriptedConsumerReader) FetchMessage(ctx context.Context) (kafkago.Message, error) {
	r.fetches++
	if r.firstFetchErr != nil {
		err := r.firstFetchErr
		r.firstFetchErr = nil
		return kafkago.Message{}, err
	}
	if !r.fetched {
		r.fetched = true
		return r.message, nil
	}
	<-ctx.Done()
	return kafkago.Message{}, ctx.Err()
}

func (r *scriptedConsumerReader) CommitMessages(context.Context, ...kafkago.Message) error {
	r.commitAttempts++
	if r.commitFailures > 0 {
		r.commitFailures--
		return errors.New("commit unavailable")
	}
	r.commits++
	if r.cancel != nil {
		r.cancel()
	}
	return nil
}

func (*scriptedConsumerReader) Close() error { return nil }

func (p *testProcessor) Process(context.Context, tasks.FileProcessingTask) error {
	p.calls++
	if p.calls == p.passAt {
		return nil
	}
	return errors.New("not ready")
}

func TestProcessWithRetryExhaustsSameTaskBeforeAdvancing(t *testing.T) {
	for _, passAt := range []int{1, 2, 3, 4} {
		p := &testProcessor{passAt: passAt}
		err := processWithRetry(context.Background(), p, tasks.FileProcessingTask{DocumentID: 12}, 0)
		if (err == nil) != (passAt <= 3) || p.calls != min(passAt, 3) {
			t.Fatalf("passAt=%d calls=%d err=%v", passAt, p.calls, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := &testProcessor{}
	if err := processWithRetry(ctx, p, tasks.FileProcessingTask{}, time.Hour); !errors.Is(err, context.Canceled) || p.calls != 0 {
		t.Fatalf("cancellation: calls=%d err=%v", p.calls, err)
	}
}

func TestConsumerRecoversFetchAndCommitErrors(t *testing.T) {
	log.Init("error", "json", "")
	ctx, cancel := context.WithCancel(context.Background())
	reader := &scriptedConsumerReader{
		message:        kafkago.Message{Value: []byte(`{"document_id":12,"file_md5":"digest","file_name":"doc.md","user_id":7}`)},
		firstFetchErr:  errors.New("broker temporarily unavailable"),
		commitFailures: 1,
		cancel:         cancel,
	}
	processor := &testProcessor{passAt: 1}
	err := runConsumer(ctx, reader, processor, 0)
	if !errors.Is(err, context.Canceled) || reader.fetches < 2 || reader.commitAttempts != 2 || reader.commits != 1 || processor.calls != 1 {
		t.Fatalf("consumer did not recover: fetches=%d commitAttempts=%d commits=%d calls=%d err=%v", reader.fetches, reader.commitAttempts, reader.commits, processor.calls, err)
	}
}

func TestConsumerCommitsOnlySuccessfulOrPersistedTerminalOutcome(t *testing.T) {
	log.Init("error", "json", "")
	t.Run("unpersisted failure keeps offset", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		reader := &scriptedConsumerReader{message: kafkago.Message{Value: []byte(`{"document_id":12,"user_id":7}`)}}
		calls := 0
		err := runConsumer(ctx, reader, processorFunc(func(context.Context, tasks.FileProcessingTask) error {
			calls++
			if calls == 3 {
				cancel()
			}
			return errors.New("database unavailable before terminal state")
		}), 0)
		if !errors.Is(err, context.Canceled) || reader.commitAttempts != 0 || calls != 3 {
			t.Fatalf("unpersisted failure advanced offset: commits=%d calls=%d err=%v", reader.commitAttempts, calls, err)
		}
	})

	t.Run("persisted failure is committed", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		reader := &scriptedConsumerReader{message: kafkago.Message{Value: []byte(`{"document_id":12,"user_id":7}`)}, cancel: cancel}
		calls := 0
		err := runConsumer(ctx, reader, processorFunc(func(context.Context, tasks.FileProcessingTask) error {
			calls++
			return tasks.MarkProcessingFailurePersisted(errors.New("parser rejected document"))
		}), 0)
		if !errors.Is(err, context.Canceled) || reader.commits != 1 || calls != 3 {
			t.Fatalf("persisted terminal result was not committed: commits=%d calls=%d err=%v", reader.commits, calls, err)
		}
	})
}
