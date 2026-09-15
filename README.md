# 包装线双光电门脉冲配对 API

纯后端服务：将包装线上、下游两道光电门采集到的微秒级脉冲时间戳做**全局、
顺序不交叉**的最优配对，用于纠正漏检、反光毛刺和密集包材导致的“按下标直接
配对”错位问题。

- 语言/框架：Go 1.24 + [Gin](https://github.com/gin-gonic/gin)
- 唯一对外业务接口：`POST /match`
- 运行方式：Docker Compose（长期运行的 `api` 服务 + 一次性 Go testing
  验收服务 `verify`）
- 宿主端口由环境变量 `API_PORT` 覆盖

---

## 1. 快速开始

```bash
# 构建并启动 api（默认宿主端口 8080）
docker compose up --build

# 使用自定义宿主端口
API_PORT=9090 docker compose up --build
```

启动后唯一业务接口监听在 `http://localhost:${API_PORT:-8080}/match`。
容器内端口固定为 `8080`，`API_PORT` 只改变宿主机映射端口。

### 一次性验收

`verify` 是一个**一次性**服务：它等待 `api` 健康后，在容器网络内对真实
运行的 API 运行整套 Go testing 验收用例，随后退出（退出码即验收结果）。

```bash
# 启动 api 并在其就绪后自动运行一次性 verify，verify 结束即整体退出
docker compose up --build --abort-on-container-exit verify

# 或先起 api，再单独执行一次验收
docker compose up -d --build api
docker compose up verify
```

`verify` 不内嵌任何固定应答，全部断言都基于请求输入独立复算。

---

## 2. 接口定义

### `POST /match`

**请求体（JSON 对象）**

| 字段         | 类型         | 范围/约束                                        |
|--------------|--------------|--------------------------------------------------|
| `upstream`   | 整数数组     | 元素非负、**严格递增**；允许空数组 `[]`          |
| `downstream` | 整数数组     | 元素非负、**严格递增**；允许空数组 `[]`          |
| `delay`      | 整数         | `0 ≤ delay ≤ 1000000`                            |
| `window`     | 整数         | `0 ≤ window ≤ 1000000`（边界含等号，可配）       |
| `penalty`    | 整数         | `0 ≤ penalty ≤ 1000000`                          |

时间戳单位为微秒，非负整数（允许超出 32 位，使用 64 位整数）。

**配对规则**

- 仅当 `abs((downstream[j] - upstream[i]) - delay) ≤ window` 时，脉冲对
  `(i, j)` 才允许配对，其代价为该绝对值。
- 跳过（不配对）任意一条脉冲的代价为 `penalty`。
- 配中的下标沿两侧都严格递增（顺序不交叉）。

**优化目标（三级判优，依次比较）**

1. 总代价最小；
2. 总代价相同，取**配对数更多**者；
3. 仍相同，取“按配对顺序组成的 `(i, j)` 序列”**字典序更小**者
   （下标从 0 开始；先比 `i`，相同再比 `j`）。

三级判优保证合法输入（含强并列的密集包材）存在**唯一、可复算**的结果。

**成功响应 `200`**

```json
{
  "cost": 5,
  "pairs": [
    { "i": 0, "j": 0, "residual": -2 },
    { "i": 1, "j": 1, "residual": 3 }
  ],
  "unmatched_upstream": [],
  "unmatched_downstream": []
}
```

| 字段                    | 含义                                                                 |
|-------------------------|----------------------------------------------------------------------|
| `cost`                  | 最优方案总代价                                                       |
| `pairs[].i` / `.j`      | 配对的上/下游下标（从 0 开始，按配对顺序严格递增）                   |
| `pairs[].residual`      | **有符号残差** `(downstream[j]-upstream[i]) - delay`                 |
| `unmatched_upstream`    | 未匹配的上游下标，升序；空时为 `[]`（不是 `null`）                   |
| `unmatched_downstream`  | 未匹配的下游下标，升序；空时为 `[]`（不是 `null`）                   |

总代价可由响应独立复算：

```
cost = Σ |residual|
       + penalty * (len(upstream) + len(downstream) - 2 * len(pairs))
```

**请求示例**

```bash
curl -s -X POST http://localhost:8080/match \
  -H 'Content-Type: application/json' \
  -d '{"upstream":[0,50],"downstream":[8,63],"delay":10,"window":5,"penalty":100}'
```

---

## 3. 校验与错误处理

数组元素不是整数、时间戳不满足严格递增、参数越界等情况，服务**整批拒绝**，
返回 **HTTP 422**，并且：

- 一次性收集**全部**字段错误，不在首个错误处中断；
- 错误按其在请求体中的**输入位置（字节偏移）升序**排列；
- 响应只包含错误，不返回任何局部结果（无 `cost`/`pairs`）。

```json
{
  "errors": [
    { "position": 18, "field": "downstream[2]", "code": "not_strictly_increasing",
      "message": "downstream[2] must be strictly greater than downstream[1]" },
    { "position": 52, "field": "delay", "code": "out_of_range",
      "message": "delay must be between 0 and 1000000" }
  ]
}
```

错误码：

| `code`                     | 触发场景                                             |
|----------------------------|------------------------------------------------------|
| `invalid_json`             | 非 JSON、非对象、JSON 语法损坏、尾随数据             |
| `required`                 | 缺少必填字段                                         |
| `unknown_field`            | 出现未定义字段                                       |
| `duplicate_field`          | 同一字段重复出现                                     |
| `not_an_array`             | `upstream`/`downstream` 收到非数组值                 |
| `not_integer`              | 应为整数处给出小数、字符串、布尔、`null`、科学计数法 |
| `negative_integer`         | 时间戳元素为负整数                                   |
| `not_strictly_increasing`  | 时间戳数组出现相等或回落                             |
| `out_of_range`             | `delay`/`window`/`penalty` 超出 `[0,1000000]`        |

其他状态码：请求体超过 1 MiB 返回 `413`；请求体无法读取返回 `400`。

---

## 4. 算法

`internal/match` 自行实现全局顺序动态规划（不依赖任何配对/对齐库）：

- 状态 `dp[i][j]` 表示使用前 `i` 条上游、前 `j` 条下游脉冲时的最优方案，
  三种转移：跳过 `upstream[i-1]`、跳过 `downstream[j-1]`、在窗内配
  `(i-1,j-1)`。
- 每个状态同时保存总代价、配对数，以及按配对顺序的 `(i,j)` 下标序列；
  配对数相同才逐对比较下标序列，精确实现第三级字典序判优。
- 滚动两行保存代价/计数；相同内容的配对序列通过内容节点 id 做 O(1) 判等
  快速路径，密集同构场景下避免退化。
- 时间复杂度约 `O(nm)` 次转移；空间为两行状态。

结果对相同输入**确定性唯一**：重复请求得到逐字段一致的配对，可由
`residual`、`unmatched_*` 与上式独立复算 `cost`。

---

## 5. 本地开发

```bash
go version            # 需要 Go 1.24+
go test ./...         # 全部单元测试（DP 含对小尺度穷举暴力解的数千组对照）

# 针对已运行的 API 执行端到端验收
go run ./cmd/server &                         # 默认 :8080，可用 API_PORT 覆盖
MATCH_API_BASE_URL=http://localhost:8080 go test ./test/...
```

### 目录结构

```
cmd/server/            HTTP 服务入口（读取 API_PORT）
internal/match/        全局顺序动态规划与三级判优（含穷举对照测试）
internal/api/          Gin 路由、位置感知 JSON 解析、整批字段校验
test/                  面向真实 API 的端到端验收（verify 容器运行）
Dockerfile             同时构建 server 与验收测试二进制
docker-compose.yml     api（长期）+ verify（一次性）
```
