# PaiSmart-Go

PaiSmart-Go（派聪明 Go 版）是一个支持组织权限的 RAG 知识库：上传文档后异步解析、分块并建立索引，用户可以检索文档，或通过流式对话获取带来源的回答。后端使用 Go，管理界面使用 Vue 3。

> 当前仓库提供**本地开发环境**：Docker Compose 启动中间件和 Document Worker，Go 后端与 Vue 前端在宿主机分别运行。它不是包含前后端的一键生产部署。

## 功能与处理流程

- 分片上传、断点续传、文件状态查询、预览、下载和重处理。
- Document Worker 解析文档并按结构生成父子块；仅子块生成 Embedding 并写入 Elasticsearch。
- Elasticsearch 混合检索，结合活动版本与 MySQL 权限复核，按组织标签控制可见范围。
- WebSocket 流式问答，返回来源引用；支持会话、用户及组织标签管理。

```text
上传 → MinIO 原件 + MySQL 任务 → Kafka → Document Worker 解析/分块
     → Embedding → Elasticsearch 索引 → 检索/权限复核 → LLM 流式回答
```

## 技术与目录

| 目录 | 职责 |
| --- | --- |
| `cmd/server/` | Go 服务入口、依赖组装和路由 |
| `internal/handler/`、`service/`、`repository/` | 接口、业务逻辑和数据访问 |
| `internal/pipeline/`、`workers/document/` | 异步处理编排与文档解析/分块 |
| `pkg/` | MySQL/Redis、MinIO、Kafka、ES、Embedding、LLM 等适配器 |
| `frontend/` | Vue 3 + TypeScript + Vite 管理界面 |
| `configs/`、`deployments/`、`docs/` | 应用配置、开发环境编排和详细文档 |

服务端基于 Go 1.23（`go.mod` 指定 `go1.24.9` toolchain）、Gin 和 GORM；数据及基础设施为 MySQL 8、Redis 7、MinIO、Kafka、Elasticsearch 8、Tika。Embedding 使用 OpenAI 兼容接口；默认模型配置为 `Qwen/Qwen3-Embedding-0.6B`（1024 维），LLM 默认配置为 DeepSeek Chat。**模型服务和密钥不由 Compose 提供。**

## 本地启动

克隆仓库后在项目根目录运行以下命令，示例使用 PowerShell。准备好 Go、Node.js ≥ 18.20、pnpm ≥ 8.7、Docker Compose，并确保 Docker 有足够内存运行 ES（2 GiB）和 Worker（上限 6 GiB）。

### 配置环境变量

在启动 Compose 和 Go 的终端中设置同一组变量；如果另开终端启动后端，需要重新设置或通过本地私有脚本加载。将占位值替换为自己的凭据，**不要提交凭据到仓库**。

```powershell
$env:MYSQL_ROOT_PASSWORD = "<mysql-password>"
$env:DATABASE_REDIS_PASSWORD = "<redis-password>"
$env:MINIO_ACCESS_KEY_ID = "<minio-user>"
$env:MINIO_SECRET_ACCESS_KEY = "<minio-password>"
$env:DOCUMENT_WORKER_TOKEN = "<private-worker-token>"
$env:DOCUMENT_TOKENIZER_REVISION = "<40-character-model-commit-sha>"

$env:DATABASE_MYSQL_DSN = "root:<mysql-password>@tcp(127.0.0.1:3307)/PaiSmart?charset=utf8mb4&parseTime=True&loc=Local"
$env:JWT_SECRET = "<at-least-32-characters>"
$env:EMBEDDING_BASE_URL = "http://127.0.0.1:<port>/v1"
$env:LLM_API_KEY = "<deepseek-api-key>"
# Embedding 服务要求鉴权时，再设置 EMBEDDING_API_KEY。
```

`DOCUMENT_TOKENIZER_REVISION` 必须是所选模型仓库的固定 40 位提交 SHA，不能写 `main`。Worker 的 tokenizer、后端 Embedding 模型和实际 Embedding 服务必须一致；更换模型还要核对向量维度。若使用其他 OpenAI 兼容 LLM，可通过 `LLM_BASE_URL`、`LLM_MODEL` 和 `LLM_API_KEY` 覆盖默认值。配置项及模型准备细节见 [文档处理说明](docs/document-processing-v1.md)。

### 启动本地依赖

```powershell
docker compose -f deployments/docker-compose.yaml up -d --build
docker compose -f deployments/docker-compose.yaml ps
```

Compose 启动 MySQL、Redis、MinIO、Kafka、Elasticsearch、Tika 和 Document Worker，**不启动 Go 后端、Vue 前端或 Embedding/LLM 服务**。首次构建与模型下载可能较久；Worker 的 `/health` 只表示进程存活，不保证模型已预热。默认宿主机端口：MySQL `3307`、Redis `6380`、MinIO `9000/9001`、Kafka `9092`、ES `9200`、Tika `9998`、Worker `8091`。

### 后端启动

```powershell
go run cmd/server/main.go
```

后端默认监听 `http://localhost:8081`。新 MySQL 数据卷会通过 [docs/ddl.sql](docs/ddl.sql) 初始化；已有数据库**不会自动迁移**，应按 [文档处理说明](docs/document-processing-v1.md#开发环境配置) 应用缺失的迁移。启动时会检查所需表和字段；缺少 Worker token、固定 tokenizer revision、Embedding 地址或模型不一致也会直接报错。

### 前端启动

在另一个终端运行：

```powershell
cd frontend
pnpm install
pnpm run dev
```

访问终端输出的 Vite 地址。开发环境接口在 [frontend/.env.test](frontend/.env.test) 中指向 `http://localhost:8081/api/v1`；本机后端端口变化时需要同步调整。系统不提供默认账号，先在注册页创建账号。如需管理员权限，由数据库管理员确认账号后执行：

```sql
UPDATE users SET role = 'ADMIN' WHERE username = 'your_admin';
```

## 验证

```powershell
go test ./...
python -m unittest discover -s workers/document -p 'test_*.py' -v
cd frontend
pnpm run typecheck
pnpm run build
```

Worker 单测需要本机 Python 及 [依赖](workers/document/requirements.txt)。这些检查命令不代表外部服务、OCR、Embedding/LLM 和完整端到端流程已经在你的环境通过。前端 `lint` 脚本带 `--fix`，会修改文件，运行前请确认工作区状态。

## 部署与安全边界

当前 Compose 面向本地开发：服务端口只绑定本机，ES 关闭了安全认证，且没有编排 Go 后端和前端。**不要原样暴露到公网作为生产环境。**生产上线需自行补齐镜像/反向代理、TLS、访问控制、凭据管理、持久化备份、监控和资源规划。修改环境变量不会自动轮换已有数据库密码，也不会迁移现有数据卷；如果旧版本曾提交真实凭据，还需轮换凭据并检查 Git 历史。

## 进一步阅读

- [文档处理、模型一致性与迁移](docs/document-processing-v1.md)
- [Document Worker 使用与限制](workers/document/README.md)
- [查询理解](docs/query-understanding.md)
- [对话记忆](docs/conversation-memory.md)
