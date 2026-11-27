# 多仓库契约升级

## 演示仓库

公开样例是一组虚构报价服务。本地运行时，PHP 提供方先保留 `/v1/quote` 的 `minor` 字段，同时增加 `/v2/quote` 的 `amount_minor`。两个消费仓切换后，末波移除旧路由。样例和业务回归由 [生成脚本](../examples/contract-estate/build-fixtures.ps1) 写入指定的新目录。

```powershell
pwsh ./examples/contract-estate/build-fixtures.ps1 -CLI ./upgrade-agent.exe -OutputDirectory ./.local/quote-estate
```

同目录下 `snapshots.json` 列出七份快照摘要。三个仓分别使用 `https://example.invalid/quote/provider`、`https://example.invalid/quote/php`、`https://example.invalid/quote/go` 作为登记地址。这些地址只标记来源，服务端不会主动克隆仓库。

| 仓库 | 可索引及导出目录 | 基线、暂存版本 | 回归脚本 |
| --- | --- | --- | --- |
| PHP 提供方 | `src, contracts` | `provider-old`、`provider-compat`、`provider-final` | `src/test.php` |
| PHP 消费方 | `src, contracts` | `php-old`、`php-new` | `src/test.php` |
| Go 消费方 | `src, contracts, go.mod` | `go-old`、`go-new` | `src/checkout_test.go` |

## 工作台操作

1. 为每个仓登记一名维护者。仓库负责人赋予索引、模型和导出各自的目录范围。样例的模型范围可以使用 `src`。在运行器一栏填 `isolated-local`。先导入基线，再勾选「暂存版本供演练」导入其余版本。
2. 新建活动，选择「双版本共存」，填写契约集 `quote-v1-v2`、提供方路由脚本 `src/router.php`、两个消费仓及其测试脚本。旧契约填 `http:GET /v1/quote`。冻结后打开策略对比，读回旧签名、响应样例及两个 HTTP 消费点。
3. 用负责人身份按仓生成候选。另一个 `reviewer` 凭据打开候选编号，逐块记录采纳或拒绝及理由。模型候选只允许从已授权的实际变更中选择。需要减少最终补丁范围时，负责人另存目标快照并重新提交差异。
4. `verifier` 提交当前波的联合演练，按编号读回矩阵。环境失败后可重试失败组合，已通过组合的输出摘要继续保留。后续波次会检查消费者能否与兼容、最终提供方运行，并在旧入口仍被使用时拦住关闭兼容期。
5. 仓库负责人导出已审阅补丁及撤回说明，在目标仓应用并提交。用 `capture -ref <实际 Git SHA>` 生成导出范围内的采纳快照，以暂存模式上传，再填写 Git 提交和新快照摘要。内容与演练目标一致才会登记采纳并推进波次。
6. 遇到基线漂移，导出会报告最新摘要。先暂存保留双方改动的新目标，在工作台提交基线重算，再重新生成、评审和验证当前波。其他仓已采纳的回执仍可读。已冻结快照的授权如有变更，应以现有授权重新建立活动。

单仓历史计划从「导入单仓计划」进入。导入会固定原计划、源码基线与已有采纳批次。原审阅记录和报告可从原计划继续查询。

## HTTP 接口

端点均位于 `/api/v1`。写入需要工作区 Bearer 凭据和 `Idempotency-Key`。跨租户查询返回 404，仓库权限不足返回 403，修订或同键内容冲突返回 409。

| 路径 | 用途 |
| --- | --- |
| `POST /estate/repositories` | 登记仓库，固定包名、来源与负责人 |
| `PUT /estate/repositories/{repo}/policy` | 负责人更新索引、模型、导出目录和运行器配置 |
| `POST /estate/repositories/{repo}/snapshots` | 按仓库修订上传 `snapshot`，用 `stage_only` 暂存 |
| `GET /estate/repositories/{repo}/contracts` | 读取当前或指定 `snapshot` 的目录 |
| `POST /estate/impact` | 比对指定生产仓版本与显式固定的消费仓版本 |
| `POST /migration-campaigns` | 新建跨仓活动 |
| `POST /migration-campaigns/legacy` | 导入原单仓计划 |
| `POST /migration-campaigns/{id}/freeze` | 固定兼容波次、权限修订和测试输入 |
| `POST /migration-campaigns/{id}/rebase` | 用 `repository_id`、`current_snapshot`、`target_snapshot` 重算漂移 |
| `POST /migration-campaigns/{id}/candidates` | 按仓生成候选 |
| `POST /migration-campaigns/{id}/candidates/{candidate}/hunks/{hunk}` | 留下独立的逐块审阅决定 |
| `POST /migration-campaigns/{id}/rehearsals` | 用 `campaign_rev`、`wave_id`、`repo_digests`、`contract_set` 和 `runner_profile` 入队，返回 202 和 `Location` |
| `POST /migration-campaigns/{id}/rehearsals/{rehearsal}/retry` | 同一矩阵内重试未通过项 |
| `POST /migration-campaigns/{id}/bundles` | 导出独立批准、测试通过的当前仓补丁 |
| `POST /migration-campaigns/{id}/bundles/{bundle}/adopt` | 用 `adoption_ref` 和 `observed_snapshot` 登记采纳 |

联合演练的组合键包括提供仓版本、消费仓版本、回归脚本和运行器。回执包含镜像标识、依赖锁摘要、命令、耗时、退出码、有限失败摘录及输出摘要。工作线程续跑队列时使用有时限的租约，过期工作线程无法覆盖最新结果。

## 运行条件

HTTP 契约在 `contracts/*.http.json` 中声明，消费方同时声明调用文件与行号。索引可确定定位静态 PHP 或 Go 函数，语言反射、间接方法及动态目标列为待确认。仓库登记用于来源和授权校验，不会推断依赖关系。快照只读取符合白名单的文本文件，打包的二进制、模块目录和 Git 对象目录均不纳入源码分析。

演练镜像需要 PHP、Composer 和 Go。容器使用只读源码输入、无网络及专用临时目录。Go 样例只用标准库；缺失已锁依赖记为执行失败。模型配置仅支持回环服务的联合契约建议。仓库实际合并由负责人在外部 Git 执行，工作台校验其上传的已采纳快照和对应提交号。 [部署与恢复](operations.md)
