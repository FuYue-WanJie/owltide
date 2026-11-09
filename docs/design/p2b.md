# P2b M0 设计文档

状态：草案（M0，只设计不实现）
日期：2026-11-09
前置：P2a（sessionlog / decisions / subagent / spill / evolve / jobctl）已上线

---

## 1. Goal / Plan / Todo 文件格式与生命周期

### 1.1 数据结构

三种文件都是 Markdown，frontmatter + 正文，沿用 agents/*.md 的渲染惯例：

```markdown
---
kind: goal | plan | todo      # 文件类型
id: g-20261109-a1f3           # 短 id，newID() 同源
session: 20261109-143230      # 创建会话
status: active | done | blocked | archived
created: RFC3339
updated: RFC3339
---

（正文）
```

- **goal.md**：目标级。正文 = 目标陈述 + 验收标准列表（每条 `- [ ]`，可勾选）+ 关联 plan id 列表
- **plan.md**：计划级。正文 = 步骤列表（有序），每步一个短句 + 可选关联 todo id；头部 `goal:` 字段回指
- **todo.md**：执行级。正文 = 勾选列表；头部 `plan:` 字段回指。现有 `TodoTool`（agent.TodoTool，写 todo.md 单文件）升级为此格式，向后兼容：无 frontmatter 的旧 todo.md 首次写入时补齐

层次关系：goal 1—N plan 1—N todo，全部用 id 字段回指，不做物理嵌套。

### 1.2 写入位置

- 现有 store（config store，`~/.owltide/` 下自进化仓库）新增 `goals/`、`plans/` 子目录；todo 保持 todo.md 原位（spill.Dir 下，会话级）
- goal/plan 属跨会话资产（进 store，受 evolve 的 auto-commit 保护与 `owltide changes` 报告）；todo 属会话工作区（不进 store）
- 会话锁内写入（与 sessionlog 同一 LockSession 约束），无独立锁

### 1.3 生命周期

```
goal: active → done | blocked → archived
plan: active → done（全 todo done 才允许）| archived
todo: 无状态字段，勾选即完成；全勾 → plan 允许置 done
```

- 状态迁移只通过工具（goal_update / plan_update），不允许模型直接改 frontmatter（guard 校验 frontmatter 不可被 run_shell 触碰——已列入 protected files）
- blocked 必须带原因行（正文首行 `blocked: <原因>`），对应 AGENTS.md 停下报告语义
- 新会话启动时 agent 恢复 active goal/plan 的 id 与验收标准到系统注入（复用 [AUTO — not a new user message] 通道，见 config.System 既有语义）

### 1.4 与现有机制的关系

- **sessionlog**：goal/plan 的状态迁移作为 decision 事件记录（decisions.jsonl 已有事件管道），正文变更不进 sessionlog
- **decisions**：迁移事件 `{event: "goal_update", id, from, to, reason}` 
- **subagent**：agent_start 的 task 可引用 `goal:<id>` / `plan:<id>`，manager 的 withContextRefs 把文件正文注入子代理任务前缀（沿用 context_refs 机制，不新增通道）
- **evolve**：goals/plans 是普通 store 文件，受现有 auto-commit 与 ProtectBuiltin 无关（用户可编辑，guard 只锁 frontmatter）

### 1.5 里程碑拆分

- **P2b-M1**：格式 + 读写工具（goal_create/goal_update/plan_create/plan_update，todo 兼容升级）+ guard 保护 + 测试
- **P2b-M2**：会话恢复注入 + context_refs 引用 + decisions 事件 + 测试
- **P2b-M3**：`owltide goals` 列表命令 + store 报告集成

### 1.6 待确认的问题

1. todo 保持会话级（不进 store）是否符合预期？是/否
2. blocked→active 的恢复是否允许模型自主进行（还是必须用户确认）？是/否
3. goal/plan 文件名用 `<id>.md` 还是 `<date>-<slug>.md`（id 进 frontmatter）？A: id.md / B: date-slug

---

## 2. 独立上下文的验收者（verifier）

### 2.1 数据结构

新内置角色 `verifier`（readonly，进 BuiltinRoles，builtinVersion 5）：

```go
{
    Name: "verifier",
    Readonly: true,
    SystemPrompt: "You are the OwlTide verifier. You receive a claim and the
        evidence refs. Re-verify each claim independently: run the read-only
        checks yourself, never trust the claimant's summary. Answer with a
        structured verdict:\nverdict: <confirmed|partial|refuted>\nchecks:\n-
        <what you ran and saw>\ngaps:\n- <what you could not verify>",
    Tools:  ["read_file", "list_dir", "run_shell"],  // 与 review 同级
    Budget: Budget{MaxTurns: 10, MaxTokens: 100000, ToolTimeoutSec: 30},
}
```

关键约束：**verifier 与被验收者不得同源**——router 模式下，同一 plan 的验收委派禁止复用执行该 plan 的会话上下文（新 agent_start 即新上下文，天然满足；禁止向 verifier 传执行者的 summary 作为唯一证据，context_refs 只允许指向原始文件/命令输出）。

### 2.2 写入位置

- 角色定义：~/.owltide/agents/verifier.md（SeedBuiltins 播种，既有 hash 升级机制）
- 验收结果：普通 sub-agent 交付（deliver → 主日志），另加审计事件 `{event: "verified", plan|goal, verdict, sub: <id>}` 到 subagents.jsonl

### 2.3 与现有机制的关系

- **sessionlog / subagent**：完全走现有 agent_start → runSession → deliver 管道，verifier 只是角色；无新代码路径
- **decisions**：verified 事件供审计对账（可与 goal 状态迁移联动：plan 置 done 前要求存在 verdict=confirmed 的 verified 事件——见待确认问题 5）
- **jobs**：verifier 可用 run_shell（只读 gate），可用 job_output 读取执行者后台作业的残留输出作为证据

### 2.4 里程碑拆分

- **P2b-M4**：verifier 角色 + 描述/schema 改动 + 测试（角色形状、只读 gate 复用）
- **P2b-M5**：verified 审计事件 + plan→done 联动校验（若问题 5 确认）+ 测试

### 2.5 待确认的问题

4. verifier 是否允许 parallel（验收多个独立 claim）？是/否
5. plan 置 done 是否强制要求 confirmed 的 verified 事件（硬门槛）？是/否（建议：M0 先软提示，M5 再收紧）
6. verifier 的 run_shell 是否保持只读 gate（不能装依赖跑构建）？是/否

---

## 3. 上下文压缩

### 3.1 数据结构

两层压缩，均落盘可恢复（与 spill 同哲学）：

- **Turn 级（已有）**：spill.Store 已把超长工具输出落盘、主上下文留摘录 + spill 引用；P2b 不动
- **会话级（新增 compaction）**：当主上下文 token 数超过阈值（config 新字段 `budget.compact_at`，默认 max_tokens 的 70%），把最旧的已投递子代理结果对 + 已完成 turn 的工具记录折叠为一条 summary 消息

```go
// core/compaction/compaction.go
type Record struct {
    At        time.Time `json:"ts"`
    Span      []int     `json:"span"`       // 被折叠的消息序号区间（sessionlog 行号）
    Summary   string    `json:"summary"`    // 折叠摘要（模型生成或模板）
    Kept      int       `json:"kept"`       // 保留的最近消息数
}
```

- 折叠摘要本身写入 `~/.owltide/compaction/<session>.jsonl`（一行一 Record），主日志不动——**sessionlog 仍是唯一事实源**，压缩只影响内存中的请求前缀
- 摘要生成策略：M0 用模板拼接（每被折叠 turn：user 首句 + 最终 reply 首句 + 工具调用名列表），不烧 token；模型自摘要留待后续里程碑

### 3.2 写入位置

- compaction 记录：~/.owltide/compaction/<session>.jsonl
- 阈值配置：config.yaml `budget.compact_at`（0 = 关闭，默认 0.7 比例）
- 恢复：--resume 时按 compaction 记录重建折叠前缀（sessionlog 全量重建后应用 Record）

### 3.3 与现有机制的关系

- **sessionlog**：只读其内容，不修改；重建顺序 = sessionlog 全量 → 应用 compaction 折叠 → 恢复出的历史与压缩时内存一致
- **spill**：工具大输出已在 spill（摘录+引用），compaction 折叠时引用保留（折叠消息里保留 spill ref 列表，模型仍可 spill read 取回全文）
- **prefix hash**：折叠发生在 turn 边界，折叠后前缀 hash 变化一次——providers 缓存失效一次是可接受的（与 RestoreHistory 同理）

### 3.4 里程碑拆分

- **P2b-M6**：阈值检测 + turn 边界折叠 + compaction 记录落盘 + 模板摘要 + 测试
- **P2b-M7**：--resume 重建应用折叠 + 测试
- **P2b-M8**：（可选，待确认）模型自摘要

### 3.5 待确认的问题

7. compact_at 默认 0.7 合适吗？是/否（填数值）
8. 模板摘要在 M0 是否足够（不引入模型自摘要的 token 成本）？是/否
9. 折叠是否需要用户显式开关（config 默认开还是关）？A: 默认开 / B: 默认关

---

## 4. 跨会话记忆

### 4.1 数据结构

```markdown
---
kind: memory
id: mem-<date>-<rand>
scope: project | user          # project 绑定工作目录哈希；user 全局
tags: [owltide, go]
created: RFC3339
updated: RFC3339
source: session:<id>           # 产生该记忆的会话
---
<一句话记忆正文（<= 2 句）>
```

- 记忆文件：`~/.owltide/memories/<scope>/<id>.md`（目录已存在于 runtime state，当前为空壳）
- 注入：系统提示尾部追加「Relevant memories」块，按 tags 与当前任务词重叠度选 top-K（K=5，M0 用词面匹配，无向量）；注入内容进前缀 hash，故**每会话只注入一次**（会话开始时定型，中途不换）

### 4.2 写入位置

- 工具 `memory_save`（主代理 registry，白名单进 router AllowedTools）：正文 + tags 必填，scope 默认 project
- 工具 `memory_forget`（按 id，soft delete → status: archived frontmatter 字段）
- 去重：同 scope 下正文 sha256 相同则拒绝（幂等）

### 4.3 与现有机制的关系

- **evolve/store**：memories/ 在 store 内，auto-commit 覆盖，`owltide changes` 可见（用户可审查模型记了什么——这是刻意的透明性）
- **sessionlog**：memory_save 调用本身作为普通工具记录进主日志
- **guard**：memories/ 加入 protected files（run_shell 不可绕过工具直接改）

### 4.4 里程碑拆分

- **P2b-M9**：memory_save/forget + 注入（词面 top-K）+ 去重 + guard 保护 + 测试
- **P2b-M10**：（可选，待确认）`owltide memories` 列表/清理命令

### 4.5 待确认的问题

10. 注入用词面 top-K（M0）还是直接全量注入前 N 条（更简单）？A: 词面 / B: 全量前 N
11. memory_save 是否需要用户确认（防止模型记错东西）？是/否
12. memories 是否允许子代理写（建议：只读，主代理汇总后写）？是/否

---

## 5. 模型参数自动匹配（待用户确认是否纳入）

### 5.1 动机

config.yaml 只配 model.name + base_url；context window / max output / 图像 / 工具调用能力全靠用户手填或猜。配错的表现：上下文截断静默丢历史、max_tokens 超限报错、不支持 tool call 的模型死循环。

### 5.2 查找顺序（四级回退）

```
1. 用户配置        config.yaml model.limits.{context_tokens,max_output,images,tools}
2. 端点 /models    GET {base_url}/models → 元数据（若端点返回 context_window/max_output 等字段）
3. 内置表          内置常见模型族的已知参数（编译进二进制的小表，键为模型名前缀匹配）
4. 保守默认值      context 32768 / max_output 4096 / images=false / tools=true
```

- 每级结果记录到 decisions（`{event: "model_limits", source, ...}`），启动日志打印一行来源，便于排查
- /models 探测失败（网络/不支持）静默降级到下一级，不阻塞启动

### 5.3 数据结构

```go
// core/config/modellimits.go
type ModelLimits struct {
    ContextTokens int   `yaml:"context_tokens"`
    MaxOutput     int   `yaml:"max_output"`
    Images        bool  `yaml:"images"`
    Tools         bool  `yaml:"tools"`
    Source        string `yaml:"-"` // user | endpoint | builtin | conservative
}
```

- 用途：budget 计算按比例取 context（compaction 阈值、token budget 上限校验）；images=false 时 multimodal 工具不注册；tools=false 启动即报错（当前架构强依赖 tool call）

### 5.4 与现有机制的关系

- **config**：Model struct 加 Limits 字段；Load 时按四级回退解析一次
- **decision**：来源审计
- **compaction（第 3 节）**：阈值基于 ContextTokens
- 不引入网络依赖的新库：/models 用现有 model.Client 的 HTTP 栈

### 5.5 里程碑拆分（若纳入）

- **P2b-M11**：ModelLimits 结构 + 四级回退 + decisions 审计 + 测试（httptest 模拟 /models）
- **P2b-M12**：budget/compaction 接入 ContextTokens + tools=false 快速失败

### 5.6 待确认的问题

13. 本节是否纳入 P2b 范围？是/否
14. 内置表是否接受（模型族前缀 → 参数的静态映射，会随时间过期）？是/否
15. /models 探测是否需要用户显式开启（避免对私有端点发额外请求）？是/否

---

## 附：里程碑总览

| 里程碑 | 内容 | 依赖 |
|--------|------|------|
| M1 | goal/plan/todo 格式+工具+guard | — |
| M2 | 会话恢复注入 + context_refs + decisions | M1 |
| M3 | owltide goals 命令 | M1 |
| M4 | verifier 角色 | — |
| M5 | verified 事件 + done 联动 | M4, M1 |
| M6 | compaction 折叠+落盘 | — |
| M7 | resume 应用折叠 | M6 |
| M9 | memory 工具+注入 | — |
| M11/M12 | 模型参数匹配（待确认） | — |
