// Package config 负责加载和管理应用程序的配置。
package config

import (
	"fmt"
	"strings"

	"github.com/spf13/viper"
)

// Config 是整个应用程序的配置结构体，与 config.yaml 文件结构对应。
type Config struct {
	Server             ServerConfig             `mapstructure:"server"`
	Database           DatabaseConfig           `mapstructure:"database"`
	JWT                JWTConfig                `mapstructure:"jwt"`
	Log                LogConfig                `mapstructure:"log"`
	Kafka              KafkaConfig              `mapstructure:"kafka"`
	Elasticsearch      ElasticsearchConfig      `mapstructure:"elasticsearch"`
	MinIO              MinIOConfig              `mapstructure:"minio"`
	Embedding          EmbeddingConfig          `mapstructure:"embedding"`
	LLM                LLMConfig                `mapstructure:"llm"`
	AI                 AIConfig                 `mapstructure:"ai"`
	QueryUnderstanding QueryUnderstandingConfig `mapstructure:"query_understanding"`
	Memory             MemoryConfig             `mapstructure:"memory"`
	DocumentProcessing DocumentProcessingConfig `mapstructure:"document_processing"`
}

// MemoryConfig uses conservative token estimates, not an exact provider tokenizer.
type MemoryConfig struct {
	Enabled               bool `mapstructure:"enabled"`
	ContextWindowTokens   int  `mapstructure:"context_window_tokens"`
	MaxInputTokens        int  `mapstructure:"max_input_tokens"`
	RecentTokens          int  `mapstructure:"recent_tokens"`
	SummaryTokens         int  `mapstructure:"summary_tokens"`
	LongTermTokens        int  `mapstructure:"long_term_tokens"`
	SummaryTimeoutSeconds int  `mapstructure:"summary_timeout_seconds"`
}

func (c MemoryConfig) WithDefaults() MemoryConfig {
	if c.ContextWindowTokens <= 0 {
		c.ContextWindowTokens = 32768
	}
	if c.MaxInputTokens <= 0 {
		c.MaxInputTokens = 20000
	}
	if c.RecentTokens <= 0 {
		c.RecentTokens = 4000
	}
	if c.SummaryTokens <= 0 {
		c.SummaryTokens = 3000
	}
	if c.LongTermTokens <= 0 {
		c.LongTermTokens = 1500
	}
	if c.SummaryTimeoutSeconds <= 0 {
		c.SummaryTimeoutSeconds = 15
	}
	return c
}

// QueryUnderstandingConfig controls the bounded pre-retrieval planning call.
type QueryUnderstandingConfig struct {
	Enabled        bool `mapstructure:"enabled"`
	TimeoutSeconds int  `mapstructure:"timeout_seconds"`
}

// ServerConfig 存储服务器相关的配置。
type ServerConfig struct {
	Port string `mapstructure:"port"`
	Mode string `mapstructure:"mode"`
}

// DatabaseConfig 存储所有数据库连接的配置。
type DatabaseConfig struct {
	MySQL MySQLConfig `mapstructure:"mysql"`
	Redis RedisConfig `mapstructure:"redis"`
}

// MySQLConfig 存储 MySQL 数据库的配置。
type MySQLConfig struct {
	DSN string `mapstructure:"dsn"`
}

// RedisConfig 存储 Redis 的配置。
type RedisConfig struct {
	Addr     string `mapstructure:"addr"`
	Password string `mapstructure:"password"`
	DB       int    `mapstructure:"db"`
}

// JWTConfig 存储 JWT 相关的配置。
type JWTConfig struct {
	Secret                 string `mapstructure:"secret"`
	AccessTokenExpireHours int    `mapstructure:"access_token_expire_hours"`
	RefreshTokenExpireDays int    `mapstructure:"refresh_token_expire_days"`
}

// LogConfig 存储日志相关的配置。
type LogConfig struct {
	Level      string `mapstructure:"level"`
	Format     string `mapstructure:"format"`
	OutputPath string `mapstructure:"output_path"`
}

// KafkaConfig 存储 Kafka 相关的配置。
type KafkaConfig struct {
	Brokers string `mapstructure:"brokers"`
	Topic   string `mapstructure:"topic"`
}

// ElasticsearchConfig 存储 Elasticsearch 相关的配置。
type ElasticsearchConfig struct {
	Addresses  string `mapstructure:"addresses"`
	Username   string `mapstructure:"username"`
	Password   string `mapstructure:"password"`
	IndexName  string `mapstructure:"index_name"`
	Dimensions int    `mapstructure:"dimensions"`
}

// MinIOConfig 存储 MinIO 对象存储的配置。
type MinIOConfig struct {
	Endpoint        string `mapstructure:"endpoint"`
	AccessKeyID     string `mapstructure:"access_key_id"`
	SecretAccessKey string `mapstructure:"secret_access_key"`
	UseSSL          bool   `mapstructure:"use_ssl"`
	BucketName      string `mapstructure:"bucket_name"`
}

// EmbeddingConfig 存储 Embedding 模型相关的配置。
type EmbeddingConfig struct {
	APIKey     string `mapstructure:"api_key"`
	BaseURL    string `mapstructure:"base_url"`
	Model      string `mapstructure:"model"`
	Dimensions int    `mapstructure:"dimensions"`
}

// LLMConfig 存储大语言模型相关的配置。
type LLMConfig struct {
	APIKey         string              `mapstructure:"api_key"`
	BaseURL        string              `mapstructure:"base_url"`
	Model          string              `mapstructure:"model"`
	TimeoutSeconds int                 `mapstructure:"timeout_seconds"`
	Generation     LLMGenerationConfig `mapstructure:"generation"`
	Prompt         LLMPromptConfig     `mapstructure:"prompt"`
}

// LLMGenerationConfig 配置生成相关参数（可选）。
type LLMGenerationConfig struct {
	Temperature float64 `mapstructure:"temperature"`
	TopP        float64 `mapstructure:"top_p"`
	MaxTokens   int     `mapstructure:"max_tokens"`
}

// LLMPromptConfig 配置系统提示与上下文包裹格式（可选）。
type LLMPromptConfig struct {
	Rules        string `mapstructure:"rules"`
	RefStart     string `mapstructure:"ref_start"`
	RefEnd       string `mapstructure:"ref_end"`
	NoResultText string `mapstructure:"no_result_text"`
}

// AIConfig 对齐 Java 的 ai.prompt/ai.generation（连字符键）
type AIConfig struct {
	Generation AIGenerationConfig `mapstructure:"generation"`
	Prompt     AIPromptConfig     `mapstructure:"prompt"`
}

type AIGenerationConfig struct {
	Temperature float64 `mapstructure:"temperature"`
	TopP        float64 `mapstructure:"top-p"`
	MaxTokens   int     `mapstructure:"max-tokens"`
}

type AIPromptConfig struct {
	Rules        string `mapstructure:"rules"`
	RefStart     string `mapstructure:"ref-start"`
	RefEnd       string `mapstructure:"ref-end"`
	NoResultText string `mapstructure:"no-result-text"`
}

// Load 从指定路径读取配置。配置由调用方持有，避免业务层依赖全局状态。
func Load(configPath string) (Config, error) {
	v := viper.New()
	v.SetConfigFile(configPath)
	v.SetConfigType("yaml")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	workerEnv := [][2]string{
		{"document_processing.worker_token", "DOCUMENT_WORKER_TOKEN"},
		{"document_processing.tokenizer_id", "DOCUMENT_TOKENIZER_ID"},
		{"document_processing.tokenizer_revision", "DOCUMENT_TOKENIZER_REVISION"},
		{"document_processing.embedding_model", "DOCUMENT_EMBEDDING_MODEL"},
	}
	for _, binding := range workerEnv {
		if err := v.BindEnv(binding[0], binding[1]); err != nil {
			return Config{}, fmt.Errorf("无法绑定 Document Worker 环境变量 %s: %w", binding[1], err)
		}
	}

	if err := v.ReadInConfig(); err != nil {
		return Config{}, fmt.Errorf("读取配置文件失败: %w", err)
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return Config{}, fmt.Errorf("无法将配置解析到结构体中: %w", err)
	}
	return cfg, nil
}
