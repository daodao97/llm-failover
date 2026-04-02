# llm-failover

`llm-failover` 是一个面向 LLM API 多渠道转发场景的 Go 库。

它的核心目标不是实现一个普通的 HTTP 代理，而是作为一层
**“成功率优先”的请求转发控制面**：

- 当主渠道失败时，自动切换到后备渠道
- 当单个渠道内部有多个 key 时，自动轮换和重试
- 当某个渠道持续报错或持续变慢时，尽快跳过，避免拖垮整体请求
- 在不改动上层业务逻辑的前提下，提高整个 API 转发链路的成功率和稳定性

适用场景包括：

- 同一个模型请求需要在多个供应商或多个账号池之间择路
- 单渠道经常出现 429、5xx、网络抖动、余额不足、限流等问题
- 流式请求和非流式请求都需要统一治理
- 业务方希望把“转发”“重试”“降级”“熔断”从业务代码中抽离出来


** 🎯 核心目标 **

这个项目围绕一个非常明确的目标设计：

** 当一次 LLM API 请求有多个可选上游时，尽可能让这次请求成功。 **

这意味着它关注的不是单次请求“优雅失败”，而是：

1. 当前渠道失败后，是否还能快速切到下一个渠道
2. 当前 key 失败后，是否还能继续尝试同渠道内其他 key
3. 某个坏渠道是否会被及时识别并跳过
4. 某个慢渠道是否会在连续拖慢请求后被自动降权到“先别再试”
5. 最终返回给调用方的错误，是否尽量保留真实上游信息，同时避免泄漏敏感细节

所以，`llm-failover` 的职责不是“帮你发请求”，而是：

** 在多个可能成功的路径里，持续寻找最有机会成功的那一条。 **

进一步严格拆开，这个项目的核心目标应该是 4 条：

1. ** 请求成功率优先 **
   在多个候选上游之间持续尝试，避免因为单点失败直接让请求失败。
2. ** 失败成本最小化 **
   尽快识别坏渠道、坏 key、慢渠道，减少在低成功率路径上的浪费时间。
3. ** 稳定性治理内聚 **
   把重试、切换、熔断、SSE 判定、错误处理集中在同一层，而不是散落在业务代码中。
4. ** 保持为库，而不是业务系统 **
   只负责转发成功率治理，不内建鉴权、计费、配置中心、数据库等业务能力。

如果按这 4 条衡量，一个功能是否应该进入这个项目，判断标准会更清楚。


** 🧠 设计机制 **

项目当前采用的是一条分阶段的转发管线。

整体流程如下：

```text
客户端请求
   ↓
预读请求体
   ↓
选择可用渠道
   ↓
按渠道顺序尝试
   ↓
单渠道内按 key 轮换
   ↓
单 key 内按策略重试
   ↓
成功则返回
   ↓
失败则记录错误/延迟，决定是否切换下一个渠道
   ↓
全部失败时统一输出错误
```

### 1. 请求体预读

在进入重试和故障切换逻辑前，请求体会先被完整读入内存。

这样做的原因很直接：

- HTTP Body 默认只能消费一次
- 一旦要进行重试或切换渠道，就必须能重复发送原始请求
- 对于模型重写、请求前钩子等逻辑，也需要可修改、可复用的请求体副本

因此，库内部维护了：

- `OriginalRequestBody`：原始请求体副本
- `RequestBody`：当前尝试可修改的请求体


### 2. 渠道选择

请求进入转发前，库会先得到本次请求可用的渠道列表。

支持两种模式：

- 静态渠道：通过 `Config.Channels` 直接传入
- 动态渠道：通过 `Config.GetChannels` 按请求动态生成

这一步只负责回答一个问题：

** 这次请求有哪些候选上游可以试？ **

然后库会继续过滤：

- `Enabled == false` 的渠道直接跳过
- `CheckAvailable()` 返回不可用的渠道直接跳过


### 3. 渠道级故障切换

渠道是最高一层的故障切换单元。

例如你可能有：

- `primary-anthropic`
- `backup-openrouter`
- `internal-pool-a`
- `internal-pool-b`

库会按顺序尝试渠道：

1. 尝试第一个渠道
2. 如果失败且满足“可以继续换下一个渠道”的条件，则尝试第二个
3. 直到某个渠道成功，或者所有渠道都失败

这个机制解决的是：

** 单个渠道失败，不应该直接导致整个请求失败。 **


### 4. 单渠道内多 key 轮换

在很多 LLM 接入场景中，一个渠道并不只有一个凭证，而是一组 key：

- 多个 API key
- 一个账号池中的多个账号 token
- 多个受并发控制的租户凭证

因此 `Channel` 支持：

- `GetKeys`：返回当前渠道可尝试的 key 列表
- `AcquireKey` / `ReleaseKey`：控制 key 并发占用

这样单个渠道内部就可以形成第二层容灾：

```text
渠道 A
  ├── key 1 失败
  ├── key 2 失败
  └── key 3 成功
```

也就是说：

** 渠道不等于单点。**

很多时候真正失效的只是某个 key，而不是整个渠道。


### 5. 单次尝试内重试

对同一个 key 的单次执行，库还支持重试。

重试由 `RetryConfig` 控制，核心决策分三类：

- `RetryOnError`：网络错误、连接错误、EOF、上游中断等
- `RetryOnResponse`：响应状态码或错误响应体是否应重试
- `RetryOnSSE`：SSE 流式响应首包异常时是否应重试

这意味着它不是只会对“请求报错”进行重试，而是能基于：

- 错误对象
- HTTP 状态码
- SSE 错误事件

综合判断。

另外还支持：

- `MaxAttempts`
- 指数退避 `BaseDelay` / `MaxDelay`

所以它不是“死循环重发”，而是可控的、有节奏的重试。


### 6. 降级与熔断

这是整个库最关键的稳定性机制之一。

如果某个渠道短时间内持续失败，或者持续变慢，那么继续优先尝试它，只会：

- 放大请求延迟
- 增加上层超时概率
- 拖累整体成功率

因此库内置了渠道级熔断器。

#### 错误率熔断

在一个时间窗口内，如果某渠道：

- 采样数达到 `MinSamples`
- 且失败率超过 `ErrorRateThreshold`

则该渠道会进入打开状态，短时间内不再参与尝试。

#### 慢请求熔断

如果某渠道虽然不一定显式报错，但持续很慢，同样会伤害整体成功率。

因此还支持按延迟判定慢请求：

- 流式请求可看 `FirstEventTime` / `TTFB`
- 非流式请求可看 `TotalDuration`

当慢请求占比超过 `SlowRateThreshold` 时，也可以触发熔断。

#### 半开恢复

熔断不是永久封禁。

冷却时间结束后，渠道会进入半开探测：

- 如果探测成功，渠道恢复
- 如果探测失败，继续重新打开

这保证了系统对坏渠道足够敏感，同时也允许它在恢复后重新回到流量路径中。


### 7. SSE 流式响应治理

很多 LLM 接口都使用 SSE 返回流式结果。

这类请求有两个典型难点：

- 成功建立连接，不代表真正成功产出
- 某些上游会返回空流、错误事件、错误格式流

因此库对 SSE 做了专门支持：

- 自动识别 `text/event-stream`
- 支持首包探测
- 支持 SSE 错误重试判断
- 支持 `OnSSE` 逐事件观察
- 支持 `TransformSSE` 转换事件内容

这意味着它不仅能“透传流”，还能在流式场景下继续做稳定性治理。


### 8. 错误回写策略

当全部渠道都失败时，库并不会只给一个笼统的 `bad gateway`。

它会尽量保留：

- 最后一次失败状态码
- 上游返回体
- 渠道标识前缀
- trace 前缀

同时也会做必要处理：

- 敏感 URL 脱敏
- request id 注释清洗
- 非 JSON 错误包装为统一 JSON 结构
- 渠道标识掩码化，避免直接暴露真实渠道

所以这个库的目标是：

** 对内保留足够的诊断信息，对外避免泄露不必要的上游细节。 **


** ⚙️ 支持能力 **

当前版本已经支持以下核心能力。

### 请求转发能力

- 标准 `http.Handler`
- 支持普通 HTTP 响应和 SSE 流式响应
- 支持静态渠道和动态渠道选择
- 支持自定义目标路径
- 支持请求头透传和渠道头覆盖

### 成功率治理能力

- 多渠道顺序故障切换
- 单渠道多 key 轮换
- 单 key 重试
- 网络错误重试
- 基于状态码的重试
- 基于 SSE 错误的重试
- 错误率熔断
- 慢请求熔断
- 半开恢复

### 渠道能力

- pool 渠道构建
- third-party 渠道构建
- API key 轮换
- OAuth Bearer 与 `x-api-key` 两种认证方式
- 模型名重写
- 按渠道定制 path
- 自定义 HTTP Client

### 可观测与扩展能力

- DNS / TCP / TLS / TTFB / 首事件 / 总耗时统计
- 请求前钩子
- 每次 attempt 前钩子
- attempt 错误钩子
- 渠道失败钩子
- 成功响应钩子
- Body 观察钩子
- SSE 观察钩子
- 错误钩子


** ⚠️ 当前缺口 **

从“成功率优先的多渠道转发控制层”这个目标出发，当前代码虽然已经具备主干能力，但还存在几个明确缺口：

1. ** 渠道选择策略仍然偏静态 **
   当前主要是顺序尝试，还没有内建权重调度、成功率反馈调度、延迟感知调度。
2. ** 没有并行探测或 hedge request **
   对极端延迟敏感场景，目前还不支持并发探测多个候选渠道后择优返回。
3. ** 熔断后的恢复策略仍较基础 **
   现在是窗口统计 + 冷却 + 半开探测，没有更细的健康评分机制。
4. ** 池诊断结构仍可继续收敛 **
   当前已经从业务化命名收敛为 `PoolStats`，但后续仍可以继续补充更明确的公开语义和示例。
5. ** examples 之前缺失 **
   这会降低可用性和上手效率，现在已补上最小示例。


** 🧩 核心类型 **

### `Config`

`Config` 用来定义整个 failover pipeline 的行为。

最重要的字段包括：

- `Channels` / `GetChannels`
- `Retry`
- `Client`
- `Logger`
- `CircuitBreaker`
- `ShouldCountFailureForCircuit`
- `BeforeProxy`
- `BeforeAttempt`
- `AfterProxy`
- `OnAttemptError`
- `OnChannelFail`
- `OnBody`
- `OnSSE`
- `TransformSSE`
- `OnError`

适合理解成：

** 一次完整的“转发控制策略配置”。 **

其中 `Logger` 是可选注入项，默认 no-op。
这个库会打点，但不会替使用方决定日志实现。


### `Channel`

`Channel` 表示一个候选上游。

它不只是一个 URL，还包含：

- 渠道名称和标识
- 认证方式
- key 列表
- 该渠道自己的重试策略
- 目标路径改写策略
- 自定义处理函数
- 可用性检查

适合理解成：

** 一个带治理策略的上游节点。 **


### `Context`

`Context` 是本次请求在 pipeline 中流转的运行时状态。

里面会记录：

- 当前渠道
- 当前 key
- 当前 attempt
- 当前目标 URL / Header
- 请求体副本
- 最后一次失败状态码和响应体
- 统计信息

适合在各种钩子中读取和修改。


### `RetryConfig`

它定义：

- 一次 key 尝试最多重试几次
- 什么错误重试
- 什么响应重试
- 什么 SSE 情况重试
- 重试等待时间

这是“如何对失败做二次尝试”的核心。


### `CircuitBreakerConfig`

它定义：

- 多久统计一次失败窗口
- 错误率达到多少打开熔断
- 慢请求比例达到多少打开熔断
- 熔断后冷却多久
- 恢复探测如何进行

这是“如何避免坏渠道持续拖累系统”的核心。


** 🚀 应用方式 **

最常见的接入方式，是把它直接挂到你的 HTTP 服务里。

### 1. 最小接入

```go
package main

import (
	"net/http"

	failover "github.com/daodao97/llm-failover"
)

func main() {
	p := failover.New(failover.Config{
		Channels: []failover.Channel{
			failover.NewChannel(1, "primary", "https://api1.example.com"),
			failover.NewChannel(2, "backup", "https://api2.example.com"),
		},
		Retry: failover.DefaultRetry(),
	})

	http.Handle("/v1/messages", p)
	_ = http.ListenAndServe(":8080", nil)
}
```

这个模式适合：

- 快速把一个接口变成多渠道转发
- 先接静态渠道，后续再切动态渠道


### 2. 动态渠道选择

如果不同用户、不同 token、不同模型需要不同候选渠道，可以使用
`GetChannels`：

```go
p := failover.New(failover.Config{
	GetChannels: func(r *http.Request) ([]failover.Channel, error) {
		return []failover.Channel{
			failover.NewChannel(1, "tier-a-primary", "https://api-a.example.com"),
			failover.NewChannel(2, "tier-a-backup", "https://api-b.example.com"),
		}, nil
	},
	Retry: failover.DefaultRetry(),
})
```

这个模式适合：

- 按租户路由
- 按模型路由
- 按权限分组路由
- 按业务策略返回不同渠道集


### 3. 构建账号池渠道

如果渠道内部本身就有多个可轮换 key，可以使用 `BuildPoolChannel`：

```go
ch, err := failover.BuildPoolChannel(failover.PoolChannelConfig{
	Id:             1,
	Name:           "claude-pool",
	DefaultBaseURL: "https://api.anthropic.com",
	GetKeys: func(ctx *failover.Context) []failover.Key {
		return []failover.Key{
			{ID: "k1", Value: "token-1"},
			{ID: "k2", Value: "token-2"},
		}
	},
})
if err != nil {
	panic(err)
}
```

这个模式适合：

- 账号池
- 多 key 池
- 需要控制并发占用的共享凭证池


### 4. 构建第三方渠道

如果是标准第三方上游，可以使用 `BuildThirdPartyChannel`：

```go
ch, err := failover.BuildThirdPartyChannel(2, "openrouter", &failover.ThirdInfo{
	BaseURL: "https://openrouter.ai/api/v1",
	APIKeys: []failover.ThirdAPIKey{
		{Value: "key-a"},
		{Value: "key-b"},
	},
})
if err != nil {
	panic(err)
}
```


### 5. 自定义模型重写

有些渠道对模型命名不同，可以通过 `ModelRewrite` 做统一接入：

```go
ch.ModelRewrite = []failover.ModelRewriteRule{
	{Source: "claude-sonnet", Target: "claude-3-7-sonnet-20250219"},
	{Source: "gpt-4o-*", Target: "gpt-4o"},
}
```

这样上层只需使用统一模型名，底层由渠道自行映射。


### 6. 自定义钩子

如果你需要在进入代理前改写 header、body、目标地址，或采集统计，
可以使用钩子：

```go
p := failover.New(failover.Config{
	Channels: []failover.Channel{ch},
	BeforeAttempt: func(ctx *failover.Context) {
		// 记录日志、埋点、尝试次数
	},
	OnAttemptError: func(ctx *failover.Context, err error) {
		// 记录每次失败原因
	},
	AfterProxy: func(ctx *failover.Context, resp *http.Response) {
		// 观察成功响应
	},
})
```

如果你希望把库内部日志接到自己的日志系统，可以注入 `Logger`。
例如使用标准库 `slog`：

```go
handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
	Level: slog.LevelInfo,
})

p := failover.New(failover.Config{
	Channels: []failover.Channel{ch},
	Logger:   failover.NewSlogLogger(slog.New(handler)),
})
```

不注入时默认是 no-op，不会修改全局 logger，也不会强制输出日志。


** 📦 Examples **

仓库现在提供了 4 个最小示例：

- [examples/basic/main.go](/Users/daodao/work/github/cc/llm-failover/examples/basic/main.go)
  演示主渠道失败后切到备用渠道。
- [examples/dynamic-channels/main.go](/Users/daodao/work/github/cc/llm-failover/examples/dynamic-channels/main.go)
  演示根据请求头动态选择不同渠道集合。
- [examples/pool/main.go](/Users/daodao/work/github/cc/llm-failover/examples/pool/main.go)
  演示单渠道内多 key 轮换与恢复。
- [examples/http-upstream/main.go](/Users/daodao/work/github/cc/llm-failover/examples/http-upstream/main.go)
  演示真实 HTTP 上游转发，以及 JSON 和 SSE 两种请求在主上游失败后切换到备用上游。

运行方式：

```bash
go run ./examples/basic
go run ./examples/dynamic-channels
go run ./examples/pool
go run ./examples/http-upstream
```

更完整的设计说明见 [ARCHITECTURE.md](/Users/daodao/work/github/cc/llm-failover/ARCHITECTURE.md)。


** 📌 典型使用场景 **

### 场景 1：多供应商兜底

主供应商不稳定时，自动切到备用供应商。

例如：

- 主走 Anthropic
- 失败后切 OpenRouter
- 再失败后切内部代理池


### 场景 2：账号池治理

一个供应商下有多个账号 token，需要：

- 轮换
- 并发限制
- 失败自动换 key


### 场景 3：统一 LLM 网关内部转发层

你有自己的 API 网关，但不想把这些高可用逻辑散落在业务 handler 中。

此时可以把 `llm-failover` 作为内部转发核心层。


### 场景 4：流式接口高可用

SSE 比普通 JSON 返回更容易出现“连接成功但内容失败”的情况。

这个库可以帮助你在流式场景下继续做：

- 首包判定
- SSE 错误重试
- 流式链路耗时统计


** 🛠️ 设计建议 **

为了让这个库真正发挥价值，建议在业务接入时遵循下面几个原则。

### 1. 渠道顺序要体现优先级

当前策略是顺序尝试。

所以渠道顺序本身就是你的业务策略：

- 最优先的渠道放前面
- 更贵但更稳的渠道放后面做兜底
- 限额低的渠道不要放在首位承接全部流量


### 2. 重试要克制

重试不是越多越好。

如果：

- 单 key 已经重试很多次
- 但还有更多备用渠道

那么通常更好的策略是：

** 少在坏 key 上浪费时间，尽快切换渠道。 **


### 3. 熔断阈值要贴近业务 SLA

如果你的接口对延迟非常敏感，那么：

- 慢请求熔断应更激进
- 冷却时间可以更短

如果你的接口更关注成功率而不是低延迟，那么：

- 可以容忍更长的慢请求阈值
- 但对 429 / 5xx 更敏感


### 4. 错误判断要结合真实上游行为

不同供应商对错误语义不完全一致。

例如：

- 有的 400 实际上是可恢复错误
- 有的 429 是临时限流，适合重试
- 有的 422 代表余额或配额异常，适合快速切换

因此建议根据你自己的上游行为，定制：

- `RetryOnResponse`
- `ShouldCountFailureForCircuit`


** 📚 项目定位 **

这个项目当前更适合作为：

- 你的 LLM 网关内部核心库
- 多供应商转发组件
- 多账号池治理组件
- 面向成功率优化的转发中间层

它不负责：

- 完整的业务鉴权
- 用户计费
- 数据库存储
- 渠道配置中心
- 管理后台

这些能力应该在更上层系统中完成，而 `llm-failover` 只专注一件事：

** 把一次请求尽可能成功地送到可用上游。 **


** ✅ 当前状态 **

当前仓库由 `subapi/pkg/proxy` 抽离而来，已经具备独立项目的最小闭环：

- 可独立构建
- 可独立测试
- 具备清晰的转发与高可用核心逻辑

后续建议优先做的演进方向：

1. 增加 `examples/`，给出真实接入示例
2. 收敛与业务强相关的字段，继续抽象公共接口
3. 补充更明确的公开 API 文档
4. 增加指标导出和健康快照接口
