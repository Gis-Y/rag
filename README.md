Go 版派聪明（PaiSmart-Go）是一个企业级的 AI 知识库管理系统，采用 RAG 技术提供智能文档处理和检索能力。核心技术栈包括：

- Go 1.23+、模块化目录：`cmd/` `internal/` `pkg/`；分层：`handler/service/repository`
- 配置/日志/关停：Viper、Zap（结构化日志）、Gin + Context 优雅停机
- Gin（路由分组/中间件）、Gorilla WebSocket（双向通信、增量写出、停止指令）
- JWT（access/refresh）、基于 `org_tag` 的层级聚合，检索期过滤（should + minimum_should_match）
- MySQL 8 + GORM（文件/分片/父子块/活动版本持久化）、Redis 7（分片进度）
- MinIO：分片对象存储；单分片 Copy、多分片 Compose；合并后后台清理分片对象
- Kafka（segmentio/kafka-go）：生产/消费、失败阈值重试、手动提交 offset
- 任务解耦：`TaskProcessor` 接口承载解析/向量化/索引流水线
- 常驻 Document Worker：PDF 使用 Docling/Tesseract OCR；Office 等格式通过 Tika 结构化解析，统一保存 IR 和来源
- 分块策略：父子分块，结构边界零重叠；仅超长不可细分单元使用 Token 限长续片及有限重叠
- Elasticsearch 8：KNN 语义召回 + BM25 rescore + 短语兜底 should；索引含 `userId/orgTag/isPublic`
- Embedding：OpenAI 兼容协议，统一配置 Qwen3-Embedding-0.6B / 1024，需提供匹配服务和固定 tokenizer 版本
- LLM：DeepSeek Chat 流式；可按同协议切换本地 Ollama
- Docker 容器化：主 Compose 包含 MySQL/Redis/ES/Kafka/MinIO/Tika/Document Worker
- 集中管理 LLM/Embedding/ES 等参数

它的目标是帮助企业和个人更高效地管理和利用知识库中的信息，支持多租户架构，允许用户通过自然语言查询知识库，并获得基于自身文档的 AI 生成响应。

![派聪明的前后端](https://cdn.tobebetterjavaer.com/stutymore/README-20251027092633.png)

系统允许用户：

- 上传和管理各种类型的文档
- 自动处理和索引文档内容
- 使用自然语言查询知识库
- 接收基于自身文档的 AI 生成响应

## Java版派聪明的成绩

派聪明 Java 版是 8 月份上线的，截止到目前，已经取得了非常瞩目的成绩，我这里晒一下哈。

![面渣逆袭+派聪明 拿下招银网络+科大讯飞](https://cdn.tobebetterjavaer.com/paicoding/03b3016a1c6dc9659fbc7791bca55ccd.png)

![](https://cdn.tobebetterjavaer.com/paicoding/2ad94e8464c1be3cd3b8fee947c2775c.png)

![腾讯后端拿下，多亏派聪明+技术派](https://cdn.tobebetterjavaer.com/paicoding/b1b3a12367cfc625311bd175774d49fe.png)

![网易拿下，多亏派聪明和面试官有的聊](https://cdn.tobebetterjavaer.com/paicoding/22b4c5e7b760f3be89315885438b9c16.png)

![球友们对派聪明发自内心的认可](https://cdn.tobebetterjavaer.com/paicoding/c460dcb29244ec470106763f48c1d087.png)


说句真心话，看到这，就可以无脑冲这个项目了，因为这些，还只是冰山一角。扫下面的优惠券（或者长按自动识别）解锁派聪明源码和教程吧，[星球](https://javabetter.cn/zhishixingqiu/)目前定价 159 元/年，优惠完只需要 129 元，每天不到 0.35 元，绝对的超值。

![派聪明优惠券](https://cdn.tobebetterjavaer.com/paicoding/97601d7a337d7d944b02bb4a79cd6430.png)

>派聪明如何写到简历上：[https://paicoding.com/column/10/2](https://paicoding.com/column/10/2)

![派聪明如何写到简历上](https://cdn.tobebetterjavaer.com/stutymore/README-20251027094034.png)

## 后端启动

可 Docker 容器化一键部署前置环境，教程见：[派聪明环境部署教程](https://paicoding.com/column/10/29)。

![Docker 拉取前置环境](https://cdn.tobebetterjavaer.com/stutymore/README-20251027093622.png)

文档处理只支持结构化新链路。先按 [文档处理配置](docs/document-processing-v1.md) 配置 Worker token、固定 tokenizer revision 和对应 Embedding 服务地址，并初始化文档表，再启动主 Compose 和后端。缺少必需配置会直接报错，不会退回旧模式。后端默认监听 8081 端口。

仓库配置不保存凭据。启动 Compose 前必须设置 `MYSQL_ROOT_PASSWORD`、`DATABASE_REDIS_PASSWORD`、`MINIO_ACCESS_KEY_ID`、`MINIO_SECRET_ACCESS_KEY`、`DOCUMENT_WORKER_TOKEN` 和 `DOCUMENT_TOKENIZER_REVISION`，这些变量均无默认秘密值；同一组 Redis 和 MinIO 变量直接供后端使用。默认模型可分别通过 `DOCUMENT_TOKENIZER_ID` 和 `DOCUMENT_EMBEDDING_MODEL` 覆盖，二者必须一致。后端还需设置 `JWT_SECRET`（不少于 32 字符）、`DATABASE_MYSQL_DSN`，以及所选模型需要的 `LLM_API_KEY` / `EMBEDDING_API_KEY`。

一般配置项按“层级中的点替换为下划线”映射环境变量；文档处理配置显式绑定到 Compose 使用的同一组 `DOCUMENT_WORKER_TOKEN`、`DOCUMENT_TOKENIZER_ID`、`DOCUMENT_TOKENIZER_REVISION` 和 `DOCUMENT_EMBEDDING_MODEL`。

系统不创建默认或演示账号。首次部署时先通过注册页面创建管理员本人账号，再由数据库管理员执行以下语句提升该账号；请把占位用户名替换为实际用户名：

```sql
UPDATE users SET role = 'ADMIN' WHERE username = 'your_admin';
```

已有部署需单独删除或重置旧演示账号，并轮换曾写入仓库的数据库、存储、JWT 和模型凭据；修改配置不会撤销泄露的凭据，也不会清除 Git 历史。已有 MySQL 数据卷不会因修改 `MYSQL_ROOT_PASSWORD` 自动改密，须由数据库管理员在实例内完成轮换。升级前按 [文档处理配置](docs/document-processing-v1.md) 应用缺失的迁移；本次代码修改未操作现有数据库、凭据或数据卷。

![后端启动](https://cdn.tobebetterjavaer.com/stutymore/README-20251027093531.png)


## 前端启动

```bash
# 进入前端项目目录
cd frontend

# 安装依赖
pnpm install

# 启动项目
pnpm run dev
```

聊天助手的访问效果如下图所示：

![Go 版派聪明的运行后效果](https://cdn.tobebetterjavaer.com/paicoding/754665f76be3ff5b0a65b684377a4d1e.png)
