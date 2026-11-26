# Laravel 定价升级案例

这个样例用 Laravel 的容器、路由、请求和集合运行一条真实的定价业务断言。原始响应里，订单 100，信用单 −25，应付 75。两个旧版示例插件分别将 Laravel 锁在 12.x。升级插件能解开框架约束，报价与信用单则有不同的语义。

## 体验固定基线

准备 PHP 8.4 和 Composer 2.10，在单独的样例目录安装锁定依赖。保留 `composer.lock`，验证前后使用同一份 `tests/contracts.php`。

```sh
composer install --no-interaction --no-plugins --no-scripts
php tests/contracts.php
```

原始锁文件使用 Laravel 12.74.0。只将 `laravel/framework` 改成 `^13.0`，Composer 会同时报告 `credit-ledger` 和 `price-catalog` 的旧版本约束。样例里的两项 `upgrade-lab/*` 元包为原创实现。

## 在工作台审阅

1. 在升级助手根目录运行 `upgrade-agent capture -root examples/laravel-pricing -ref laravel-pricing-v1 > baseline.json`。将基线导入工作台，再提交同目录的 `plan-options.json`。
2. 计划中的 `dependencies` 将 Laravel 及两个插件作为一个前置批次。生成候选并验证。通过后采纳依赖批次，再打开 `rule-sale-amount`。
3. 直接生成规则候选会一起改两处调用。验证时带上已采纳的依赖候选摘要，确认 `pricing-contract` 检测到信用单金额变为 0。拒绝该候选。
4. 以人工候选选择第 0 项报价调用，保留信用单调用。再次验证和采纳，再从计划导出两批的 Git 补丁。对同一基线执行 `git apply --check` 后，可在新的工作目录应用补丁并复跑定价契约。

实际验收使用 PHP 8.4.28、Laravel 12.74.0 → 13.44.1、Composer 2.10.0 和 MySQL 8.4.13。本例的候选目录固定为已核准的包版本，如果外部仓库出现新版本，需重新核准候选目录。框架兼容性见 [Laravel 13 升级指南](https://laravel.com/framework/docs/13.x/upgrade)。

## 准备验证依赖

在有网络的准备环境，用专用 `COMPOSER_HOME` 和 `COMPOSER_CACHE_DIR` 安装基线，再在第二份可信样例中将 Laravel 与两个插件分别设为 `^13.0`、`^2.0`、`^2.0` 后运行 `composer update --no-plugins --no-scripts`。保留两版的缓存包与 Composer 仓库元数据。默认发行镜像面向离线元包案例。要用 Docker 验证本例，构建包含 PHP 8.4 及 Laravel 所需扩展、Composer 和上述已核准缓存的镜像。将缓存置入 `/opt/upgrade-agent/composer-cache`，然后配置该镜像的不可变 ID。

Windows 上也可以用项目脚本复现原生测试。向 `scripts/test-mysql.ps1` 传入 `-LaravelPhp` 和 `-LaravelComposerCache` 以及原有的 `-MySQLBin`、`-Php`、`-Composer`。测试中的原生执行仅用于本仓库的可信夹具，执行环境与生产的 Docker 验证入口不同。
