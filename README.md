# Broker Resilience Platform

![Go](https://img.shields.io/badge/Go-1.27-00ADD8?logo=go&logoColor=white)
![Python](https://img.shields.io/badge/Python-3.13-3776AB?logo=python&logoColor=white)
![React](https://img.shields.io/badge/React-19-61DAFB?logo=react&logoColor=white)
![License](https://img.shields.io/badge/License-MIT-green)

**面向 RabbitMQ 的故障注入与可靠性评估平台**：通过可配置测试场景、受控故障注入、消息全链路对账与监控指标分析，量化验证消息系统在异常情况下的消息处理可靠性、容错能力与故障恢复能力。

```
定义场景 → 执行测试 → 主动注入故障 → 采集证据 → 核验消息 → 评估结果 → 生成报告
```

## 核心特性

- **受控故障注入**：基于 Docker SDK 与 Toxiproxy 实现消费者终止/暂停、Broker 重启、网络延迟/断连四类故障策略，按生命周期管理（计划—注入—观测—恢复—验证—清理），带超时保护与三重清理兜底；
- **消息全链路对账**：以唯一 `message_id` 贯穿生产、Broker 确认、投递、业务处理、ACK 五个阶段，区分消息重复投递与重复业务副作用，为消息丢失判定提供可追溯证据链；
- **可靠性量化评估**：自动计算最终业务处理率、业务重复处理率、端到端延迟、故障恢复时间、积压清空时间等指标，按场景断言输出 PASS/FAIL 与证据摘要，生成自包含 HTML 报告并支持历史对比。

## 技术栈

| 层 | 技术 |
|----|------|
| 前端 | React 19 + TypeScript + Vite + Ant Design + ECharts |
| 控制面 | Go (Gin + GORM) + PostgreSQL + Redis |
| 执行面 | Python Agent (pytest + FastAPI + pika) |
| 故障注入 | Docker SDK + Toxiproxy |
| 监控 | Prometheus（曲线由前端 ECharts 呈现） |
| 工程化 | Docker Compose + Jenkins Pipeline |

## 快速开始（建设中）

```bash
task install      # 安装前端依赖
task up           # 启动平台基础设施（PostgreSQL / Redis / Prometheus）
task dev:backend  # 启动后端（air 热重载）
task dev:frontend # 启动前端（Vite）
```

## 目录结构

```
├── frontend/   React 前端
├── backend/    Go 控制面（模块化单体，按业务域分包）
├── agent/      Python 测试执行面（Test Agent）
├── lab/        实验资产（pytest 场景库 + 实验辅助程序）
├── deploy/     部署资产（Compose / Nginx / Prometheus）
└── docs/       工程文档（协议冻结件、架构图、环境说明）
```

## 开发约定

- 分支模型：`main`（可演示）+ `dev`（日常开发），里程碑节点合并打 tag；
- 提交信息：约定式格式 `feat/fix/test/chore(scope): 描述`；
- 换行符：仓库统一 LF（见 `.gitattributes`）；
- 代码检查：`task lint`（golangci-lint / ruff / ESLint）。

## 路线图

| 阶段 | 内容 | 时间 |
|------|------|------|
| M0 | 工程骨架 + Compose 基础环境 + 登录 | 2026-11 |
| M1 | 消息测试闭环 + 协议冻结 | 2026-12 |
| M2 | 任务调度与 Agent 闭环 | 2027-01 |
| M3 | 四类故障注入与恢复 | 2027-01 |
| M4 | 监控评估 + HTML 报告 | 2027-03 |
| M5-M6 | 场景扩充 + CI 完整化 | 2027-04 |

## License

[MIT](./LICENSE)
