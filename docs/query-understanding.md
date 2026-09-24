# 多轮查询理解：运行与验收

## 功能与配置

查询理解发生在检索之前，复用当前 LLM、SearchService 和会话存储：
当前问题 + 有界近期原文 + 增量摘要 + 按需确认记忆 → 指代消解/独立改写/拆解 → 有界检索 → 融合证据 → 一次最终回答。
不新增 Agent 框架。会话归档、摘要及用户长期记忆的建表与升级步骤见 [conversation-memory.md](conversation-memory.md)。

在 `configs/config.yaml` 配置：

```yaml
query_understanding:
  enabled: true
  timeout_seconds: 10
```

修改后重启后端。关闭 `enabled` 会恢复原问题的单次检索；会话隔离、取消、权限过滤仍需保留。
`timeout_seconds` 控制每次规划/依赖绑定调用，不是整轮回答时限；小于等于零时使用 10 秒。
该阶段复用现有模型，温度为 0，输出上限 1024 tokens；不保证所有模型都能稳定遵守 JSON 指令。

## 执行契约

- **指代与改写**：使用最多最近 6 个完整问答对并受 `memory.recent_tokens` 预算限制（默认保守估算 4000）；更早原文增量压缩成摘要，按需召回用户确认记忆，不截断半条消息凑近期窗口。保留否定、时间、版本、比较对象、过滤条件和输出要求，用户纠正优先，换话题不继承旧主体。历史助手消息只用于识别讨论对象，不是本轮事实证据。
- **澄清**：有多个合理指代或必要历史已丢失时先问澄清，不猜测。澄清使用现有 `chunk + completion`，按原问题 + 澄清回答保存；下一轮的“云桥”“前一个”等短回复应恢复完整查询。预算不足时请用户缩小范围。
- **有界拆解**：最多 3 个步骤、2 层依赖、3 路并行检索。简单问题只有 1 步；并列步骤可并行，第二层必须等待第一层证据，不能依赖第二层。第二层绑定合并为一次额外模型调用，不递归生成新步骤。
- **证据绑定**：依赖问题用 `{{step:1}}` 等占位符保留未知实体；绑定模型只返回 `id + evidence[{source_id, quote, value}]`，或澄清/无证据状态，不允许返回任意新问题。Go 校验来源 ID、逐字引文和实体值后替换对应占位符；每个直接前置步骤都要有证据，同一前置步骤的证据必须给出一致值。没有证据就停止该分支，多种合理对象就澄清，不能把历史助手猜测当作姓名或其他实体。引文存在不等于推论正确，语义正确性仍需评测。
- **检索与融合**：简单问题 Top 10，每个子问题 Top 5；单路检索最多 30 秒，最终最多 12 个分块。用 `(UserID, FileMD5, ChunkID)` 去重，用 RRF 排名融合，保留依赖证据和各分支候选，再统一引用编号；前端沿用 `(来源#编号: 文件名)` 格式。每次检索均使用当前用户的原有权限过滤。
- **会话与取消**：一轮固定会话 ID 和带版本的上下文快照，完成后在 MySQL 追加原文并 CAS 更新状态；迟到旧版本拒绝保存。Redis 仅为最近 20 条的可丢弃缓存，续期 7 天；SQL 原文不随缓存过期删除。当前同用户活动请求互斥针对单个后端实例，多实例一致性由数据库事务/CAS 保证，但不会避免重复模型开销。停止请求同时取消摘要、规划、检索和生成；换号/退出时关闭前端连接并清空会话状态。

内部规划格式示例（不会作为聊天内容发到前端）：

```json
{
  "standalone_question": "东辰项目的负责人还负责哪些项目？",
  "steps": [
    {"id": 1, "question": "东辰项目的负责人是谁？", "depends_on": []},
    {"id": 2, "question": "{{step:1}}还负责哪些其他项目？", "depends_on": [1]}
  ]
}
```

需要澄清时 `clarification_question` 为非空问题，`steps` 必须为空。
独立问题、步骤数、连续 ID、向前依赖和最多两层均由 Go 校验；模型不能输出权限、ES DSL 或额外执行字段。

## 失败、回退与观测

- 规划结果必须是一个 UTF-8 JSON 对象；不接受 Markdown 代码块、未知字段、尾随对象或说明、无效步骤。收集响应最多 16 KiB，不输出模型内部规划。
- 规划超时、断流、JSON/计划无效时，本轮返回可重试错误，由用户重试。没有自动修复循环，也不会静默退回带歧义原问题继续检索；功能开关是显式运维回退入口。
- 某条检索失败或依赖绑定失败时，最终回答说明未完成分支，并只依据可用资料回答已完成部分。全部实际检索调用失败时报错；检索成功但零命中属于无资料，不等同服务故障。
- 主线程取消优先结束，不应把取消表现成成功完成。规划、各检索步骤、证据绑定、最终生成和整轮耗时应可区分；观察错误/澄清/部分失败占比与首字延迟，不记录完整私有规划或文档正文到普通运行日志。
- 暂不提供无限多跳、任意多对象枚举、自动提取长期记忆或自动执行操作。长期记忆仅保存用户明确确认的条目。复杂表格/文档解析质量不由此模块解决；查询改写本身也不能补救错误源文档或缺失权限。

## 100 条离线样例与验收

`internal/service/testdata/query_understanding_eval.jsonl` 是手工编写的 100 条中文、虚构业务场景样例，不是生产用户日志，也不是已测正确率。
10 类各 10 条：指代、省略、纠正、换题、约束、歧义、并列、多跳、澄清续问、对抗输入。
日期固定为 `2026-09-03`，保证“今年/去年”标注可重复；真模型评测需要同样固定注入日期，不能用运行当天日期直接比较。

每行字段一致：

| 字段 | 含义 |
| --- | --- |
| `id`, `category`, `current_date` | 唯一编号、类别、评估时间 |
| `history` | 按时间排序的完整 `user/assistant` 消息，只含 `role/content` |
| `query` | 用户原问题，保留口语、省略和对抗文本 |
| `expected.mode` | `single / parallel / multihop / clarify` |
| `expected.subjects` | 独立问题必须正确定位的主体；澄清样例可列候选主体 |
| `expected.conditions` | 必须保留的语义条件或必须遵守的处理边界 |
| `expected.steps` | 参考检索问题及依赖，每步 `id/question/depends_on`；依赖问题使用 `{{step:ID}}` 占位符，澄清为空 |
| `expected.clarification_focus` | 需要澄清什么；不澄清时为空字符串 |

参考问题允许等义改写；不要用逐字相等或简单关键词命中代替语义评分。
`conditions` 中“不继承旧主体”“权限不变”等是行为约束，不要求模型把这些话写入独立问题。
本集验证规划意图，不提供检索文档或多跳绑定的事实答案；绑定、权限和引用正确性还需固定检索桩/实际授权语料验证。

运行 `go test ./internal/service -run TestQueryEvaluationCorpus -count=1` 会校验全部样例的 schema、完整轮次、分类和依赖预算，默认不访问模型。
如需显式启动真实模型规划评测，在 PowerShell 中设置配置文件的绝对路径（会发起最多 100 次模型调用）：

```powershell
$env:PAISMART_QUERY_EVAL_CONFIG = (Resolve-Path configs/config.yaml).Path
go test ./internal/service -run TestQueryEvaluationCorpus -count=1 -v
Remove-Item Env:PAISMART_QUERY_EVAL_CONFIG
```

该入口固定注入样例日期，输出每条实际计划及分类匹配数；结构/传输错误立即停止，不反复请求故障服务。分类匹配不等于语义正确率，主体、限定条件和答案质量仍需按参考标注人工复核。不要把此环境变量长期留在常规测试环境中。

在仓库根目录可先做不调用模型的基本格式检查（PowerShell）：

```powershell
$cases = @(Get-Content -Encoding UTF8 -LiteralPath internal/service/testdata/query_understanding_eval.jsonl | ForEach-Object { $_ | ConvertFrom-Json })
if ($cases.Count -ne 100 -or @($cases.id | Select-Object -Unique).Count -ne 100) { throw 'Expected 100 unique cases' }
foreach ($case in $cases) {
  if (!$case.query -or !$case.category -or $case.current_date -ne '2026-09-03') { throw "Invalid input: $($case.id)" }
  if ($case.expected.mode -notin @('single', 'parallel', 'multihop', 'clarify')) { throw "Invalid mode: $($case.id)" }
  if (($case.expected.mode -eq 'clarify') -ne (@($case.expected.steps).Count -eq 0)) { throw "Invalid clarification: $($case.id)" }
  if (@($case.expected.steps).Count -gt 3 -or @($case.history).Count % 2 -ne 0) { throw "Invalid bounds: $($case.id)" }
}
'100 cases parsed; semantic quality has not been measured.'
```

完整验收分两层：

1. **确定性执行测试**：`go test ./internal/service ./internal/repository ./internal/handler`；覆盖严格 JSON、历史窗口、依赖校验、RRF 去重、澄清不检索、原话保存、权限透传、失败、取消与并发隔离。真实 MySQL/Redis 测试的独立测试库与环境变量见记忆文档；未配置时跳过，不应把跳过记作验证通过。
2. **真实模型对比**：固定模型、提示词版本、日期、历史与可访问语料，各运行旧链路与新链路。逐条记录主体正确、条件保留、澄清必要性、分支覆盖、依赖方向、检索证据和首字延迟；按类别报告分子/分母和失败案例，记录 token 与调用数。100 条数据可作基线，实际业务补充样例后再决定上线门槛。

真实模型调用会把测试输入发给配置的模型服务，并产生对应费用；使用脱敏数据及授权环境。当前文档和语料不宣称已完成真实模型、生产知识库或线上并发评测。
