// Package kafka 提供了与 Kafka 消息队列交互的功能。
package kafka

import (
	"context"
	"encoding/json"
	"fmt"
	"pai-smart-go/internal/config"
	"pai-smart-go/pkg/log"
	"pai-smart-go/pkg/tasks"
	"time"

	kafkago "github.com/segmentio/kafka-go"
)

// TaskProcessor defines the interface for any service that can process a task.
// This decouples the Kafka consumer from the concrete pipeline implementation.
type TaskProcessor interface {
	Process(ctx context.Context, task tasks.FileProcessingTask) error
}

type consumerReader interface {
	FetchMessage(context.Context) (kafkago.Message, error)
	CommitMessages(context.Context, ...kafkago.Message) error
	Close() error
}

// Producer 是可注入的 Kafka 任务生产者。
type Producer struct {
	writer *kafkago.Writer
}

func NewProducer(cfg config.KafkaConfig) *Producer {
	return &Producer{writer: &kafkago.Writer{
		Addr:         kafkago.TCP(cfg.Brokers),
		Topic:        cfg.Topic,
		Balancer:     &kafkago.Hash{},
		RequiredAcks: kafkago.RequireAll,
	}}
}

// ProduceFileTask 发送一个文件处理任务到 Kafka。
func (p *Producer) ProduceFileTask(ctx context.Context, task tasks.FileProcessingTask) error {
	taskBytes, err := json.Marshal(task)
	if err != nil {
		return err
	}
	key := fmt.Sprintf("%d:%d:%s", task.UserID, task.DocumentID, task.FileMD5)
	return p.writer.WriteMessages(ctx, kafkago.Message{Key: []byte(key), Value: taskBytes})
}

func (p *Producer) Close() error {
	return p.writer.Close()
}

// StartConsumer 启动一个 Kafka 消费者来处理文件任务。
func StartConsumer(cfg config.KafkaConfig, processor TaskProcessor) {
	r := kafkago.NewReader(kafkago.ReaderConfig{
		Brokers:  []string{cfg.Brokers},
		Topic:    cfg.Topic,
		GroupID:  "pai-smart-go-consumer",
		MinBytes: 10e3, // 10KB
		MaxBytes: 10e6, // 10MB
	})

	log.Infof("Kafka 消费者已启动，正在监听主题 '%s'", cfg.Topic)
	if err := runConsumer(context.Background(), r, processor, time.Second); err != nil {
		log.Errorf("Kafka 消费者已停止: %v", err)
	}
	if err := r.Close(); err != nil {
		log.Errorf("关闭 Kafka 消费者失败: %v", err)
	}
}

func runConsumer(ctx context.Context, r consumerReader, processor TaskProcessor, retryDelay time.Duration) error {
	for {
		m, err := r.FetchMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Error("从 Kafka 读取消息失败", err)
			if !waitConsumerRetry(ctx, retryDelay) {
				return ctx.Err()
			}
			continue
		}

		log.Infof("收到 Kafka 消息: offset %d", m.Offset)

		var task tasks.FileProcessingTask
		if err := json.Unmarshal(m.Value, &task); err != nil {
			log.Errorf("无法解析 Kafka 消息: %v, value: %s", err, string(m.Value))
			// 消息格式错误，直接提交，避免阻塞队列
			if err := commitWithRetry(ctx, r, m, retryDelay); err != nil {
				return err
			}
			continue
		}

		log.Infof("开始处理文件任务: MD5=%s, FileName=%s", task.FileMD5, task.FileName)
		for {
			err = processWithRetry(ctx, processor, task, retryDelay)
			if err == nil {
				log.Infof("文件任务处理成功: MD5=%s", task.FileMD5)
				break
			}
			if tasks.IsProcessingFailurePersisted(err) {
				log.Errorf("文件任务以已持久化失败终态结束: MD5=%s, Error: %v", task.FileMD5, err)
				break
			}
			// Do not fetch a later offset: committing it would also commit this
			// message. Retry the same identity until SQL records a terminal state.
			log.Errorf("文件任务失败且终态未持久化，保留offset并重试: MD5=%s, Error: %v", task.FileMD5, err)
			if !waitConsumerRetry(ctx, retryDelay) {
				return ctx.Err()
			}
		}
		if err := commitWithRetry(ctx, r, m, retryDelay); err != nil {
			return err
		}
	}
}

func commitWithRetry(ctx context.Context, r consumerReader, message kafkago.Message, delay time.Duration) error {
	for {
		if err := r.CommitMessages(ctx, message); err == nil {
			return nil
		} else if ctx.Err() != nil {
			return ctx.Err()
		} else {
			log.Errorf("提交 Kafka 消息 offset 失败，将重试同一消息: %v", err)
		}
		if !waitConsumerRetry(ctx, delay) {
			return ctx.Err()
		}
	}
}

func waitConsumerRetry(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func processWithRetry(ctx context.Context, processor TaskProcessor, task tasks.FileProcessingTask, delay time.Duration) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err = processor.Process(ctx, task)
		if err == nil {
			return nil
		}
		if attempt < 2 {
			timer := time.NewTimer(delay * time.Duration(attempt+1))
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	return fmt.Errorf("file processing failed after 3 attempts: %w", err)
}
