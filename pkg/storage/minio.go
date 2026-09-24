// Package storage 提供对象存储访问和对象键规则。
package storage

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"pai-smart-go/internal/config"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

var ErrMD5Mismatch = errors.New("object MD5 mismatch")

// Client 封装 MinIO 客户端和桶名，调用方无需依赖包级全局变量。
type Client struct {
	client *minio.Client
	bucket string
}

// NewClient 创建客户端并确保目标桶存在。
func NewClient(ctx context.Context, cfg config.MinIOConfig) (*Client, error) {
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		Secure: cfg.UseSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("初始化 MinIO 客户端失败: %w", err)
	}

	exists, err := client.BucketExists(ctx, cfg.BucketName)
	if err != nil {
		return nil, fmt.Errorf("检查 MinIO 存储桶失败: %w", err)
	}
	if !exists {
		if err := client.MakeBucket(ctx, cfg.BucketName, minio.MakeBucketOptions{}); err != nil {
			return nil, fmt.Errorf("创建 MinIO 存储桶失败: %w", err)
		}
	}

	return &Client{client: client, bucket: cfg.BucketName}, nil
}

func (c *Client) Put(ctx context.Context, objectKey string, reader io.Reader, size int64) error {
	_, err := c.client.PutObject(ctx, c.bucket, objectKey, reader, size, minio.PutObjectOptions{})
	return err
}

func (c *Client) Get(ctx context.Context, objectKey string) (io.ReadCloser, error) {
	return c.client.GetObject(ctx, c.bucket, objectKey, minio.GetObjectOptions{})
}

func (c *Client) Copy(ctx context.Context, destinationKey, sourceKey string) error {
	_, err := c.client.CopyObject(ctx,
		minio.CopyDestOptions{Bucket: c.bucket, Object: destinationKey},
		minio.CopySrcOptions{Bucket: c.bucket, Object: sourceKey},
	)
	return err
}

func (c *Client) Compose(ctx context.Context, destinationKey string, sourceKeys []string) error {
	sources := make([]minio.CopySrcOptions, 0, len(sourceKeys))
	for _, key := range sourceKeys {
		sources = append(sources, minio.CopySrcOptions{Bucket: c.bucket, Object: key})
	}
	_, err := c.client.ComposeObject(ctx, minio.CopyDestOptions{Bucket: c.bucket, Object: destinationKey}, sources...)
	return err
}

func (c *Client) Remove(ctx context.Context, objectKey string) error {
	return c.client.RemoveObject(ctx, c.bucket, objectKey, minio.RemoveObjectOptions{})
}

func (c *Client) RemoveMany(ctx context.Context, objectKeys []string) error {
	for _, key := range objectKeys {
		if err := c.Remove(ctx, key); err != nil {
			return err
		}
	}
	return nil
}

// RemoveDocumentIR has a fixed owner/document scope, never a caller-supplied prefix.
func (c *Client) RemoveDocumentIR(ctx context.Context, userID, documentID uint) error {
	if userID == 0 || documentID == 0 {
		return fmt.Errorf("invalid document artifact identity")
	}
	return c.removePrefix(ctx, documentIRPrefix(userID, documentID))
}

// RemoveDocumentChunks removes canonical chunks and abandoned upload attempts
// without touching the verified raw object.
func (c *Client) RemoveDocumentChunks(ctx context.Context, userID, documentID uint) error {
	if userID == 0 || documentID == 0 {
		return fmt.Errorf("invalid document chunk identity")
	}
	return c.removePrefix(ctx, documentUploadPrefix("chunks", userID, documentID))
}

// RemoveDocumentUpload never crosses the SQL upload generation, including failed merge attempts.
func (c *Client) RemoveDocumentUpload(ctx context.Context, userID, documentID uint) error {
	if userID == 0 || documentID == 0 {
		return fmt.Errorf("invalid document upload identity")
	}
	for _, kind := range []string{"merged", "chunks"} {
		if err := c.removePrefix(ctx, documentUploadPrefix(kind, userID, documentID)); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) removePrefix(ctx context.Context, prefix string) error {
	for object := range c.client.ListObjects(ctx, c.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if object.Err != nil {
			return object.Err
		}
		if err := c.Remove(ctx, object.Key); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// VerifyMD5 校验对象内容，避免客户端声明的 MD5 与实际文件不一致。
func (c *Client) VerifyMD5(ctx context.Context, objectKey, expectedMD5 string) error {
	object, err := c.Get(ctx, objectKey)
	if err != nil {
		return err
	}
	defer object.Close()
	return verifyMD5(object, expectedMD5)
}

func verifyMD5(reader io.Reader, expectedMD5 string) error {
	hash := md5.New()
	if _, err := io.Copy(hash, reader); err != nil {
		return err
	}
	if got := hex.EncodeToString(hash.Sum(nil)); !strings.EqualFold(got, expectedMD5) {
		return fmt.Errorf("%w: got %s, want %s", ErrMD5Mismatch, got, expectedMD5)
	}
	return nil
}

func (c *Client) PresignedGetURL(ctx context.Context, objectKey string, expiry time.Duration) (string, error) {
	presignedURL, err := c.client.PresignedGetObject(ctx, c.bucket, objectKey, expiry, url.Values{})
	if err != nil {
		return "", err
	}
	return presignedURL.String(), nil
}
