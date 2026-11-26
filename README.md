# Upgrade Agent · 代码库升级与迁移助手

Upgrade Agent 帮助 PHP 仓库维护者盘点 Composer 依赖、拆分迁移批次、验证补丁并留下采纳记录。它读取经授权的源文件快照，在独立工作区验证候选，输出标准 Git 补丁和可复核的测试证据。

| 能力 | 可用入口 |
| --- | --- |
| 固定基线 | 命令行读取本地白名单，工作台确认文件摘要和基线修订 |
| 定位阻断 | 声明、锁文件、传递约束链、当前工具链与候选版本 |
| 检查影响 | 指向 PHP 静态调用的文件行号，同名和不确定调用进入待核 |
| 生成补丁 | Composer 锁文件、确定性替换、人工子集、可选模型子集 |
| 运行验证 | 基线与候选分别运行批准任务，记录通过、回归、既有失败和超时 |
| 审阅与交付 | 前置条件校验、采纳、退回、连带撤回和 Git 补丁导出 |

## 启动

需要 Go 1.26、MySQL 8.4 和一个可丢弃的工作区数据库。PHP 和 Composer 由验证镜像提供。也可以先启动工作台，在验证镜像准备好之前查看盘点和补丁。

```bash
go build -o upgrade-agent ./cmd/upgrade-agent
export UPGRADE_MYSQL_DSN='upgrade:CHANGE_ME@tcp(127.0.0.1:3306)/upgrade_agent'
./upgrade-agent migrate
./upgrade-agent provision -tenant workshop -workspace pricing -actor maintainer -role maintainer
./upgrade-agent serve
```

MySQL 账户只需要该数据库的权限。给开发者和测试负责人分别签发 `reviewer`、`verifier` 凭据。把维护者的凭据填入 `http://127.0.0.1:8101/` 的连接表单，或者存放在当前终端的 `UPGRADE_WORKSPACE_TOKEN` 环境变量中。

## 跑通定价样例

```bash
./upgrade-agent capture -root examples/legacy-pricing -ref legacy-v1 > baseline.json
./upgrade-agent request -id pricing-baseline-1 -input baseline.json POST snapshots
./upgrade-agent request GET workspace
```

工作台可以导入 `baseline.json`。基线固定后加载 `examples/legacy-pricing/plan-options.json` 创建计划。计划将框架和两个插件的 Composer 约束作为一个前置批次，将销售报价和货币格式分开。首次把所有旧的报价调用一起替换会打坏负数信用单。用索引 `0` 生成审阅候选后，期望只改动销售报价的调用。

完整操作见 [定价升级演练](docs/pricing-walkthrough.md)。已经跑通的验证和环境见 [验收记录](docs/validation.md)，功能范围和边界见 [原始需求](docs/requirements/01-产品总览-prd.md)。

[Laravel 定价升级案例](examples/laravel-pricing/MIGRATION.md)使用实际的 Laravel 12 和 13、固定的 Composer 依赖以及一条负数信用单业务契约，可复现依赖冲突、回归拦截和人工审核后的补丁导出。

## 验证镜像

```bash
docker build -f deploy/validation.Dockerfile -t upgrade-validation:local .
export UPGRADE_DOCKER="$(command -v docker)"
export UPGRADE_VALIDATION_IMAGE="$(docker image inspect --format '{{.Id}}' upgrade-validation:local)"
./upgrade-agent serve
```

工作进程用固定镜像 ID 启动无网络、只读挂载的容器。批准的 Composer 任务禁用脚本和插件。依赖版本必须来自已审核的本地 Composer 仓库配置或镜像内的依赖准备。候选补丁保存到工作区，原始仓库只在维护者自行运行 `git apply` 时改变。

## 可选模型

管理员可以配置支持 `/v1/chat/completions` 和 JSON Schema 的服务。每个计划单独批准 token 预算，发送的数据是已命中、已授权的调用片段、函数名和该规则的审阅提示。模型返回候选索引，上线前仍需独立回归与人工采纳。 [配置与运行](docs/operations.md)

## 许可

Apache-2.0。第三方依赖见 [THIRD_PARTY.md](THIRD_PARTY.md)。
