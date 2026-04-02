# ARCHITECTURE

`llm-failover` 的设计目标很单一：

**在多个候选上游之间，以尽可能低的失败成本，尽可能把一次 LLM 请求送达可用上游。**

这份文档只解释当前已经落地的核心结构，不扩展新的调度算法。

## 1. 系统定位

它是一个库，不是完整网关系统。

它负责：

- 多渠道故障切换
- 单渠道多 key 轮换
- 单 key 重试
- SSE 与普通响应统一治理
- 渠道级熔断
- 错误回写与脱敏

它不负责：

- 用户鉴权
- 计费
- 配置中心
- 数据库存储
- 管理后台
- 长期健康评分系统

## 2. 请求管线

当前实现是一条顺序决策管线：

```text
HTTP Request
   -> prepareRequestBody
   -> selectChannels
   -> tryChannels
        -> tryChannel
             -> prepareChannelAttempt
             -> key loop
             -> attempt loop
                  -> executeSingleAttempt
                  -> evaluateRetryDecision
        -> circuit breaker record
   -> writePipelineResponse
```

可以把它理解成 3 层容灾：

1. 渠道级切换
2. 单渠道内 key 轮换
3. 单 key 内重试

## 3. 渠道 / Key / Attempt 层级

```text
Request
  ├── Channel A
  │     ├── Key 1
  │     │     ├── Attempt 1
  │     │     └── Attempt 2
  │     └── Key 2
  │           └── Attempt 1
  └── Channel B
        └── Key 1
              └── Attempt 1
```

这 3 层的职责不同：

- `Channel`：不同上游之间的切换
- `Key`：同一上游内不同凭证之间的切换
- `Attempt`：同一凭证上的短时重试

核心原则是：

**先把“便宜的恢复动作”做完，再进入更重的切换动作。**

## 4. 为什么要预读请求体

在进入重试和切换之前，请求体会先被完整读入 `Context`。

原因是：

- HTTP body 默认只能消费一次
- 重试需要复用请求体
- 渠道模型改写需要可修改副本
- 切换到备用渠道时不能丢原始请求内容

当前上下文里同时保留：

- `OriginalRequestBody`
- `RequestBody`

前者是原始副本，后者是当前尝试可变副本。

## 5. 渠道选择与切换

渠道来源有两种：

- 静态 `Config.Channels`
- 动态 `Config.GetChannels`

选出候选集合后，当前实现按顺序尝试。

顺序尝试的优点：

- 决策简单
- 业务优先级直观
- 调试成本低

顺序尝试的代价：

- 不具备权重调度
- 不具备 hedge request
- 对尾延迟的压缩能力有限

所以现在的设计偏“稳定、可解释”，而不是“最激进的低延迟调度”。

## 6. 单渠道内的 key 轮换

很多上游不是单点，而是一个凭证池。

当前模型里，渠道可通过 `GetKeys` 返回一批候选 key。之后库会：

- 依次尝试 key
- 为每个 key 运行自己的 attempt 循环
- 支持 `AcquireKey` / `ReleaseKey` 控制并发占用

这能解决的典型问题是：

- 单 key 限流
- 单 key 临时失效
- 单 key 并发占满

## 7. Retry 决策

Retry 不是单一规则，而是 3 类判定组合：

- `RetryOnError`
- `RetryOnResponse`
- `RetryOnSSE`

也就是说，库允许你基于：

- Go error
- HTTP 状态码
- SSE 首包内容

分别决定是否继续重试。

这让它比“只看 5xx”更接近真实 LLM 上游行为。

## 8. Circuit Breaker

熔断器是为了避免坏渠道持续拖慢整体请求。

当前是渠道级熔断，状态机可简化理解为：

```text
closed
  -> (error rate / slow rate exceed threshold)
open
  -> (cooldown elapsed)
half-open
  -> (probe success) closed
  -> (probe failure) open
```

### closed

正常接收请求，持续累计窗口数据。

### open

直接跳过该渠道，不再把请求打上去。

### half-open

允许少量探测请求验证渠道是否恢复。

如果恢复成功，清空失败状态并回到 `closed`。
如果恢复失败，重新进入 `open`。

## 9. 慢请求判定

当前除了错误率，还会统计慢请求比例。

- 流式请求优先看 `FirstEventTime` / `TTFB`
- 非流式请求优先看 `TotalDuration`

这很重要，因为很多 LLM 上游的问题不是“立刻报错”，而是“极慢但最终有响应”。

从成功率角度看，极慢渠道同样会拖垮整体体验。

## 10. SSE 处理

SSE 在这个库里不是特殊旁路，而是正式的一等公民。

它支持：

- 自动识别 `text/event-stream`
- 首事件耗时统计
- SSE 错误重试判定
- `OnSSE` 观察
- `TransformSSE` 转换

所以流式请求也可以纳入统一的成功率治理模型。

## 11. Error 处理

全部渠道失败后，输出逻辑分两类：

- 如果上游已经给出可复用错误体，优先保留它
- 如果上游返回的是非 JSON 或本地错误，则包装成统一错误响应

同时会做：

- 渠道标识掩码
- 上游 URL 脱敏
- request id 注释清理

目标是：

**对调用方尽量保留有价值的错误信息，但不直接泄露上游内部细节。**

## 12. PoolStats 的角色

`PoolStats` 不是调度器输入，而是诊断补充信息。

它只解决一个问题：

**当池里没有可用 key 时，能不能给出更准确的解释。**

例如：

- 候选本来就为空
- scope 不匹配
- 候选都忙
- 候选都已失效

这部分信息主要用于：

- 错误说明
- 日志
- 可观测

而不是参与核心选择算法。

日志本身不由库内建实现决定，而是通过 `Config.Logger` 交给外部注入。

## 13. 扩展点

当前主要扩展点有：

- `GetChannels`
- `Logger`
- `BeforeProxy`
- `BeforeAttempt`
- `AfterProxy`
- `OnAttemptError`
- `OnChannelFail`
- `OnBody`
- `OnSSE`
- `TransformSSE`
- `OnError`
- `ShouldCountFailureForCircuit`

这些扩展点的设计原则是：

**尽量通过 hook 扩展行为，而不是把业务概念塞进核心结构。**

## 14. 当前有意不做的事

为了控制复杂度，当前版本刻意没有引入：

- 权重调度
- 多渠道并发探测
- hedge request
- 全局健康评分
- 自适应优选算法

这些能力未来可以做，但会显著增加系统复杂度。
当前版本优先保证的是：

- 行为清晰
- 失败路径可解释
- 核心逻辑容易验证
