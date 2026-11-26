# 验收记录

## 2026-11-25 · 原创定价夹具

Go 1.26.0、MySQL 8.4.13、PHP 8.2.30、Composer 2.10.0。使用临时 MySQL，仓库文件留在原目录，测试只在隔离复制目录运行由项目提供的样例文件。

| 项目 | 观察结果 |
| --- | --- |
| MySQL 前版升级 | 从 v1 升至 v2，之前签发的工作区令牌有效 |
| 依赖链 | 两个旧插件均阻断框架升级，完整目标解出四个 v2 元包 |
| 锁文件 | 真实 Composer 更新、校验及 dry-run 安装通过 |
| 归因 | 别名和全限定调用定位准确，同名替代类只待核 |
| 业务回归 | 全量规则替换使负数信用单测试失败，采纳被拦截 |
| 修正审核 | 人工选择销售调用后通过，未采纳前置批次时无法采纳后继 |
| 并发回执 | 同键的第二次请求收到进行中，最终只有一次 Composer 执行 |
| 数据隔离 | 其他租户读取计划、任务、补丁和记录均得不到数据 |
| 撤回 | 确认后撤回前置和后继，独立的货币格式补丁仍可导出 |
| 旧证据 | 基线往返后，旧报告无法重新放行 |
| 补丁 | 三个批次合并导出，标准 `git apply --check` 通过 |

端到端生命周期测试耗时 59.76 秒。这个结果来自 Windows 上的真实 PHP 与 Composer，执行的文件来自仓库内的可信夹具。

## 2026-11-26 · 批量审阅与中断续跑

在独立 MySQL 8.4.13 中执行迁移 1、2、3，原先签发的凭据仍有效。完整生命周期重跑用时 59.35 秒，Composer 与 PHP 测试继续拦住信用单回归。审阅者保存三项决定，提交第一项后中断。另一数据库连接读到一项已记录、两项未开始。并发续跑后只保留一条生效审计，另两项分别保留需要撤回已选中候选和找不到候选的原因。同键换内容返回冲突。同工作区其他操作者、其他租户和验证员均无法越权执行。基线切换再切回时，较早修订的所有待执行项被拦截。

用已发布的 `v1.0.0` 建立计划、补丁和验证报告，从迁移 2 升至 4，再恢复到第二个独立数据库。原凭据、基线、补丁与报告摘要一致，原计划可按目标版本查到。在 Chrome 无界面会话中丢弃保存和执行回执。恢复连接后仍能查询原选集，最终记录一项生效和一项拒绝，数据库只有一条生效审计。原工作区维持未执行状态，跨租户查询、下载和导出返回 404。 [升级前后记录](results/upgrade-rehearsal.json) · [浏览器记录](results/review-browser.json) · [审阅截图](results/review-browser.png)

将上一发行版可执行文件和证据目录通过 `-PreviousCLI`、`-EvidenceDirectory` 传给 `scripts/test-mysql.ps1` 可以复现升级演练。浏览器脚本需要 Node.js 和 Playwright。可通过 `UPGRADE_REHEARSAL_PLAYWRIGHT` 指向 Playwright 模块，也可以使用本地安装的模块。设置 `PLAYWRIGHT_CHROMIUM_EXECUTABLE` 可指定 Chromium 或 Chrome。

## Laravel 综合验收

在 PHP 8.4.28、Composer 2.10.0 和独立 MySQL 8.4.13 中，使用真实 Laravel 12.74.0 与 13.44.1。仅升级框架时，实际 Composer 同时报出两个旧插件的限制。计划按依赖批次、调用批次排序。依赖批次用离线缓存生成锁文件并通过验证。全量替换改坏负数信用单，工作台拒绝采纳。审阅者保留信用单原调用后验证通过，导出只包含已采纳的两批。原生测试覆盖应用的真实存储和采纳逻辑，默认 Docker 镜像用于定价元包夹具。 [测试与对象摘要](results/laravel-rehearsal.json) · [复现步骤](../examples/laravel-pricing/MIGRATION.md)

## 逐条验收

| 用例 | 实测结果 | 可复核证据 |
| --- | --- | --- |
| AC-01 | 两次使用同一基线和配置分析，返回同一计划摘要 | `TestWorkspaceLifecycle` |
| AC-02 | 浏览器预览移除锁文件。其他维护者抢先更新后，旧确认被拦截，显示差异后才完成确认 | [浏览器记录](results/review-browser.json) |
| AC-03 | Composer 和计划均定位框架升级被旧插件限制的依赖链 | `TestWorkspaceLifecycle` 和 [Laravel 记录](results/laravel-rehearsal.json) |
| AC-04 | 移除基线锁文件，计划返回 `missing_lock`，依赖批次无法执行 | `TestWorkspaceLifecycle` |
| AC-05 | 浏览器打开命中项并高亮源文件的准确行 | [浏览器记录](results/review-browser.json) |
| AC-06 | 别名及限定类名可准确解析，不同类型的同名调用进入待核 | `TestHoldout/unrelated-short-name` |
| AC-07 | 演练先采纳依赖再采纳调用。未采纳依赖时下游无法生效 | `TestWorkspaceLifecycle` 和 `TestLaravelPricingLifecycle` |
| AC-08 | 冻结用例中存在冲突的传递约束，求解无可执行候选，实际 Composer 同样拒绝 | `TestHoldout/transitive-unsatisfied` |
| AC-09 | 规则候选只有预期调用文件的改动 | `TestWorkspaceLifecycle` |
| AC-10 | 将 `composer.json` 修改放进调用批次时拒绝整个候选 | `TestPatchRejectsConfigurationOutsideCallBatch` |
| AC-11 | 全量替换导致信用单回归，审阅子集用同一断言通过 | [Laravel 记录](results/laravel-rehearsal.json) |
| AC-12 | 验证工具不可用时报告阻断，未满足的平台扩展被实际 Composer 拒绝 | `TestWorkspaceLifecycle` 和 `TestHoldout/platform-extension-missing` |
| AC-13 | 撤回依赖及后继后，独立货币格式批次仍可导出 | `TestWorkspaceLifecycle` |
| AC-14 | 前置候选被拒绝后，其调用批次无法合并 | `TestWorkspaceLifecycle` |
| AC-15 | 同一批 20 个冻结用例保留规则基线及真实模型结果 | [规则记录](results/holdout-static.json) · [模型记录](results/holdout-live.json) |
| AC-16 | 录制建议标记为 `replay`，真实推理报告单列，超范围索引被拒绝 | `TestWorkspaceLifecycle` 和 [模型记录](results/holdout-live.json) |

## 冻结保留集

20 个原创场景耗时 34.59 秒。10 个依赖场景全部通过实际 Composer 复核。静态求解器确认其中 6 个候选、拦截 3 个确定冲突，并将缺少平台扩展证据的一例交还 Composer；Composer 拒绝该安装。10 个 PHP 场景执行独立回归断言，人工复核后的选择均通过。原始报告列出机械替换会破坏业务断言的用例。 [冻结用例原始记录](results/holdout-static.json)

复现命令见 `scripts/test-mysql.ps1`。无数据库时 `go test ./...` 会跳过该生命周期场景。

## 真实模型建议

2026-11-26 使用本地 Qwen2.5-0.5B-Instruct Q4_K_M 和 llama.cpp b12686 复跑同一批冻结用例。使用 6 个 CPU 线程、4096 上下文和单推理槽。20 个场景耗时 34.24 秒，其中 8 个存在可供模型选择的直接调用。8 个响应均通过结构校验，4 个选择与人工标注一致并通过业务回归。其余 4 个场景因模型选中了需保留旧行为的调用而触发回归。8 次请求合计使用 1,217 个输入 token、160 个输出 token，接口累计耗时 4.487 秒。模型建议进入人工复核，采纳继续受确定性规则、业务测试和审批状态约束。 [模型原始记录](results/holdout-live.json)
