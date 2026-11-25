# 定价升级演练

本例使用原创的微型 PHP 项目和四个本地 Composer 元包，离线即可解析依赖。生产仓库可替换为自己的依赖目录和测试脚本。

1. 创建并固定样例快照。把 `plan-options.json` 中两个插件的目标删掉，只保留框架 `^2.0` 后创建计划。阻断链应指明旧插件引用了 contracts v1。
2. 使用完整计划选项重新创建计划。先在依赖批次里调用 Composer 生成锁文件，再运行验证并采纳。验证比较基线和候选的 Composer 可重复性和 PHP 契约。
3. 在 `rule-sale-price` 生成规则候选。把已采纳的依赖候选摘要填入前置栏，运行验证。报告应显示负数信用单回归，并拦截采纳。
4. 从审阅者终端生成只选择第一处的候选。验证通过后采纳。

```bash
printf '%s\n' '{"source":"reviewer","indices":[0]}' |
  ./upgrade-agent request -id pricing-reviewed -input - POST "plans/$PLAN/batches/rule-sale-price/candidates"
```

5. 独立处理 `rule-currency-display`，运行验证并采纳。工作台可以导出包含完整补丁、摘要和验证证据的 JSON。
6. 撤回依赖批时，界面返回受影响的 `rule-sale-price`。确认后再导出，结果应只包含独立的货币格式批次。

补丁应用前可执行 `git apply --check migration.patch`。演练使用单独的 checkout 或副本保存改动。
