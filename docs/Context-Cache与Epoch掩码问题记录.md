# Context Cache 与 Epoch 掩码线上观测问题记录

> 记录时间：2026-11-26  
> 关联模块：`internal/agent/observation.go`、`internal/agent/compact.go`、`internal/core/compaction/`、`internal/core/session/session.go`、`plugins/qoder/provider.go`、`internal/modes/rpc/server.go`

---

## 一、背景与已验证成效

在将 E4-A（`ContextLedger` + `Epoch` 冻结掩码 + `recall_entries` / `restore_observation` + `context_metrics` 可观测性）合入 `gp` 并修复以下阻塞问题后：
1. **Skill 目录全量内联导致的 172k+ System Prompt 膨胀**（`internal/core/resources/loader.go` 跳过嵌套文档并增加 `24KB` Progressive Disclosure 阈值）；
2. **Provider 缓存用量字段映射缺失**（`plugins/qoder/provider.go` 与 `internal/ai/providers/openai/openai.go` 补全 `prompt_tokens_details.cached_tokens` / `cache_read_input_tokens` / `cache_creation_input_tokens` / `prompt_cache_hit_tokens`）；
3. **同一 Epoch 内冻结 `ContextLedger` + 收紧抽取提示词**（防止每轮抖动击穿缓存）；
4. **`SessionTree` 畸形工具参数持久化与断链自愈**（`internal/core/session/session.go` 清洗非法 `ToolCalls.Arguments`、对 `Commit` 增加瞬态重试、`NewSessionTree` 桥接历史断链）；
5. **AGP 活跃会话指针持久化**（`internal/modes/rpc/server.go` 通过 `documents` 表 `__rpc_active_sessions__` 持久化 `/new` 与 `/resume` 状态）。

线上实测（`gp.db` 主长会话 `..._sess_81c6c2e3-388f-4b69-8f3b-6ceda630ebec`）表现：
- 起始上下文从 `172,397 tokens` 降至 `19,397 tokens`（`sys_tokens=4691`, `tools_tokens=11785`）；
- 同 Epoch 内 Append-Only 阶段缓存命中率稳定在 **94% ~ 99.5%**（单轮最高 `127,531 / 128,186`）；
- `ContextLedger` 在达到压缩条件（T104）时稳定沉淀为 `v3 / 21afc8a9`（3 条精准约束），并在后续 `Epoch #15 → #17` 期间保持零抖动；
- 上下文组装耗时 `p50 ≈ 0.14ms`，`restore_observation` 在真实长会话中已正常按需触发。

---

## 二、待后续迭代的两个已知可优化问题（暂不修改代码）

### 问题 1：部分上游模型路由下，跨 Epoch 边界时 `CacheRead` 退回至底座 `18,852`

#### 现象描述
对比主会话（`..._sess_81c6c2e3-388f-4b69-8f3b-6ceda630ebec`）前后两段跨 Epoch 边界的 `context_metrics` 记录：

- **前半段（17:42–18:26，`T001~T067`）**：
  - 正常 Append-Only 轮次的 `cache_read_tokens` 按 `64 / 128` token 块对齐；
  - 跨 Epoch 边界时，**能够保留改写点之前全部已冻结的历史消息缓存**（例如 `T066 ep#10` 跨 Epoch 后仍有 `in=95,023`, `read=72,064`, 命中率 `75.8%`）。
- **后半段（18:28–次日 10:38，`T068~T113`）**：
  - 同 Epoch 内的 Append-Only 轮次，`cache_read_tokens` 精确等于 `上一轮 input_tokens - 6`（如 `112,236 → 112,230`、`48,019 → 48,013`）；
  - 一旦跨越 Epoch 边界触发原地工具输出折叠（如 `T077`、`T082`、`T090`、`T095`、`T100`、`T107`、`T113`），`cache_read_tokens` **全都精确跌回 `18,852`**（即仅剩 `System + Tools` 固定底座命中，中间数万至十万级历史消息全部重新计算一次），随后下一轮恢复到 `98% ~ 99.5%`。

#### 根因排查结果
1. **排除本地前缀篡改**：从 `gp.db` `entries` 表回溯还原 `T099`（`freeze=224`）与 `T100`（`freeze=240`）发往模型的完整消息列表，确认 **`win[0..223]` 共 224 条前置消息完全字节一致**，首个变更精确发生在 `win[224]`，本地 `ApplyEpochObservationMaskingWithOffset` 严格保持了冻结前缀不变。
2. **上游路由差异（推断）**：生产配置使用 `model = "auto"`（`plugins/qoder`），`T068` 前后上游隐式路由到的模型/缓存后端行为发生变化——前半段支持逐块 Longest-Common-Prefix（LCP）部分截断命中；后半段路由后的后端仅维护 `System + Tools`（`18,852`）与“上一轮尾部追加前的完整 `messages[]`”缓存，一旦 `messages[]` 中间任意位置发生原地修改（包括 Epoch 折叠或 Compaction），整段 `messages[]` 缓存失效并退回到 `18,852`。

---

### 问题 2：Epoch 步长按 `messages[]` 条数 `16` 切分，真实对话中切换偏密

#### 现象描述
主会话在前 103 轮内跨越了 15 个 Epoch（`T011 ep#1`、`T017 ep#2`、`T023 ep#3`、`T029 ep#4`、`T038 ep#5`、`T043 ep#6`、`T047 ep#7`、`T054 ep#8`……），平均每 **4–7 轮** 触发一次 Epoch 切换。

#### 代码原因
- `internal/agent/observation.go` 中 `DefaultObservationMaskEpoch = 16` 直接作用在扁平 `messages[]` 下标（`globalCutoff / epochStride`）。
- 在 ReAct 工具循环中，每 1 个模型轮次通常产生 `assistant(tool_calls)` + `1~N 个 tool_result`，即单轮增加 `2~4` 条 `ai.Message`，导致仅 `4~7` 轮就会累积满 `16` 条消息而触发跨 Epoch 折叠。
- 结合“问题 1”在上游不支持中间 LCP 分段的后端上的表现，偏密的 Epoch 切换放大了重算 `18,852` 之后历史前缀的频率。

---

## 三、后续优化方向（备忘）

1. **放宽或调整 Epoch 步长单位**：将 `DefaultObservationMaskEpoch` 从 `16` 条消息放宽至 `32~48` 条消息，或改为按真实助手/工具轮次（例如 `16` 个 tool-calling turns）推进，预计可将跨 Epoch 重算频次降低 `2~3` 倍。
2. **显式固定模型对照验证**：在对比测试中避开 `model = "auto"` 动态路由，固定指定支持分段/断点前缀缓存的模型（如 Anthropic `/v1/messages` 带显式 `cache_control` 断点或支持块级 LCP 的模型），验证跨 Epoch 时中间历史前缀保留率。
3. **短窗口与长任务平衡**：当前 `103` 轮长会话即使在每 `5~6` 轮跨一次 Epoch 回退到 `18,852` 的情况下，总命中率仍达 **86.5%**（`5,124,422 / 5,926,957`）；后续若做 PR #2 优化 Epoch 步长，可进一步将长会话总体缓存命中率推高至 **92%+**。
