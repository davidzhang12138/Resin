# 平台详情 — 活跃租约表格

日期：2026-06-26
分支基线：master (81eb7f4)

## 背景与目标

在平台详情里，仿照"可路由节点"独立页面的模式，新增一个"活跃租约"独立页面，展示某平台当前所有 sticky 租约，支持搜索/排序/分页，并支持对租约执行两种操作：

1. **重指节点** — 把某条租约重新指向该平台可路由节点中的一个指定节点。
2. **清除租约** — 删除某条租约，让该 account 下次请求时由路由器重新分配节点。

## 现状结论

- 后端租约列表接口**已存在**：`GET /api/v1/platforms/{id}/leases`（`internal/api/handler_lease.go` `HandleListLeases`），返回 `LeaseResponse[]`，支持 `account` 模糊搜索、`sort_by`/`sort_order`、`offset`/`limit` 分页。数据来自内存 `Router`（`RangeLeases`），无 DB 表。
- 单条/全部删除接口**已存在**：`DELETE /api/v1/platforms/{id}/leases/{account}`、`DELETE /api/v1/platforms/{id}/leases`。
- 租约持久化：内存为主，`cache.db` 的 `leases` 表用于重启恢复。`Router.UpsertLease` 发出 `LeaseReplace` 事件 → `app_runtime.go` 监听 → `engine` 标脏 → flush 写 cache.db。IP 计数（`IPLoadStats`）在 `UpsertLease` 内部原子增减。
- "重指节点"接口**不存在**，需新增。前端租约列表页/类型/API 函数**不存在**，需新增。

## 方案

采用**方案 A**：复用 `Router.UpsertLease`，新增 `ReassignLease` service 方法 + `PUT` handler。

重指的本质是"用新节点覆盖同 account 的租约"，与 `UpsertLease` 的覆盖语义完全契合，可自动复用 IP 计数、`LeaseReplace` 事件、持久化标脏全套机制。只需在 service 层补"校验目标节点在本平台可路由视图内 + 重算 egress + 续期"。

## 后端设计

### 新增 service 方法

文件：`internal/service/control_plane_leases.go`

```go
func (s *ControlPlaneService) ReassignLease(platformID, account, targetNodeHash string) (*LeaseResponse, error)
```

逻辑：

1. 校验 `platformID` 存在（`s.Pool.GetPlatform`），否则 `notFound("platform not found")`。
2. 读原租约 `s.Router.ReadLease(model.LeaseKey{PlatformID, Account})`；不存在或已过期（`ExpiryNs < now`）→ `notFound("lease not found")`。
3. 解析 `targetNodeHash`（`node.ParseHex`）；从 pool 取 entry，校验该节点在该平台可路由视图内（`plat.View().Contains(hash)`）且 entry 存在；否则 `invalidArg("node not routable on this platform")`。
4. 新 egress = target entry 的 `GetEgressIP()`。
5. 构造 `model.Lease`：保留 `PlatformID`/`Account`/`CreatedAtNs`；`NodeHash`=target、`EgressIP`=新值；`ExpiryNs` = now + 平台 `StickyTTLNs`（`model.Platform.StickyTTLNs`，纳秒，与正常建租约路径取值一致）；`LastAccessedNs` = now。
6. `s.Router.UpsertLease(next)` → 触发 `LeaseReplace` → 自动标脏持久化。
7. 返回新 `LeaseResponse`（`node_tag` 经 `resolveLeaseNodeTag` 解析）。

请求体只接受 `node_hash`（前端从节点选择器拿到的节点对象自带 hash，无需 tag 反查；pool 也无按 tag 反查 hash 的方法）。

### 新增 handler

文件：`internal/api/handler_lease.go`

- `HandleReassignLease`：`PUT /api/v1/platforms/{id}/leases/{account}`，body `{"node_hash":"..."}`。
- 调 `ReassignLease`，成功返回单条 `LeaseResponse`。
- 路由注册在 `internal/api/server.go` 现有 lease 路由组旁。

### 并发与一致性

- 并发安全由 `Router.UpsertLease` 的 `xsync.Map.Compute` 原语保证：重指与正常流量建租约同时发生时后写者覆盖，IP 计数正确增减。
- 持久化由现有 `LeaseReplace` → 标脏 → flush 链路自动处理，无需额外代码。

## 前端设计

### 独立页面

新文件：`webui/src/features/platforms/PlatformLeasesPage.tsx`，路由 `/platforms/:id/leases`。

- 顶部：标题"活跃租约"、平台名面包屑、手动刷新按钮。数据用 `useQuery` + 手动 `refetch`，**不自动轮询**（与 NodesPage 一致）。
- 复用 `DataTable`（TanStack React Table）+ `OffsetPagination`，与 NodesPage 同款。
- 入口：`PlatformMonitorPanel.tsx` 的"活跃租约"KPI 卡片改为可点击链接，指向 `/platforms/:id/leases`（与"可路由节点"卡片跳 `/nodes?platform_id=...` 对称）。

### 新 API 函数

文件：`webui/src/features/platforms/api.ts`

```ts
listPlatformLeases(id, params)    // GET    /api/v1/platforms/{id}/leases
reassignLease(id, account, body)  // PUT    /api/v1/platforms/{id}/leases/{account}
deleteLease(id, account)          // DELETE /api/v1/platforms/{id}/leases/{account}
```

`clearAllPlatformLeases` 已存在，复用。

### 新类型

文件：`webui/src/features/platforms/types.ts`

```ts
interface LeaseResponse {
  platform_id: string;
  account: string;
  node_hash: string;
  node_tag: string;
  egress_ip: string;
  expiry: string;        // RFC3339Nano
  last_accessed: string; // RFC3339Nano
}
```

### 表格列

| 列 | 内容 | 说明 |
|---|---|---|
| Account | `account` | 文本，主标识 |
| 节点 | `node_tag`（+ `node_hash` 短码） | 复用 NodesPage 节点 tag 渲染风格 |
| 出口 IP | `egress_ip` | 文本 |
| 剩余存活 | `expiry - now` 倒计时 | 过期=红、≤阈值(60s)=黄、正常=灰；tooltip 显示绝对过期时间 |
| 最近访问 | `last_accessed` 相对时间 | tooltip 显示绝对时间 |
| 操作 | 行操作菜单 | 见下 |

### 搜索/过滤栏

- account 关键字 → 后端 `?account=...&fuzzy=true`（已有）。
- egress_ip 过滤 → 前端本地过滤（后端列表接口无该参数；数据量不大，本地过滤足够，后续量大再加后端参数）。
- 排序：account / expiry / last_accessed，走后端 `sort_by`/`sort_order`。
- 分页：`limit`/`offset` 走后端分页。

### 行操作菜单

1. **重指节点** → 打开弹窗（见下）。
2. **清除租约** → 确认对话框 → 单条 `DELETE` → 刷新列表。

批量：表头多选 + "批量清除"按钮 → 逐条 `DELETE` → 刷新。**不做批量重指**（重指须逐个选目标节点，批量无意义）。

### 重指节点弹窗

- 触发：行操作菜单点"重指节点"。
- 标题："重指租约节点 — {account}"。
- 当前信息行：当前节点 `{node_tag}` / 出口 `{egress_ip}`。
- 目标节点选择器：可搜索下拉列表，数据来自 `GET /api/v1/nodes?platform_id={id}&has_outbound=true`（复用现有 nodes 接口，仅本平台可路由且有 outbound 的节点）。列表项显示 tag、区域、出口 IP、参考延迟。顶部搜索框按 tag 关键字过滤。
- 不预选，强制明确选择；未选中时确认按钮 disabled。
- 提交：调 `reassignLease(id, account, { node_hash })`。成功 → 关闭弹窗 + 刷新表格 + toast。失败 → toast 错误，弹窗保留供重试。

### 前端不重复后端校验

依赖后端权威校验：
- 目标节点不在平台可路由视图 → 后端 400。
- 原租约操作间已过期/被清 → 后端 404，前端提示"租约已不存在"并刷新表格。

## 错误处理

- 后端：`ReassignLease` 用现有 `notFound`/`invalidArg`/`internal` 包装；handler 走现有 JSON 错误响应。
- 前端：API 失败走现有 toast/错误展示；弹窗内错误不关闭弹窗；列表错误显示空状态 + 重试。

## 测试

后端（`internal/service/control_plane_leases_test.go` 等）：
- `ReassignLease` 成功：node_hash/egress_ip 更新、expiry 续期、IPLoad 旧 IP 减/新 IP 加。
- 目标节点不在平台视图 → `invalidArg`。
- 原租约不存在/已过期 → `notFound`。
- handler：`PUT` 路由 200/400/404，body 解析（`node_hash`）。

前端：
- 页面渲染、搜索过滤、分页。
- 重指弹窗：加载节点列表、选中提交、错误态。
- 行操作：清除租约确认 → 刷新。

## 改动文件清单

后端：
- `internal/service/control_plane_leases.go` — 新增 `ReassignLease`
- `internal/api/handler_lease.go` — 新增 `HandleReassignLease`
- `internal/api/server.go` — 注册 `PUT` 路由
- 对应 `_test.go` 补测试

前端：
- `webui/src/features/platforms/PlatformLeasesPage.tsx` — 新页面
- 路由配置文件 — 注册 `/platforms/:id/leases`
- `webui/src/features/platforms/PlatformMonitorPanel.tsx` — KPI 卡片加跳转链接
- `webui/src/features/platforms/api.ts` — 3 个 API 函数
- `webui/src/features/platforms/types.ts` — `LeaseResponse` 类型
- `webui/src/i18n/translations.ts` — 新增文案
- 新增重指弹窗组件（同目录）
- 可选测试文件

## 不做

- 不动 DB migration（租约在内存 + cache.db，已有）。
- 不动 Router 核心路由逻辑。
- 不做自动轮询。
- 不做批量重指。
