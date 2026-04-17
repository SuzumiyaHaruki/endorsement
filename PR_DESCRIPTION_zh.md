# 支持面向云服务器的 endorsement 实验改造

## 变更概述

这个 PR 对 Nitro 中与 endorsement 相关的代码做了整理和补充，使其更适配后续的 4 台云服务器实验部署模式：

- 1 台主服务器运行 sequencer / Nitro 主节点
- 3 台背书服务器分别运行 endorser A / B / C

本次改动的目标，是让 Nitro 侧的运行逻辑与 `nitro-testnode` 中新的云端实验脚本保持一致。

## 主要改动

- 完善 `cmd/endorser` 的 handler 初始化与错误处理
- 梳理并补充 `executionengine` 中 endorsement 相关流程的注释与逻辑说明
- 梳理并补充 `sequencer` 中候选块、背书策略、重建流程相关注释与逻辑说明

## 改动原因

原先的实验环境更多依赖单机 Docker 拓扑。
现在需要把实验部署迁移到 4 台云服务器：

- 主节点负责出块和远程发起背书请求
- A/B/C 三个背书节点分别独立部署

因此需要保证 Nitro 内部的 endorsement 流程在这种部署方式下仍然清晰、稳定，并与实验脚本的运行方式一致。

当前整体流程保持不变：

- sequencer 为每笔交易解析 endorsement policy
- execution engine 在区块提交前向远端背书节点请求背书
- 若背书失败，则触发 candidate block 重建

## 关联说明

这个 PR 需要配合 `endorsement-testnode` 仓库中的对应改动一起使用。
后者主要负责把实验脚本从单机 Docker 模式迁移到多机云服务器模式。

## 验证情况

- 已完成本地代码整理并提交
- 已与配套的 `nitro-testnode` 分支联动验证脚本和流程接线
