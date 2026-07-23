# Task 13 实施报告：长期记忆验收门禁

日期：2026-07-23

基线：`bde8005`

分支：`codex/long-term-memory-v1`

## 结论

Task 13 已完成实现和本地验收。固定版本评测语料、真实 PostgreSQL Host
黑盒、pgvector CI 门禁和运维文档已经落地。所有要求使用 PostgreSQL 的
测试均在 `MEMORYKIT_REQUIRE_POSTGRES=1` 下通过，没有 SKIP。

本地验收环境：

- PostgreSQL `16.14`
- pgvector `0.8.2`
- 数据库端点：`127.0.0.1:58603/memorykit_test`

## TDD：RED 证据

### 评测 API

先写 `EvalCase`、`EvalReport`、`Evaluate` 和固定语料测试，再执行：

```text
cd memorykit
go test ./... -run TestEvaluate
```

初始失败为 `EvalCase`、`Evaluate`、`EvalReport` 未定义。实现后测试通过，
并额外固定了以下非法输入：

- 重复 case ID；
- expected / forbidden 集合重叠；
- 分母为零时比率为零。

### 真实 PostgreSQL Host 黑盒

先创建黑盒入口并调用尚不存在的
`runHostMemoryPostgresBlackBox`，编译按预期失败。

黑盒骨架完成后的第一个业务 RED 是显式写入 workflow 最终 Artifact 返回
`read completed`，而不是 `write completed`。原因是生产 Agent 收到的是
稳定 Artifact 引用，不是原始输入正文。黑盒 fake LLM 随后改为根据稳定
workflow 引用编排，同时继续通过生产工具完成 search/read。

加强“Agent B 首次 LLM 请求必须自动包含同项目持久记忆”断言后，再次得到
真实 RED：首次请求只包含 `Review input artifact artifact:<workflow>:input`，
没有持久记忆。根因是自动召回的默认 query builder 正在查询 Artifact 引用，
而不是当前用户输入。

经确认采用 A1 后：

1. 先写 `Recaller.MaxQueryRunes()` 测试，初始编译失败；
2. 先写 Host Artifact-backed query builder 测试，初始编译失败；
3. 实现只读 getter，以及仅在生产 workflow 稳定引用标记存在时读取
   `text/plain` 输入 Artifact 的瞬时 query builder；
4. 对引用不一致、错误 content type、无效 UTF-8、空白、NUL、超限和
   Artifact 错误全部 fail closed；
5. 保留 context cancel/deadline，其他错误统一为不含正文的稳定错误；
6. 直接 runner 仍完整委托 `DefaultQueryBuilder`；
7. Artifact 正文不写入 request、metadata、checkpoint 或消息。

加强后的黑盒证明：同项目新 session 的第一次 LLM 请求在任何 search 工具
调用前已包含持久记忆。

### 测试库污染回归

第一次全量 real-PG race 在
`TestEmbeddingPendingPutCorrectAndErase` 失败。只读 SQL 确认存在 3 条
历史 `host-handler-*` active 测试记录、6 条 source、6 条 revision、0 条
embedding。根因是 Host 真实 PG handler 测试创建唯一租户后没有清理，而
`PendingEmbeddings` 按 embedding profile 扫描全库 active 待处理项。

没有放宽 pgstore 断言，也没有改变 worker 的全库语义。修复为 handler
测试在 create 成功后立即注册基于同一 Scope/ID 的 public `Get + Erase`
`t.Cleanup`，按实际版本清理，错误使用 `t.Errorf`。已确认的 3 条旧测试
记录按精确 ID/tenant 删除，回查 memory/source/revision/embedding 均为 0。
后续重复运行结束时没有 active、非空正文或 source 残留；保留的 inactive
空内容审计墓碑符合 Erase 语义。

## 固定语料与四策略结果

语料版本为 `project-memory-v1`，包含 6 条 memory 和 4 个 case。所有策略
使用同一份语料、相同预算和相同 forbidden 集合：

| 策略 | Cases | ExpectedFound / Total | ForbiddenInjected / Total | Recall@budget | False injection |
|---|---:|---:|---:|---:|---:|
| exact-only | 4 | 1 / 2 | 0 / 4 | 0.5 | 0 |
| FTS-only | 4 | 0 / 2 | 0 / 4 | 0 | 0 |
| vector-only | 4 | 2 / 2 | 0 / 4 | 1 | 0 |
| fused | 4 | 2 / 2 | 0 / 4 | 1 | 0 |

`fused` 恢复了 semantic-only case，且没有注入其他项目、candidate 或 inactive
negative。该 harness 用于固定回归，不宣称等价于 PostgreSQL 的生产排序；
真实 PostgreSQL 黑盒另行覆盖 vector 和 FTS 降级路径。

## 真实 PostgreSQL 黑盒覆盖

`TestHostMemoryPostgresBlackBox` 通过生产 `NewServer` workflow/Agent 路径，
使用真实 `pgstore`、确定性 3D Embedder、认证 project authorizer 和 fake
LLM，覆盖：

- Agent A 显式写入，Agent B 新 session 同项目首轮自动召回；
- 不同 project 和 tenant 隔离；
- Store 关闭并重开后的持久化；
- 真实 embedding 行写入和删除；
- embedding 行删除后仍可经 FTS 查询召回；
- candidate 隐藏、activate、correct、forget、erase；
- erase 后正文、source 被清除，revision 标记 `content_erased`；
- 内容校验拒绝时只返回“未保存”，数据库没有该记录；
- 恶意记忆不能增加写工具权限，也不能跨 Scope read；
- Host 对请求注入 `subject_type=user` 返回 HTTP 400。

黑盒按唯一 tenant 清理。最终回查 `host-blackbox-*` 记录为 0。

## CI 与文档

`.github/workflows/ci.yml` 新增 `memory-postgres` Linux job：

- `pgvector/pgvector:0.8.2-pg16-bookworm`；
- `MEMORYKIT_REQUIRE_POSTGRES=1`；
- memorykit 全量 race；
- Host PostgreSQL 黑盒 race；
- 保留既有 macOS verify job。

新增 `memorykit/README.md`，并更新根 README 与 Host README，明确：

- V1 仅 project Scope，user Scope 未来禁用；
- Limits、RecallPolicy、Embedder、authorizer 都是显式依赖；
- 自动召回和按需召回边界；
- active 直接写与 candidate 审核路径；
- exact/FTS/vector 的错误与降级语义；
- migrate、embedding rebuild、forget、erase、audit 的差异；
- 可直接运行的 required PostgreSQL 命令；
- 不提供 nil/default authorizer 的生产配置；
- 明确非目标。

`scripts/verify-all.sh` 在基线中已经包含 memorykit 测试，因此没有制造无意义
diff。

## 依赖收口

按计划执行 `go work sync` 后，workspace MVS 一度把依赖变化扩散到 30 个
无关 `go.mod`、`go.sum` 和 workspace 文件。该 broad diff 已通过可审计
反向 `apply_patch` 全部移除。

随后独立执行：

```text
cd memorykit
GOWORK=off go mod tidy -diff

cd ../examples/host-api
GOWORK=off go mod tidy
GOWORK=off go mod tidy -diff
```

最终结果：

- memorykit tidy diff 为零；
- Host tidy diff 为零；
- 唯一依赖 diff 是 `github.com/jackc/pgx/v5 v5.7.6` 从 indirect 移到
  direct，因为黑盒测试直接导入 pgx stdlib driver；
- `go.sum`、`go.work`、`go.work.sum` 和所有无关模块无 diff。

## 最终门禁

在最小依赖基线下重新执行，结果如下：

```text
cd memorykit
GOWORK=off MEMORYKIT_REQUIRE_POSTGRES=1 ... go test -count=1 -race ./...
ok github.com/eruca/goagents/memorykit
ok github.com/eruca/goagents/memorykit/agentadapter
ok github.com/eruca/goagents/memorykit/memorystore
ok github.com/eruca/goagents/memorykit/pgstore

GOWORK=off go vet ./...
PASS

cd ../examples/host-api
GOWORK=off MEMORYKIT_REQUIRE_POSTGRES=1 ... \
  go test -count=1 -race -run '^TestHostMemoryPostgresBlackBox$' ./...
ok github.com/eruca/goagents/examples/host-api

GOWORK=off go vet ./...
PASS

cd ../..
bash scripts/verify-release-layout-test.sh
PASS

bash scripts/verify-release-layout.sh
PASS

MEMORYKIT_REQUIRE_POSTGRES=1 ... bash scripts/verify-all.sh
PASS

git diff --check
PASS
```

required memory PostgreSQL 测试没有 SKIP。`verify-all.sh` 中既有
OpenAI-compatible 示例因未配置外部服务而打印自身的可选 Skip，不属于
memory PostgreSQL 门禁。

## 敏感输出与设计一致性审计

- verbose 黑盒输出经过 durable/foreign/tenant/malicious/bearer 合成哨兵和
  vector 文本匹配，匹配数为 0；
- 合成哨兵只存在于测试文件，不存在于生产文件；
- 新增生产路径没有日志输出正文、query、vector、Authorization 或 Artifact
  body；
- 无效 workflow query 错误是稳定的无正文错误；
- candidate、inactive、expired 的排除由 recall/pgstore 回归测试覆盖；
- other-project、other-tenant 和恶意跨 Scope 由真实黑盒覆盖；
- `goagent/ports` 与 `goagent/agentcore` 相对基线无 diff；
- PostgreSQL migration 中没有 HNSW/IVFFlat ANN index；
- Host V1 user Scope 注入由真实黑盒固定为 HTTP 400。

## 剩余明确非目标

- 不提供网络 memory service；
- 不提供生产管理 UI；
- 不创建 ANN 索引；
- 不复制 todo/workflow/live state 正文作为记忆；
- 不自动把 candidate 提升为 active；
- 不支持 Host user Scope；
- 不设通用生产延迟 SLA；
- 不在本任务合并、推送、删除 worktree 或删除 PostgreSQL 容器。
