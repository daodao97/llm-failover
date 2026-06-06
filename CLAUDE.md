# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 常用命令

```bash
go build ./...                      # 构建
go test ./...                       # 运行全部测试
go test -run TestName .             # 运行单个测试（所有代码都在根包 failover 中）
go test -run 'TestCircuit.*' -v .   # 按模式匹配并输出详细日志
go vet ./...                        # 静态检查
go run ./examples/basic             # 运行示例（另有 dynamic-channels / pool / http-upstream）
```

测试不依赖外部服务（Redis 等均通过接口 mock），可直接本地运行。

## 项目定位

这是一个 Go 库（非可执行服务），实现「成功率优先」的 LLM API 多渠道转发控制面：渠道故障切换、单渠道多 key 轮换、单 key 重试、SSE 流治理、渠道级熔断、错误回写脱敏。

**边界约束**（决定一个功能是否应该进入本库）：只做转发成功率治理，不内建鉴权、计费、配置中心、数据库、管理后台。业务行为通过 hook 扩展（`Config` 上的各类钩子），而不是把业务概念塞进核心结构。

详细设计说明见 [README.md](README.md) 和 [ARCHITECTURE.md](ARCHITECTURE.md)（均为中文，代码注释也是中文，新代码请保持一致）。

## 核心架构

所有源码都在根目录的单一扁平包 `failover` 中，没有子包（`examples/` 除外）。

### 请求管线（三层容灾）

入口是 `Proxy`（handler.go，实现 `http.Handler`），核心流程在 pipeline.go：

```text
ServeHTTP
  -> prepareRequestBody     # 预读 body 到 Context（重试/切渠道需复用）
  -> selectChannels         # 静态 Config.Channels 或动态 Config.GetChannels，过滤 Enabled/CheckAvailable
  -> tryChannels            # 第 1 层：按顺序切换渠道
       -> tryChannel
            -> key loop     # 第 2 层：单渠道内 GetKeys 轮换，AcquireKey/ReleaseKey 控并发
            -> attempt loop # 第 3 层：单 key 内按 RetryConfig 重试
                 -> executeSingleAttempt    (attempt_http.go)
                 -> evaluateRetryDecision   # RetryOnError / RetryOnResponse / RetryOnSSE 三类判定
       -> circuit breaker record
  -> writePipelineResponse  (response.go)
```

`Context` 携带本次请求全部运行时状态（当前渠道/key/attempt、`OriginalRequestBody` 原始副本与 `RequestBody` 可变副本、最后失败响应、耗时统计），是所有钩子的读写对象。

### 熔断器（channel_breaker.go）

渠道级熔断，状态机 closed → open → half-open。关键语义：

- **两种触发**：错误率（`MinSamples` + `ErrorRateThreshold`）和慢请求率（`SlowThreshold` + `SlowRateThreshold`；流式看 TTFB/FirstEventTime，非流式看 TotalDuration）。
- **记账区分渠道类型**：`third` 渠道按单次真实上游失败记账（请求内连续 429/5xx 能尽早触发熔断）；`pool` 渠道按整轮 key 池的最终结果记账（一个 key 失败不代表池失败）。
- **半开探测收紧为单次尝试**：第一次探测失败立刻重新 open，不在同一轮半开里继续重试。
- **重开退避有上限**：连续熔断时冷却指数退避，但被 `MaxCooldown`（默认 16× `Cooldown`）封顶；窗口事件条数被 `MaxWindowSamples` 封顶（默认 2048，Redis 存储默认 256）。
- 状态存储通过 `CircuitBreakerStore` 接口抽象：默认进程内存，`channel_breaker_redis.go` 提供 Redis 实现（多实例共享，配合 `BreakerScope` 隔离）。Store 出错时降级为 fail-open 并以 30s 节流记日志。
- 动态创建/销毁 `Proxy`（如按租户）时需调用 `Proxy.Close()`，从全局健康注册表注销熔断器。
- `CircuitBreakerWhitelist` 中的渠道不参与熔断（但仍记录健康统计）。
- `ShouldCountFailureForCircuit` 钩子允许业务自定义哪些失败计入熔断窗口。

### 超时预算（timeout.go）

防止顺序重试把请求拖到上层（如 Cloudflare）超时：`FailoverTimeout` 是整条链路总预算，`AttemptTimeout` 是单次 attempt 拿到响应头前的预算，`MinAttemptTimeout` 避免剩余时间太少时还发起新 attempt。拿到响应头后预算不再中断 body 转发（保护 SSE 长流）。

attempt 超时以 `ErrAttemptTimeout` 哨兵错误标识（`IsAttemptTimeoutError` 判定），与客户端取消/总预算耗尽（`IsContextDoneError`）严格区分：attempt 超时计入熔断记账，并在总预算允许时跳过同 key 重试、继续轮换下一个 key/渠道；只有客户端取消或总预算耗尽才终止整条链路。若先耗尽 `FailoverTimeout` 总预算，错误必须保持为 `context.DeadlineExceeded`，不能包装成 `ErrAttemptTimeout`。包装 attempt 超时时底层 `context.Canceled` 必须用 `%v` 展平，不能让它留在错误链里被 `IsContextDoneError` 误判。

### SSE 是一等公民

自动识别 `text/event-stream`，支持首包探测（`RetryOnSSE` 可基于首事件决定重试）、`OnSSE` 逐事件观察、`TransformSSE` 事件改写（response.go）。「连接成功但流内容失败」是 LLM 上游的常见故障形态，所以流式与非流式统一纳入重试/熔断治理：2xx 之后上游断流会记入 `Context.StreamReadErr` 并计为熔断失败（客户端主动取消除外）。

### 可观测

- `Logger`（logger.go）：可选注入，默认 no-op，库不强制日志实现。
- `Observer`（observer.go）：结构化观测回调接口（attempt 起止、重试、熔断状态变更、SSE 事件、最终错误）；observer_prometheus.go 提供 Prometheus 实现。
- 错误回写（response.go + mask.go）：全渠道失败时保留最后上游错误体，但做渠道标识掩码、URL 脱敏、request id 注释清理——对内保留诊断信息，对外不泄露上游细节。

### 渠道构建

`NewChannel`（最简）、`BuildPoolChannel`（账号池，`GetKeys` 动态返回 key）、`BuildThirdPartyChannel`（标准第三方上游）。渠道支持 `ModelRewrite` 模型名映射（含通配符）、自定义 path、自定义 HTTP Client、OAuth Bearer 与 `x-api-key` 两种认证。

## 有意不做的事

权重调度、hedge request / 并发探测、全局健康评分、自适应优选——当前刻意保持顺序尝试的「稳定、可解释」设计，渠道顺序即业务优先级。除非明确要求，不要引入这些机制。
