# 文档处理第一版

这版实现《架构.md》的核心闭环，是项目唯一的文档处理模式。上传、解析、预览和检索统一使用结构化 IR、父子块及活动版本；没有模式开关、字符切分管线或旧向量兼容路径。项目按无存量文档配置，不自动修改数据库或删除索引。

## 已实现与边界

- TXT/Markdown、HTML/Tika XHTML 转统一 IR；PDF 使用 Docling，文字层不足时启用中文/英文 Tesseract OCR。先保留结构、来源、表格原始单元格，再分块。
- 标题、段落、列表等结构边界切分时 **0 重叠**；标题跳级也按真实层级维护路径。子块参考 450、上限 600 tokens；恰好达到上限不拆。超长段落先按完整句子、代码先按完整行切分，均不重叠；只有单句或单行仍超限时才做 Token 兜底，同一单元、同一父块内续片最多重叠 75 tokens。
- 父块参考 1500、上限 2000 tokens，不重叠、不向量化。最终检索字符串含元数据前缀，使用固定 revision 的真实 tokenizer 计数。完整标题和文件名保留在元数据，不因前缀限长而丢失。
- `context_prefix` 随父子块持久化并进入最终问答上下文，`body_text` 保持原文。表格 caption、单位、列路径、行定位值和代码语言不能静默截断；表格按行/单元格切分且不重叠，单个必要前缀本身超过预算时明确失败。行定位优先解析器标出的行标题，否则使用首个非空数据单元格，不推断业务主键或数值单位。
- MinIO 保存原文件及包含完整 IR、SHA256、parser/tokenizer 版本的产物；MySQL 保存处理状态和带来源的父子块；只有子块向量进入 ES。
- 每次处理写独立版本；全部 Embedding 与 ES Bulk 逐项验证成功、refresh 可见后，SQL CAS 发布。已经发布的结构化 active 版本在重建失败时仍可检索。
- 查询在现有混合检索上增加活动版本/模型过滤及 SQL 权限复核，按预算批量补充父块，同父只注入一次。完整子块与多跳必要证据优先；预算不足先撤销父块扩展，不再统一截前 1000 字符。
- 来源保留标题路径、页码、块内 Unicode 字符范围及可用 BBox；不知道的位置为空。父块额外事实使用独立来源编号。回答的 `[来源#N]` 绑定服务端返回并持久化的文档 ID/版本；历史记录恢复同一来源，下载和预览按身份及活动版本复核权限，不再按同名文件猜测。没有来源身份的历史文本不可点击；未新增前端页图定位组件。
- 回答渲染禁用原始 HTML 和任意 Markdown 属性；来源名称按纯文本显示，点击只通过受控编号查找本轮来源，不能从文件名拼接下载目标。
- Kafka 对同一消息显式尝试最多 3 次；仅在成功或失败状态已经持久化后提交 offset，拉取/提交失败会重试。删除先置不可用状态，再清理新索引、IR 和父子块，旧任务不能重新发布。
- 合并使用数据库租约和独立原件键 `merged/{ownerID}/{documentID}/{mergeToken}`，校验 MD5 后才发布；重复请求直接返回已完成结果，失败请求不能覆盖或删除已发布原件。分片及 Redis 进度也按文档 ID 隔离，旧清理任务不会影响删除后重新上传的文件。
- 上传完成状态与待投递任务在同一 SQL 事务内提交；后台从 `document_task_outbox` 重试投递，收到同步 Kafka `RequireAll` 确认后才清除记录。重处理也走同一队列；采用至少一次投递，确认丢失时可能重复处理，不承诺恰好一次。
- 删除接口按文档 ID 定位并校验所有者或管理员，不能用同 MD5 的其他用户文档代替。上传列表保留尚未落库的本地任务，刷新只接受最新响应；账号/会话切换取消旧上传，正常 Token 刷新不打断续传。检索结果在权限/版本过滤后为空时返回 `[]`。
- Markdown 表格保留空的首尾单元格，代码围栏校验闭合符类型和长度，HTML 预格式化代码保留缩进和换行；PDF OCR 按实际内容流及变换矩阵检查图片覆盖（含内联图片和嵌套 Form），循环、损坏资源或超过检查预算时保守启用 OCR，不能只凭水印文字跳过扫描内容。
- Go 在接收 Worker 结果时验证完整 IR、表格单元格、字符范围及父子块内容覆盖；子进程返回结果也受总超时约束。健康检查可与解析并行，解析仍受并发和资源上限控制。
- 预览直接读取已发布版本的 IR，不再由 Go 调用 Tika 重新解析。Tika 仅作为 Worker 的通用格式结构化解析服务；未处理完成的文档不能伪装成已可用内容。
- 上传前端读取服务端 `maxFileBytes`，服务端在持久化前校验总大小、分片序号及实际字节数；每个 multipart 请求最多接收 5 MiB 分片加 1 MiB 表单开销。日志不读取请求/响应正文，避免先于上传限额缓存整个文件。

第一版暂不包括：PaddleOCR-VL / MinerU / Camelot 多候选路由、完整页面质量诊断、跨页表格自动合并、独立双路 RRF/专用 Reranker、表格摘要索引及 SQL 数值聚合。表格完整原始数据在 IR；不把 Top-K 行组冒充整表统计。最终问答预算仍复用现有保守字节估算，和文档分块的实际 Token 计数明确分开。

## 开发环境配置

1. 新数据库通过 `docs/ddl.sql` 初始化，包含处理表、来源字段和上传投递队列，不再创建 `document_vectors`。已有开发库缺两张处理表时，执行 `docs/migrations/20260903_document_processing.sql`；缺来源字段时，再执行一次 `docs/migrations/20260903_document_sources.sql`。本轮新增字段及队列通过 `docs/migrations/20260904_document_upload_lifecycle.sql` 应用一次。启动会检查必需表/字段，不自动执行迁移；按无存量文档设计，不兼容旧原件对象键，若开发库有旧文档须重新上传。
2. 配置与 tokenizer 一致的 Embedding 服务。配置统一为 `Qwen/Qwen3-Embedding-0.6B / 1024`，`embedding.base_url` 留空，必须填写实际服务地址及所需凭据；不能假设其他模型的服务端支持这个模型名称。
3. 在受控环境准备 Docling 模型缓存和对应模型的 `tokenizer.json`，取得官方固定的 40 位提交 SHA。按 [后端启动说明](../README.md#后端启动) 设置服务凭据、`DOCUMENT_TOKENIZER_REVISION` 和非空 `DOCUMENT_WORKER_TOKEN`，然后启动主 Compose（已包含 Worker）：

```powershell
docker compose -f deployments/docker-compose.yaml up -d --build
```

Worker 默认只映射本机 `8091`，有 32 MiB 文件、200 页、180 秒解析、6 GiB 内存等上限；健康检查仅说明进程存活，不保证模型已就绪。模型初始化也计入解析时限，建议预热并持久化缓存。详细参数见 `workers/document/README.md`。

4. 在 `configs/config.yaml` 填写以下非秘密配置。Go 显式读取同一组 `DOCUMENT_WORKER_TOKEN`、`DOCUMENT_TOKENIZER_ID`、`DOCUMENT_TOKENIZER_REVISION` 和 `DOCUMENT_EMBEDDING_MODEL` 环境变量，需让 Go 启动进程也继承这些变量；仅放在 Compose 的 `.env` 文件中不会自动传给另行启动的 Go 进程。凭据通过环境变量注入，不写入受版本控制的 YAML：

```yaml
document_processing:
  worker_url: "http://127.0.0.1:8091"
  worker_token: "" # DOCUMENT_WORKER_TOKEN
  tokenizer_id: "Qwen/Qwen3-Embedding-0.6B"
  tokenizer_revision: "" # DOCUMENT_TOKENIZER_REVISION，官方固定的40位提交SHA
  embedding_model: "Qwen/Qwen3-Embedding-0.6B"
  timeout_seconds: 300
  max_file_bytes: 33554432
  parent_context_tokens: 3000
embedding:
  model: "Qwen/Qwen3-Embedding-0.6B"
  dimensions: 1024
  base_url: "填写实际部署的兼容接口地址，包含所需的 /v1 路径"
  # api_key 由对应服务提供；无鉴权的本地服务可留空。
elasticsearch:
  index_name: "knowledge_base"
  # 维度统一由 embedding.dimensions 传入并校验。
```

若 Go 后端也运行在 Compose 内，Worker 地址改为 `http://document-worker:8091`。缺少 Worker 地址/鉴权、固定 tokenizer revision、匹配的模型或 Embedding 地址时启动直接报错。ES 不再默认回退 2048 维；发现不匹配的开发索引时也会报错，不自动删除重建。Embedding 服务不能隐式添加与分块计数不一致的前缀或截断输入。

若调整上传上限，Go 的 `document_processing.max_file_bytes` 与 Worker 的 `MAX_FILE_BYTES` 必须同步；前端通过 `GET /api/v1/upload/supported-types` 的 `maxFileBytes` 使用 Go 的当前值。

5. 运行 `go run cmd/server/main.go`，上传测试文件，检查处理状态、预览、父子来源和回答。Worker 不可用时处理明确失败，修复配置后可调用重处理接口；不会回退到另一套解析/切分模式。

## 状态与重处理

接口始终注册，沿用现有 JWT。状态与重处理接口默认仅自己的文档，管理员可通过 `?userId=<ownerID>` 指定所有者；删除始终按文档 ID 定位。

| 接口 | 用途 |
| --- | --- |
| `GET /api/v1/documents/:fileMd5/processing` | 查询是否未处理、处理中、失败或已有 ready 版本 |
| `POST /api/v1/documents/:fileMd5/reprocess` | 根据 SQL 中的文档 ID、所有者和权限持久化待投递任务；202 仅代表已入队 |
| `DELETE /api/v1/documents/:documentId` | 按指定文档 ID 删除；仅所有者或管理员，不接受 MD5 或所有者参数替代目标 |

上传完成 `file_upload.status=1` 不等于解析完成；`status=4` 表示有合并租约，租约到期后可重新尝试。处理状态存于 `document_processing_states`；只有 `active_version` 对应的父子版本可被结构化检索使用。状态和重处理接口不接受调用者伪造的文档 ID、源地址、组织或公开标志；删除接口仅接受指定的文档 ID，所有者由服务端查询。

Kafka 任务必须包含 `document_id` 与所有者身份；文件名和 MD5 不能替代文档身份。没有已发布活动版本的文档不会被检索，ES 返回缺少版本的结果也会被拒绝。

## 运行限制

- 重处理失败保留已经发布的活动版本。未来切换模型或维度时需要重新生成匹配的索引，不允许混用不同模型的向量。
- 初始状态简化为 `parsing → embedding → ready / failed`，Worker 内部完成规范化/分块；不伪造细粒度进度。失败重试会生成新候选版本，并尝试清理本次失败版本的产物；外部存储持续故障时可能残留不可检索的产物，尚无持久化清理队列，需离线巡检回收。
- 删除会清理该文档已有的新 IR、ES 和 SQL 数据；极端情况下正在执行的外部写入可能晚于清理，留下不可检索孤儿，需离线巡检清理。SQL 文档身份、删除态和版本校验阻止其重新可见。
- Tika 作为独立服务必须自行隔离和限制资源，Worker 超时不能终止远程 Tika 已开始的工作。没有可靠的页/单元格定位时不生成虚构坐标。PDF OCR 是基础路线，不代表已经验证所有扫描样本。

## 验证

```powershell
go test ./...
go test -race ./...
python -m unittest discover -s workers/document -p 'test_*.py' -v
$testFiles = @(rg --files frontend -g '*.test.mjs' -g '!**/node_modules/**')
node --test @testFiles
```

测试覆盖结构/Token 边界、续片覆盖与重叠、表格及来源、Worker 请求限制、批量响应错误、版本与权限过滤、发布失败保留、删除防复活、上下文预算和重处理鉴权。本轮还覆盖合并幂等与并发租约、不确定提交保留原件、上传/队列事务回滚、投递重试、按 ID 删除与旧清理隔离，以及空列、嵌套 PDF 图片、代码缩进和前端列表竞态。测试中的伪 tokenizer 用于确定性边界；可选的真实 `tokenizers` BPE 测试不等于 Qwen 模型质量测试。

开发验证包含 `go test ./...`、`go test -race ./...`、`go vet ./...`、`go build ./...` 和 Worker 回归测试（含可选真实 Tokenizers ByteLevel BPE 测试及 Excel/Tika 路由测试）。可设置 `DOCUMENT_TEST_PYTHON` 执行 Python → Go 输出契约测试。主 Compose 进行 YAML/依赖/资源限制静态校验；当前环境无 Docker CLI，未构建或启动容器。

本轮 Worker 37 项回归已通过（含真实 ByteLevel BPE，无跳过）；Go 全量测试、竞态测试、`vet` 和构建通过；前端 70 项 Node 回归通过（含知识库/检索模板编译、请求刷新及取消、跨账号隔离和 WebSocket 代理）。前端整库 `typecheck` 仍报依赖缺失、第三方源码类型和自动导入声明错误，不能视作已通过；本轮未运行 Vite 构建。真实 MySQL/Redis 持久化测试需专用 `_test` 库、相应迁移及显式测试环境变量，本轮未运行。

当前未自动启动生产服务、执行迁移或调用真实 Embedding/LLM。Docling/OCR、固定 Qwen tokenizer 和 MySQL/Kafka/MinIO/ES 全链路仍需在配置好的隔离环境，用原生/扫描/混合 PDF 样本联调验收。
