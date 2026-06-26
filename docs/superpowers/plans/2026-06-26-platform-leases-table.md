# 平台活跃租约表格 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在平台详情里新增"活跃租约"独立页面，展示某平台的 sticky 租约列表，支持搜索/排序/分页，并支持"重指节点"和"清除租约"两种行操作。

**Architecture:** 后端租约列表/删除接口已存在，仅需新增"重指节点"——复用 `Router.UpsertLease`（自动处理 IP 计数、`LeaseReplace` 事件、cache.db 持久化），在 service 层补"校验目标节点在本平台可路由视图内 + 重算 egress + 续期"。前端仿照 NodesPage 独立页面模式，新增 `/platforms/:id/leases` 页面 + DataTable + 重指弹窗，从 PlatformMonitorPanel 的"活跃租约"KPI 卡片链接进入。

**Tech Stack:** Go（service/handler）、React + TypeScript + TanStack Query/Table + react-router-dom（前端）。

**Spec:** `docs/superpowers/specs/2026-06-26-platform-leases-table-design.md`

---

## File Structure

**后端（新增/修改）：**
- `internal/service/control_plane_leases.go` — 新增 `ReassignLease` 方法
- `internal/service/control_plane_leases_test.go` — 新增 `ReassignLease` 测试 + 夹具
- `internal/api/handler_lease.go` — 新增 `HandleReassignLease`
- `internal/api/server.go` — 注册 `PUT /api/v1/platforms/{id}/leases/{account}` 路由
- `internal/api/handler_lease_test.go`（若存在则追加，否则新增）— handler 测试

**前端（新增/修改）：**
- `webui/src/features/platforms/types.ts` — 追加 `LeaseResponse` 类型
- `webui/src/features/platforms/api.ts` — 追加 `listPlatformLeases` / `reassignLease` / `deleteLease`
- `webui/src/features/platforms/PlatformLeasesPage.tsx` — 新页面
- `webui/src/features/platforms/ReassignLeaseDialog.tsx` — 重指节点弹窗
- `webui/src/app/routes.tsx` — 注册 `/platforms/:platformId/leases` 路由
- `webui/src/features/platforms/PlatformMonitorPanel.tsx` — "活跃租约"KPI 卡片加跳转链接
- `webui/src/i18n/translations.ts` — 追加新文案

---

## Task 1: 后端 — ReassignLease service 方法（TDD）

**Files:**
- Modify: `internal/service/control_plane_leases.go`
- Test: `internal/service/control_plane_leases_test.go`

- [ ] **Step 1: 写失败测试 — 成功重指**

在 `internal/service/control_plane_leases_test.go` 末尾追加。先加一个能让节点进入平台可路由视图的夹具，再测成功重指。

```go
func newReassignTestService(t *testing.T) (*ControlPlaneService, *platform.Platform, node.Hash) {
	t.Helper()
	subMgr := topology.NewSubscriptionManager()
	pool := topology.NewGlobalNodePool(topology.PoolConfig{
		SubLookup:              subMgr.Lookup,
		GeoLookup:              func(netip.Addr) string { return "us" },
		MaxLatencyTableEntries: 16,
		MaxConsecutiveFailures: func() int { return 3 },
		LatencyDecayWindow:     func() time.Duration { return 10 * time.Minute },
	})

	sub := subscription.NewSubscription("sub-1", "Sub1", "https://example.com/sub-1", true, false)
	subMgr.Register(sub)

	// Seed a fully-routable target node into the pool.
	raw := []byte(`{"type":"ss","server":"203.0.113.40","port":443}`)
	targetHash := node.HashFromRawOptions(raw)
	sub.ManagedNodes().StoreNode(targetHash, subscription.ManagedNode{Tags: []string{"target"}})
	entry := node.NewNodeEntry(targetHash, raw, time.Now(), 16)
	entry.AddSubscriptionID(sub.ID)
	entry.SetEgressIP(netip.MustParseAddr("203.0.113.40"))
	ob := testutil.NewNoopOutbound()
	entry.Outbound.Store(&ob)
	pool.LoadNodeFromBootstrap(entry)

	plat := platform.NewPlatform("plat-reassign", "ReassignPlatform", nil, nil)
	plat.StickyTTLNs = int64(30 * time.Minute)
	pool.RegisterPlatform(plat)
	// Build the routable view so the target node is contained.
	plat.FullRebuild(pool.Range, subMgr.Lookup, func(netip.Addr) string { return "us" })

	router := routing.NewRouter(routing.RouterConfig{
		Pool:        pool,
		Authorities: func() []string { return []string{"cloudflare.com"} },
		P2CWindow:   func() time.Duration { return 10 * time.Minute },
	})

	cp := &ControlPlaneService{
		Pool:   pool,
		SubMgr: subMgr,
		Router: router,
	}
	return cp, plat, targetHash
}

func TestReassignLease_Success(t *testing.T) {
	cp, plat, targetHash := newReassignTestService(t)

	// Seed an existing lease pointing at a different (non-routable) hash.
	oldHash := node.HashFromRawOptions([]byte(`{"id":"old-node"}`)).Hex()
	now := time.Now().UnixNano()
	seedLease(t, cp, model.Lease{
		PlatformID:     plat.ID,
		Account:        "alice",
		NodeHash:       oldHash,
		EgressIP:       "198.51.100.7",
		CreatedAtNs:    now - int64(5*time.Minute),
		ExpiryNs:       now + int64(time.Minute),
		LastAccessedNs: now - int64(time.Minute),
	})

	resp, err := cp.ReassignLease(plat.ID, "alice", targetHash.Hex())
	if err != nil {
		t.Fatalf("ReassignLease: %v", err)
	}
	if resp.NodeHash != targetHash.Hex() {
		t.Fatalf("node_hash: got %q, want %q", resp.NodeHash, targetHash.Hex())
	}
	if resp.EgressIP != "203.0.113.40" {
		t.Fatalf("egress_ip: got %q, want 203.0.113.40", resp.EgressIP)
	}
	if resp.Account != "alice" {
		t.Fatalf("account: got %q, want alice", resp.Account)
	}

	// Expiry should be renewed to roughly now + StickyTTLNs.
	got := cp.Router.ReadLease(model.LeaseKey{PlatformID: plat.ID, Account: "alice"})
	if got == nil {
		t.Fatal("expected lease to still exist")
	}
	if got.NodeHash != targetHash.Hex() {
		t.Fatalf("persisted node_hash: got %q, want %q", got.NodeHash, targetHash.Hex())
	}
	wantExpiry := now + plat.StickyTTLNs
	if got.ExpiryNs < wantExpiry-int64(5*time.Second) || got.ExpiryNs > wantExpiry+int64(5*time.Second) {
		t.Fatalf("expiry_ns: got %d, want ~%d", got.ExpiryNs, wantExpiry)
	}
}
```

确认 `internal/service/control_plane_leases_test.go` 顶部 import 已包含 `testutil`；若没有，添加 `"github.com/Resinat/Resin/internal/testutil"`。其余 import（`node`/`platform`/`routing`/`topology`/`subscription`/`model`/`netip`/`time`）该文件已有。

- [ ] **Step 2: 运行测试，确认失败**

Run: `go test ./internal/service/ -run TestReassignLease_Success -v`
Expected: FAIL，编译错误 `cp.ReassignLease undefined`。

- [ ] **Step 3: 实现 ReassignLease**

在 `internal/service/control_plane_leases.go` 中，`GetLease` 方法之后插入：

```go
// ReassignLease moves an existing lease to a different routable node on the same platform.
// It recomputes the egress IP from the target node, renews the expiry to now + platform
// sticky TTL, and preserves the original account and created-at. The replacement is
// persisted via Router.UpsertLease (emits LeaseReplace -> marks dirty -> flushes cache.db).
func (s *ControlPlaneService) ReassignLease(platformID, account, targetNodeHash string) (*LeaseResponse, error) {
	plat, ok := s.Pool.GetPlatform(platformID)
	if !ok {
		return nil, notFound("platform not found")
	}
	account = strings.TrimSpace(account)
	if account == "" {
		return nil, invalidArg("account: must be non-empty")
	}
	targetNodeHash = strings.TrimSpace(targetNodeHash)
	if targetNodeHash == "" {
		return nil, invalidArg("node_hash: must be non-empty")
	}

	targetHash, err := node.ParseHex(targetNodeHash)
	if err != nil {
		return nil, invalidArg("node_hash: invalid format")
	}
	if !plat.View().Contains(targetHash) {
		return nil, invalidArg("node not routable on this platform")
	}
	entry, ok := s.Pool.GetEntry(targetHash)
	if !ok {
		return nil, invalidArg("node not routable on this platform")
	}
	egressIP := entry.GetEgressIP()
	if !egressIP.IsValid() {
		return nil, invalidArg("target node has no egress IP")
	}

	current := s.Router.ReadLease(model.LeaseKey{PlatformID: platformID, Account: account})
	if current == nil {
		return nil, notFound("lease not found")
	}
	nowNs := time.Now().UnixNano()
	if current.ExpiryNs < nowNs {
		return nil, notFound("lease not found")
	}

	next := *current
	next.NodeHash = targetHash.Hex()
	next.EgressIP = egressIP.String()
	next.ExpiryNs = nowNs + plat.StickyTTLNs
	next.LastAccessedNs = nowNs

	if err := s.Router.UpsertLease(next); err != nil {
		return nil, internal("reassign lease", err)
	}

	resp := leaseToResponse(model.Lease{
		PlatformID:     next.PlatformID,
		Account:        next.Account,
		NodeHash:       next.NodeHash,
		EgressIP:       next.EgressIP,
		ExpiryNs:       next.ExpiryNs,
		LastAccessedNs: next.LastAccessedNs,
	}, s.resolveLeaseNodeTag(targetHash))
	return &resp, nil
}
```

确认 `control_plane_leases.go` 顶部 import 已含 `strings`、`time`、`model`、`node`（该文件已有）。`internal`、`notFound`、`invalidArg` 错误构造器在该包已存在（见 `InheritLeaseByPlatformName` 用法）。

- [ ] **Step 4: 运行测试，确认通过**

Run: `go test ./internal/service/ -run TestReassignLease_Success -v`
Expected: PASS。

- [ ] **Step 5: 写失败测试 — 错误分支**

在 `control_plane_leases_test.go` 追加：

```go
func TestReassignLease_NodeNotRoutable(t *testing.T) {
	cp, plat, _ := newReassignTestService(t)

	// A hash that is NOT in the platform's routable view.
	otherHash := node.HashFromRawOptions([]byte(`{"id":"not-routable-node"}`)).Hex()
	now := time.Now().UnixNano()
	seedLease(t, cp, model.Lease{
		PlatformID: plat.ID, Account: "alice",
		NodeHash: node.HashFromRawOptions([]byte(`{"id":"old-node"}`)).Hex(),
		EgressIP: "198.51.100.7", CreatedAtNs: now, ExpiryNs: now + int64(time.Minute),
		LastAccessedNs: now,
	})

	_, err := cp.ReassignLease(plat.ID, "alice", otherHash)
	if err == nil {
		t.Fatal("expected INVALID_ARGUMENT for non-routable node")
	}
	assertServiceErrorCode(t, err, "INVALID_ARGUMENT")
}

func TestReassignLease_LeaseMissingOrExpired(t *testing.T) {
	cp, plat, targetHash := newReassignTestService(t)

	// No lease seeded -> not found.
	_, err := cp.ReassignLease(plat.ID, "ghost", targetHash.Hex())
	if err == nil {
		t.Fatal("expected NOT_FOUND for missing lease")
	}
	assertServiceErrorCode(t, err, "NOT_FOUND")

	// Expired lease -> not found.
	now := time.Now().UnixNano()
	seedLease(t, cp, model.Lease{
		PlatformID: plat.ID, Account: "bob",
		NodeHash: node.HashFromRawOptions([]byte(`{"id":"old-node"}`)).Hex(),
		EgressIP: "198.51.100.8", CreatedAtNs: now - int64(time.Hour),
		ExpiryNs: now - int64(time.Second), LastAccessedNs: now - int64(time.Minute),
	})
	_, err = cp.ReassignLease(plat.ID, "bob", targetHash.Hex())
	if err == nil {
		t.Fatal("expected NOT_FOUND for expired lease")
	}
	assertServiceErrorCode(t, err, "NOT_FOUND")
}

func TestReassignLease_PlatformMissing(t *testing.T) {
	cp, _, targetHash := newReassignTestService(t)
	_, err := cp.ReassignLease("no-such-platform", "alice", targetHash.Hex())
	if err == nil {
		t.Fatal("expected NOT_FOUND for missing platform")
	}
	assertServiceErrorCode(t, err, "NOT_FOUND")
}
```

- [ ] **Step 6: 运行全部 ReassignLease 测试**

Run: `go test ./internal/service/ -run TestReassignLease -v`
Expected: PASS（4 个用例全过）。

- [ ] **Step 7: 提交**

```bash
git add internal/service/control_plane_leases.go internal/service/control_plane_leases_test.go
git commit -m "feat: add ReassignLease service method"
```

---

## Task 2: 后端 — ReassignLease HTTP handler 与路由

**Files:**
- Modify: `internal/api/handler_lease.go`
- Modify: `internal/api/server.go`
- Test: `internal/api/handler_lease_test.go`（若不存在则新建）

- [ ] **Step 1: 确认 handler 测试文件与夹具**

Run: `ls internal/api/handler_lease_test.go 2>/dev/null && head -60 internal/api/handler_lease_test.go`
若文件存在，查看其现有的 service mock/构造方式（用于复用夹具）。若不存在，Step 3 将新建并自带最小夹具。

- [ ] **Step 2: 实现 HandleReassignLease**

在 `internal/api/handler_lease.go` 的 `HandleDeleteLease` 之后插入：

```go
// HandleReassignLease returns a handler for PUT /api/v1/platforms/{id}/leases/{account}.
// It moves the lease to a different routable node specified by node_hash in the body.
func HandleReassignLease(cp *service.ControlPlaneService) http.HandlerFunc {
	type reassignRequest struct {
		NodeHash string `json:"node_hash"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		platformID, ok := requireUUIDPathParam(w, r, "id", "platform_id")
		if !ok {
			return
		}
		account, err := validateAccountPath(r)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		var req reassignRequest
		if err := DecodeJSONBody(w, r, &req); err != nil {
			return
		}
		req.NodeHash = strings.TrimSpace(req.NodeHash)
		if req.NodeHash == "" {
			writeInvalidArgument(w, "node_hash: must be non-empty")
			return
		}
		lease, err := cp.ReassignLease(platformID, account, req.NodeHash)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, lease)
	}
}
```

确认 `DecodeJSONBody`、`WriteJSON`、`writeInvalidArgument`、`writeServiceError`、`requireUUIDPathParam`、`validateAccountPath` 在 `internal/api` 包中已存在（`HandleDeleteLease` 等已用）。`strings` 已 import。

- [ ] **Step 3: 注册路由**

在 `internal/api/server.go` 第 92 行（`HandleDeleteLease` 行）之后插入：

```go
		authed.Handle("PUT /api/v1/platforms/{id}/leases/{account}", HandleReassignLease(cp))
```

- [ ] **Step 4: 编译**

Run: `go build ./...`
Expected: 编译通过，无错误。

- [ ] **Step 5: 写 handler 测试**

若 `internal/api/handler_lease_test.go` 不存在则新建；已存在则在末尾追加。复用其现有的 service 构造方式；若该文件用真实 `ControlPlaneService`，参考 Task 1 夹具；若用接口 mock，则为 `ReassignLease` 增加对应 mock 分支。最小真实路径测试（若新建文件）：

```go
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/model"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/platform"
	"github.com/Resinat/Resin/internal/routing"
	"github.com/Resinat/Resin/internal/service"
	"github.com/Resinat/Resin/internal/subscription"
	"github.com/Resinat/Resin/internal/testutil"
	"github.com/Resinat/Resin/internal/topology"
)

func newReassignHandlerService(t *testing.T) (*service.ControlPlaneService, *platform.Platform, node.Hash) {
	t.Helper()
	subMgr := topology.NewSubscriptionManager()
	pool := topology.NewGlobalNodePool(topology.PoolConfig{
		SubLookup:              subMgr.Lookup,
		GeoLookup:              func(netip.Addr) string { return "us" },
		MaxLatencyTableEntries: 16,
		MaxConsecutiveFailures: func() int { return 3 },
		LatencyDecayWindow:     func() time.Duration { return 10 * time.Minute },
	})
	sub := subscription.NewSubscription("sub-1", "Sub1", "https://example.com/sub-1", true, false)
	subMgr.Register(sub)
	raw := []byte(`{"type":"ss","server":"203.0.113.40","port":443}`)
	targetHash := node.HashFromRawOptions(raw)
	sub.ManagedNodes().StoreNode(targetHash, subscription.ManagedNode{Tags: []string{"target"}})
	entry := node.NewNodeEntry(targetHash, raw, time.Now(), 16)
	entry.AddSubscriptionID(sub.ID)
	entry.SetEgressIP(netip.MustParseAddr("203.0.113.40"))
	ob := testutil.NewNoopOutbound()
	entry.Outbound.Store(&ob)
	pool.LoadNodeFromBootstrap(entry)
	plat := platform.NewPlatform("plat-reassign", "ReassignPlatform", nil, nil)
	plat.StickyTTLNs = int64(30 * time.Minute)
	pool.RegisterPlatform(plat)
	plat.FullRebuild(pool.Range, subMgr.Lookup, func(netip.Addr) string { return "us" })
	router := routing.NewRouter(routing.RouterConfig{
		Pool: pool, Authorities: func() []string { return []string{"cloudflare.com" } },
		P2CWindow: func() time.Duration { return 10 * time.Minute },
	})
	cp := &service.ControlPlaneService{Pool: pool, SubMgr: subMgr, Router: router}

	// Seed existing lease.
	now := time.Now().UnixNano()
	if err := router.UpsertLease(model.Lease{
		PlatformID: plat.ID, Account: "alice",
		NodeHash: node.HashFromRawOptions([]byte(`{"id":"old-node"}`)).Hex(),
		EgressIP: "198.51.100.7", CreatedAtNs: now, ExpiryNs: now + int64(time.Minute),
		LastAccessedNs: now,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return cp, plat, targetHash
}

func TestHandleReassignLease_Success(t *testing.T) {
	cp, plat, targetHash := newReassignHandlerService(t)
	body, _ := json.Marshal(map[string]string{"node_hash": targetHash.Hex()})
	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/platforms/"+plat.ID+"/leases/alice", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	HandleReassignLease(cp).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp service.LeaseResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.NodeHash != targetHash.Hex() {
		t.Fatalf("node_hash: got %q, want %q", resp.NodeHash, targetHash.Hex())
	}
}

func TestHandleReassignLease_EmptyNodeHash(t *testing.T) {
	cp, plat, _ := newReassignHandlerService(t)
	body, _ := json.Marshal(map[string]string{"node_hash": "  "})
	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/platforms/"+plat.ID+"/leases/alice", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	HandleReassignLease(cp).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rr.Code)
	}
}

func TestHandleReassignLease_NodeNotRoutable(t *testing.T) {
	cp, plat, _ := newReassignHandlerService(t)
	other := node.HashFromRawOptions([]byte(`{"id":"not-routable"}`)).Hex()
	body, _ := json.Marshal(map[string]string{"node_hash": other})
	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/platforms/"+plat.ID+"/leases/alice", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	HandleReassignLease(cp).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rr.Code)
	}
}
```

新建文件时需在 import 中加 `"bytes"`。

> 注意：若仓库已有 `handler_lease_test.go` 且其用 mock 接口而非真实 service，则按该文件既有 mock 模式为 `ReassignLease` 增加用例，不要引入真实 service 夹具造成风格冲突。实现前先读该文件决定。

- [ ] **Step 6: 运行 handler 测试**

Run: `go test ./internal/api/ -run TestHandleReassignLease -v`
Expected: PASS（3 个用例全过）。

- [ ] **Step 7: 全量后端测试**

Run: `go test ./internal/...`
Expected: 全部 PASS，无回归。

- [ ] **Step 8: 提交**

```bash
git add internal/api/handler_lease.go internal/api/server.go internal/api/handler_lease_test.go
git commit -m "feat: add PUT lease reassign handler and route"
```

---

## Task 3: 前端 — 类型与 API 函数

**Files:**
- Modify: `webui/src/features/platforms/types.ts`
- Modify: `webui/src/features/platforms/api.ts`

- [ ] **Step 1: 追加 LeaseResponse 类型**

在 `webui/src/features/platforms/types.ts` 末尾追加：

```ts
export type LeaseResponse = {
  platform_id: string;
  account: string;
  node_hash: string;
  node_tag: string;
  egress_ip: string;
  expiry: string;
  last_accessed: string;
};

export type ReassignLeaseInput = {
  node_hash: string;
};
```

确认该文件已有 `PageResponse<T>` 类型（explore 报告确认存在）；若没有，一并追加：

```ts
export type PageResponse<T> = {
  items: T[];
  total: number;
  limit: number;
  offset: number;
};
```

- [ ] **Step 2: 追加 API 函数**

在 `webui/src/features/platforms/api.ts` 末尾（`clearAllPlatformLeases` 之后）追加。复用文件中已有的 `apiRequest` 与 `basePath`（`basePath = "/api/v1/platforms"`）。

```ts
export type ListPlatformLeasesInput = {
  account?: string;
  fuzzy?: boolean;
  sort_by?: "account" | "expiry" | "last_accessed";
  sort_order?: "asc" | "desc";
  limit?: number;
  offset?: number;
};

export async function listPlatformLeases(
  platformId: string,
  input: ListPlatformLeasesInput = {},
): Promise<PageResponse<LeaseResponse>> {
  const params = new URLSearchParams();
  if (input.account) params.set("account", input.account);
  if (input.fuzzy) params.set("fuzzy", "true");
  if (input.sort_by) params.set("sort_by", input.sort_by);
  if (input.sort_order) params.set("sort_order", input.sort_order);
  if (input.limit !== undefined) params.set("limit", String(input.limit));
  if (input.offset !== undefined) params.set("offset", String(input.offset));
  const qs = params.toString();
  const path = `${basePath}/${platformId}/leases${qs ? `?${qs}` : ""}`;
  return apiRequest<PageResponse<LeaseResponse>>(path);
}

export async function reassignLease(
  platformId: string,
  account: string,
  input: ReassignLeaseInput,
): Promise<LeaseResponse> {
  return apiRequest<LeaseResponse>(
    `${basePath}/${platformId}/leases/${encodeURIComponent(account)}`,
    { method: "PUT", body: input },
  );
}

export async function deleteLease(platformId: string, account: string): Promise<void> {
  await apiRequest<void>(
    `${basePath}/${platformId}/leases/${encodeURIComponent(account)}`,
    { method: "DELETE" },
  );
}
```

确认 import 中已引入 `LeaseResponse`、`PageResponse`、`ReassignLeaseInput`（同目录 `./types`）。`apiRequest` 已在该文件 import。

- [ ] **Step 3: 类型检查**

Run: `cd webui && npm run typecheck`（或 `npx tsc --noEmit`，按项目实际脚本）
Expected: 无类型错误。若 `npm run typecheck` 脚本不存在，用 `npx tsc --noEmit`。

- [ ] **Step 4: 提交**

```bash
git add webui/src/features/platforms/types.ts webui/src/features/platforms/api.ts
git commit -m "feat(webui): add lease list/reassign/delete api and types"
```

---

## Task 4: 前端 — 重指节点弹窗组件

**Files:**
- Create: `webui/src/features/platforms/ReassignLeaseDialog.tsx`

弹窗用项目既有内联模式：`modal-overlay` + `Card` + `useState` 控制（见 PlatformPage.tsx 的 createModal 模式）。节点列表来自 `listNodes({ platform_id, has_outbound: true, limit: 大 })`，前端按 tag 本地过滤。需确认 `listNodes` 与 `NodeSummary` 的导出路径。

- [ ] **Step 1: 确认 nodes API 与类型导出**

Run: `grep -n "export.*listNodes\|export.*NodeSummary\|has_outbound" webui/src/features/nodes/api.ts webui/src/features/nodes/types.ts`
确认 `listNodes`、`NodeSummary` 的导出名与 `has_outbound` 过滤参数。记录 import 路径用于下一步。

- [ ] **Step 2: 创建弹窗组件**

新建 `webui/src/features/platforms/ReassignLeaseDialog.tsx`：

```tsx
import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Button } from "../../components/ui/Button";
import { Card } from "../../components/ui/Card";
import { Input } from "../../components/ui/Input";
import { useI18n } from "../../i18n";
import { listNodes } from "../nodes/api";
import type { NodeSummary } from "../nodes/types";
import { reassignLease } from "./api";
import type { LeaseResponse } from "./types";

type Props = {
  platformId: string;
  lease: LeaseResponse;
  onClose: () => void;
  onReassigned: () => void;
  showToast: (tone: "success" | "error", text: string) => void;
};

export function ReassignLeaseDialog({ platformId, lease, onClose, onReassigned, showToast }: Props) {
  const { t } = useI18n();
  const [keyword, setKeyword] = useState("");
  const [selectedHash, setSelectedHash] = useState<string>("");
  const [submitting, setSubmitting] = useState(false);

  const nodesQuery = useQuery({
    queryKey: ["nodes", "reassign", platformId],
    queryFn: () =>
      listNodes({ platform_id: platformId, has_outbound: true, limit: 1000, offset: 0 }),
    staleTime: 30_000,
  });
  const nodes: NodeSummary[] = nodesQuery.data?.items ?? [];

  const filtered = useMemo(() => {
    const kw = keyword.trim().toLowerCase();
    if (!kw) return nodes;
    return nodes.filter((n) => (n.tag ?? "").toLowerCase().includes(kw));
  }, [nodes, keyword]);

  const confirm = async () => {
    if (!selectedHash || submitting) return;
    setSubmitting(true);
    try {
      const updated = await reassignLease(platformId, lease.account, { node_hash: selectedHash });
      showToast("success", t("租约已重指到 {{tag}}", { tag: updated.node_tag || selectedHash.slice(0, 8) }));
      onReassigned();
      onClose();
    } catch (err) {
      showToast("error", t("重指租约失败"));
      // eslint-disable-next-line no-console
      console.error(err);
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div className="modal-overlay" role="dialog" aria-modal="true">
      <Card className="modal-card">
        <div className="modal-header">
          <h3>{t("重指租约节点 — {{account}}", { account: lease.account })}</h3>
          <Button variant="ghost" size="sm" onClick={onClose}>{t("关闭")}</Button>
        </div>
        <div className="form-grid">
          <p className="platform-monitor-kpi-sub">
            {t("当前节点")}: {lease.node_tag || lease.node_hash.slice(0, 8)} · {t("出口 IP")}: {lease.egress_ip}
          </p>
          <div>
            <label>{t("搜索节点")}</label>
            <Input value={keyword} onChange={(e) => setKeyword(e.target.value)} placeholder={t("按节点名过滤")} />
          </div>
          <div className="reassign-node-list">
            {filtered.length === 0 ? (
              <p className="platform-monitor-kpi-sub">{t("无可选节点")}</p>
            ) : (
              filtered.map((n) => (
                <label key={n.node_hash} className={`reassign-node-option${selectedHash === n.node_hash ? " selected" : ""}`}>
                  <input
                    type="radio"
                    name="reassign-target"
                    value={n.node_hash}
                    checked={selectedHash === n.node_hash}
                    onChange={() => setSelectedHash(n.node_hash)}
                  />
                  <span>{n.tag ?? n.node_hash.slice(0, 8)}</span>
                  <span className="platform-monitor-kpi-sub">{n.egress_ip}</span>
                </label>
              ))
            )}
          </div>
          <div className="detail-actions">
            <Button onClick={confirm} disabled={!selectedHash || submitting}>{t("确认重指")}</Button>
            <Button variant="secondary" onClick={onClose}>{t("取消")}</Button>
          </div>
        </div>
      </Card>
    </div>
  );
}
```

> `NodeSummary` 字段名（`tag`/`egress_ip`/`node_hash`）以 Step 1 实际确认为准；若 NodesPage 用 `firstTag(node)` 取 tag，则这里同样用一个 helper 或直接 `n.tag`。Step 1 已记录确切字段名，按之调整。

- [ ] **Step 3: 类型检查**

Run: `cd webui && npx tsc --noEmit`
Expected: 无类型错误。按 Step 1 确认的字段名修正 `NodeSummary` 取值。

- [ ] **Step 4: 提交**

```bash
git add webui/src/features/platforms/ReassignLeaseDialog.tsx
git commit -m "feat(webui): add reassign lease dialog"
```

---

## Task 5: 前端 — 租约列表页面

**Files:**
- Create: `webui/src/features/platforms/PlatformLeasesPage.tsx`
- Modify: `webui/src/app/routes.tsx`

仿照 NodesPage：`useQuery` + 手动刷新（不轮询）、`DataTable` + `OffsetPagination`、搜索栏、行操作菜单、清除租约确认（`window.confirm`）、重指弹窗。

- [ ] **Step 1: 创建页面组件**

新建 `webui/src/features/platforms/PlatformLeasesPage.tsx`：

```tsx
import { useMemo, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { createColumnHelper } from "@tanstack/react-table";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { RefreshCw, MoreHorizontal } from "lucide-react";
import { Button } from "../../components/ui/Button";
import { Card } from "../../components/ui/Card";
import { DataTable } from "../../components/ui/DataTable";
import { Input } from "../../components/ui/Input";
import { OffsetPagination } from "../../components/ui/OffsetPagination";
import { ToastContainer } from "../../components/ui/Toast";
import { useToast } from "../../hooks/useToast";
import { useI18n } from "../../i18n";
import { deleteLease, listPlatformLeases } from "./api";
import type { LeaseResponse } from "./types";
import { ReassignLeaseDialog } from "./ReassignLeaseDialog";

const PAGE_SIZE = 20;

function timeAgo(iso: string, now: number): string {
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return iso;
  const diff = now - t;
  const min = Math.floor(diff / 60000);
  if (min < 1) return "刚刚";
  if (min < 60) return `${min} 分钟前`;
  const hr = Math.floor(min / 60);
  if (hr < 24) return `${hr} 小时前`;
  return `${Math.floor(hr / 24)} 天前`;
}

function remaining(iso: string, now: number): { text: string; tone: "expired" | "warn" | "ok" } {
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return { text: "-", tone: "ok" };
  const diff = t - now;
  if (diff <= 0) return { text: "已过期", tone: "expired" };
  const sec = Math.floor(diff / 1000);
  if (sec < 60) return { text: `${sec} 秒`, tone: "warn" };
  const min = Math.floor(sec / 60);
  if (min < 60) return { text: `${min} 分`, tone: min <= 1 ? "warn" : "ok" };
  const hr = Math.floor(min / 60);
  return { text: `${hr} 时 ${min % 60} 分`, tone: "ok" };
}

export function PlatformLeasesPage() {
  const { platformId } = useParams<{ platformId: string }>();
  const { t } = useI18n();
  const queryClient = useQueryClient();
  const { toasts, showToast, dismissToast } = useToast();

  const [accountKeyword, setAccountKeyword] = useState("");
  const [sortBy, setSortBy] = useState<"account" | "expiry" | "last_accessed">("expiry");
  const [sortOrder, setSortOrder] = useState<"asc" | "desc">("asc");
  const [page, setPage] = useState(0);
  const [egressFilter, setEgressFilter] = useState("");
  const [now, setNow] = useState(Date.now());
  const [menuFor, setMenuFor] = useState<string | null>(null);
  const [reassignFor, setReassignFor] = useState<LeaseResponse | null>(null);

  const queryKey = useMemo(
    () => ["platform-leases", platformId, accountKeyword, sortBy, sortOrder, page] as const,
    [platformId, accountKeyword, sortBy, sortOrder, page],
  );

  const leasesQuery = useQuery({
    queryKey,
    queryFn: () =>
      listPlatformLeases(platformId!, {
        account: accountKeyword || undefined,
        fuzzy: true,
        sort_by: sortBy,
        sort_order: sortOrder,
        limit: PAGE_SIZE,
        offset: page * PAGE_SIZE,
      }),
    enabled: Boolean(platformId),
    placeholderData: (prev) => prev,
  });

  const pageData = leasesQuery.data ?? { items: [], total: 0, limit: PAGE_SIZE, offset: page * PAGE_SIZE };
  const items = egressFilter.trim()
    ? pageData.items.filter((l) => l.egress_ip.includes(egressFilter.trim()))
    : pageData.items;
  const totalPages = Math.max(1, Math.ceil(pageData.total / PAGE_SIZE));

  const refresh = async () => {
    setNow(Date.now());
    await queryClient.invalidateQueries({ queryKey: ["platform-leases"] });
  };

  const removeLease = async (lease: LeaseResponse) => {
    if (!window.confirm(t("确认清除租约 {{account}}？", { account: lease.account }))) return;
    try {
      await deleteLease(platformId!, lease.account);
      showToast("success", t("租约 {{account}} 已清除", { account: lease.account }));
      await refresh();
    } catch (err) {
      showToast("error", t("清除租约失败"));
      // eslint-disable-next-line no-console
      console.error(err);
    }
  };

  const col = createColumnHelper<LeaseResponse>();
  const columns = [
    col.accessor("account", {
      header: () => (
        <button type="button" className="table-sort-btn" onClick={() => { setSortBy("account"); setSortOrder(sortBy === "account" && sortOrder === "asc" ? "desc" : "asc"); }}>
          {t("账号")}<span>{sortBy === "account" ? (sortOrder === "asc" ? "▲" : "▼") : ""}</span>
        </button>
      ),
      cell: (info) => info.getValue(),
    }),
    col.accessor("node_tag", {
      header: t("节点"),
      cell: (info) => {
        const l = info.row.original;
        return (
          <span title={l.node_hash}>
            {l.node_tag || l.node_hash.slice(0, 8)}
          </span>
        );
      },
    }),
    col.accessor("egress_ip", { header: t("出口 IP"), cell: (info) => info.getValue() }),
    col.accessor("expiry", {
      header: () => (
        <button type="button" className="table-sort-btn" onClick={() => { setSortBy("expiry"); setSortOrder(sortBy === "expiry" && sortOrder === "asc" ? "desc" : "asc"); }}>
          {t("剩余存活")}<span>{sortBy === "expiry" ? (sortOrder === "asc" ? "▲" : "▼") : ""}</span>
        </button>
      ),
      cell: (info) => {
        const r = remaining(info.getValue(), now);
        return <span className={`lease-remaining lease-${r.tone}`} title={info.getValue()}>{r.text}</span>;
      },
    }),
    col.accessor("last_accessed", {
      header: () => (
        <button type="button" className="table-sort-btn" onClick={() => { setSortBy("last_accessed"); setSortOrder(sortBy === "last_accessed" && sortOrder === "asc" ? "desc" : "asc"); }}>
          {t("最近访问")}<span>{sortBy === "last_accessed" ? (sortOrder === "asc" ? "▲" : "▼") : ""}</span>
        </button>
      ),
      cell: (info) => <span title={info.getValue()}>{timeAgo(info.getValue(), now)}</span>,
    }),
    col.display({
      id: "actions",
      header: t("操作"),
      cell: (info) => {
        const l = info.row.original;
        const open = menuFor === l.account;
        return (
          <div className="table-actions">
            <Button variant="ghost" size="sm" onClick={() => setMenuFor(open ? null : l.account)}>
              <MoreHorizontal size={16} />
            </Button>
            {open ? (
              <div className="table-action-menu" onMouseLeave={() => setMenuFor(null)}>
                <button type="button" onClick={() => { setMenuFor(null); setReassignFor(l); }}>{t("重指节点")}</button>
                <button type="button" onClick={() => { setMenuFor(null); void removeLease(l); }}>{t("清除租约")}</button>
              </div>
            ) : null}
          </div>
        );
      },
    }),
  ];

  return (
    <div className="page-container">
      <div className="page-header">
        <div>
          <h2>{t("活跃租约")}</h2>
          <Link to={`/platforms/${platformId}`} className="platform-monitor-kpi-sub">{t("返回平台详情")}</Link>
        </div>
        <Button size="sm" variant="secondary" onClick={refresh} disabled={leasesQuery.isFetching}>
          <RefreshCw size={16} className={leasesQuery.isFetching ? "spin" : undefined} />
          {t("刷新")}
        </Button>
      </div>

      <Card className="filter-bar">
        <div className="filter-item">
          <label>{t("账号")}</label>
          <Input value={accountKeyword} onChange={(e) => { setAccountKeyword(e.target.value); setPage(0); }} placeholder={t("模糊搜索")} />
        </div>
        <div className="filter-item">
          <label>{t("出口 IP")}</label>
          <Input value={egressFilter} onChange={(e) => setEgressFilter(e.target.value)} placeholder={t("本地过滤")} />
        </div>
      </Card>

      {items.length ? (
        <DataTable data={items} columns={columns} getRowId={(l) => l.account} />
      ) : (
        <Card><p className="platform-monitor-kpi-sub">{leasesQuery.isLoading ? t("加载中…") : t("租约列表为空")}</p></Card>
      )}

      <OffsetPagination
        page={page}
        totalPages={totalPages}
        totalItems={pageData.total}
        pageSize={PAGE_SIZE}
        onPageChange={setPage}
      />

      {reassignFor ? (
        <ReassignLeaseDialog
          platformId={platformId!}
          lease={reassignFor}
          onClose={() => setReassignFor(null)}
          onReassigned={() => void refresh()}
          showToast={showToast}
        />
      ) : null}

      <ToastContainer toasts={toasts} onDismiss={dismissToast} />
    </div>
  );
}
```

> `OffsetPagination` 的 props（`page`/`totalPages`/`totalItems`/`pageSize`/`onPageChange`）以 NodesPage 实际用法为准（explore 报告已确认该签名）。若该组件还要求 `pageSizeOptions`/`onPageSizeChange`，按 NodesPage 补齐。

- [ ] **Step 2: 注册路由**

在 `webui/src/app/routes.tsx` 中：
1. 顶部 import：`import { PlatformLeasesPage } from "../features/platforms/PlatformLeasesPage";`
2. 在 `/platforms/:platformId` 路由行（第 35 行）之后插入：

```tsx
<Route path="/platforms/:platformId/leases" element={<PlatformLeasesPage />} />
```

- [ ] **Step 3: 类型检查**

Run: `cd webui && npx tsc --noEmit`
Expected: 无类型错误。按 NodesPage/OffsetPagination 实际签名修正 props。

- [ ] **Step 4: 提交**

```bash
git add webui/src/features/platforms/PlatformLeasesPage.tsx webui/src/app/routes.tsx
git commit -m "feat(webui): add platform leases page"
```

- [ ] **Step 5: 追加批量清除能力**

在 `PlatformLeasesPage.tsx` 中增加表头多选与批量清除。在现有 state 旁加：

```tsx
const [selected, setSelected] = useState<Set<string>>(new Set());
```

在 `columns` 数组开头插入选择列：

```tsx
col.display({
  id: "select",
  header: () => (
    <input
      type="checkbox"
      aria-label={t("全选")}
      checked={items.length > 0 && items.every((l) => selected.has(l.account))}
      onChange={(e) => {
        const next = new Set(selected);
        if (e.target.checked) items.forEach((l) => next.add(l.account));
        else items.forEach((l) => next.delete(l.account));
        setSelected(next);
      }}
    />
  ),
  cell: (info) => (
    <input
      type="checkbox"
      aria-label={t("选择")}
      checked={selected.has(info.row.original.account)}
      onChange={(e) => {
        const next = new Set(selected);
        if (e.target.checked) next.add(info.row.original.account);
        else next.delete(info.row.original.account);
        setSelected(next);
      }}
    />
  ),
}),
```

在 `filter-bar` 的 `Card` 内末尾加批量清除按钮：

```tsx
<Button
  size="sm"
  variant="secondary"
  disabled={selected.size === 0}
  onClick={async () => {
    if (!window.confirm(t("确认清除选中的 {{n}} 条租约？", { n: selected.size }))) return;
    try {
      await Promise.all([...selected].map((account) => deleteLease(platformId!, account)));
      showToast("success", t("已清除 {{n}} 条租约", { n: selected.size }));
      setSelected(new Set());
      await refresh();
    } catch (err) {
      showToast("error", t("批量清除失败"));
      console.error(err);
    }
  }}
>
  {t("批量清除")} ({selected.size})
</Button>
```

并在 `refresh` 里清空选择：`setSelected(new Set());`（加在 `setNow(Date.now())` 之后）。

- [ ] **Step 6: 类型检查**

Run: `cd webui && npx tsc --noEmit`
Expected: 无类型错误。

- [ ] **Step 7: 提交**

```bash
git add webui/src/features/platforms/PlatformLeasesPage.tsx
git commit -m "feat(webui): add bulk clear leases selection"
```

---

## Task 6: 前端 — KPI 卡片入口链接 + 文案

**Files:**
- Modify: `webui/src/features/platforms/PlatformMonitorPanel.tsx`
- Modify: `webui/src/i18n/translations.ts`

- [ ] **Step 1: 给"活跃租约"KPI 卡片加跳转链接**

在 `webui/src/features/platforms/PlatformMonitorPanel.tsx` 的"活跃租约"卡片（约第 851-860 行）内，`</Card>` 之前插入一个 `<Link>`，参照同文件"可路由节点"卡片（约第 884 行）的写法。确认 `Link`、`Link2` 已在该文件 import（"可路由节点"卡片已用）。

将原卡片：

```tsx
<Card className="platform-monitor-kpi-card">
  <div className="dashboard-kpi-icon lease">
    <Layers size={18} />
  </div>
  <div>
    <p className="platform-monitor-kpi-label">{t("活跃租约")}</p>
    <p className="platform-monitor-kpi-value">{formatCount(latestActiveLeases)}</p>
    <p className="platform-monitor-kpi-sub">{t("当前实时值")}</p>
  </div>
</Card>
```

改为：

```tsx
<Card className="platform-monitor-kpi-card">
  <div className="dashboard-kpi-icon lease">
    <Layers size={18} />
  </div>
  <div>
    <p className="platform-monitor-kpi-label">{t("活跃租约")}</p>
    <p className="platform-monitor-kpi-value">{formatCount(latestActiveLeases)}</p>
    <p className="platform-monitor-kpi-sub">{t("当前实时值")}</p>
  </div>
  <Link to={`/platforms/${platform.id}/leases`} className="platform-monitor-kpi-link">
    <Link2 size={14} />
    <span>{t("活跃租约")}</span>
  </Link>
</Card>
```

- [ ] **Step 2: 追加 i18n 文案**

在 `webui/src/i18n/translations.ts` 的 `EXACT_ZH_TO_EN` 对象末尾追加（中文 key 即默认文案，英文为 value）：

```ts
"平台租约": "Platform Leases",
"活跃租约列表": "Active Lease List",
"账号": "Account",
"剩余存活": "Remaining TTL",
"最近访问": "Last Accessed",
"重指节点": "Reassign Node",
"重指租约节点 — {{account}}": "Reassign lease node — {{account}}",
"确认重指": "Confirm Reassign",
"搜索节点": "Search node",
"按节点名过滤": "Filter by node name",
"无可选节点": "No selectable nodes",
"当前节点": "Current node",
"租约已重指到 {{tag}}": "Lease reassigned to {{tag}}",
"重指租约失败": "Failed to reassign lease",
"确认清除租约 {{account}}？": "Clear lease {{account}}?",
"租约 {{account}} 已清除": "Lease {{account}} cleared",
"清除租约失败": "Failed to clear lease",
"清除租约": "Clear Lease",
"返回平台详情": "Back to platform detail",
"本地过滤": "Local filter",
"租约列表为空": "No leases found",
"加载中…": "Loading…",
"刚刚": "just now",
"已过期": "expired",
"全选": "Select all",
"选择": "Select",
"批量清除": "Bulk Clear",
"确认清除选中的 {{n}} 条租约？": "Clear {{n}} selected leases?",
"已清除 {{n}} 条租约": "Cleared {{n}} leases",
"批量清除失败": "Bulk clear failed",
```

> 项目约定中文文本即 key；`useI18n` 的 `t("刚刚")` 等在 `buildZhTranslations` 下原样返回。新增 key 不与现有重复即可（先用 `grep` 确认）。

- [ ] **Step 3: 类型检查 + 确认无重复 key**

Run: `cd webui && grep -n '"账号":' src/i18n/translations.ts` 等逐条确认无重复，再 `npx tsc --noEmit`
Expected: 无重复、无类型错误。

- [ ] **Step 4: 提交**

```bash
git add webui/src/features/platforms/PlatformMonitorPanel.tsx webui/src/i18n/translations.ts
git commit -m "feat(webui): link active leases KPI to leases page and add copy"
```

---

## Task 7: 前端样式与端到端校验

**Files:**
- Modify: `webui/src/styles/theme.css`（可选，按需补样式）

- [ ] **Step 1: 补充必要 CSS（若现有 class 不足）**

在 `webui/src/styles/theme.css` 末尾追加租约页用到的、尚不存在的 class（仅补缺，复用 NodesPage/modal 既有样式）。先用 `grep` 确认哪些已存在：

Run: `cd webui && grep -n "modal-overlay\|modal-card\|modal-header\|table-action-menu\|table-actions\|filter-bar\|filter-item\|page-container\|page-header\|spin\|table-sort-btn\|detail-actions\|form-grid" src/styles/theme.css`

对缺失的 class（重点是 `.reassign-node-list`、`.reassign-node-option`、`.lease-remaining`、`.lease-expired`、`.lease-warn`）追加：

```css
.reassign-node-list {
  max-height: 320px;
  overflow-y: auto;
  display: flex;
  flex-direction: column;
  gap: 4px;
}
.reassign-node-option {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 6px 8px;
  border: 1px solid var(--border-color, #ddd);
  border-radius: 6px;
  cursor: pointer;
}
.reassign-node-option.selected {
  border-color: var(--accent-color, #2563eb);
  background: var(--accent-soft, rgba(37, 99, 235, 0.08));
}
.lease-remaining { font-variant-numeric: tabular-nums; }
.lease-expired { color: var(--danger-color, #dc2626); }
.lease-warn { color: var(--warn-color, #d97706); }
```

其余 class（`modal-overlay` 等）若已存在则不动。

- [ ] **Step 2: 前端构建**

Run: `cd webui && npm run build`
Expected: 构建成功，无 TS/lint 错误。

- [ ] **Step 3: 启动并人工冒烟（可选，若环境允许）**

启动后端 + 前端，进入某平台详情 → 点"活跃租约"卡片 → 进入租约页 → 验证列表/搜索/排序/分页 → 对某行点"重指节点"选目标节点确认 → 验证节点变更 → 点"清除租约"确认 → 验证行消失。

- [ ] **Step 4: 提交**

```bash
git add webui/src/styles/theme.css
git commit -m "feat(webui): add lease page dialog and remaining-ttl styles"
```

---

## Task 8: 收尾 — 全量验证

- [ ] **Step 1: 后端全量测试**

Run: `go test ./...`
Expected: 全部 PASS。

- [ ] **Step 2: 后端 vet + build**

Run: `go vet ./... && go build ./...`
Expected: 无错误。

- [ ] **Step 3: 前端 typecheck + build**

Run: `cd webui && npx tsc --noEmit && npm run build`
Expected: 无错误。

- [ ] **Step 4: 回顾 spec 对照**

逐条核对 `docs/superpowers/specs/2026-06-26-platform-leases-table-design.md` 的"改动文件清单"与"不做"清单，确认无遗漏、无越界。

- [ ] **Step 5: 最终提交（如有剩余改动）**

```bash
git add -A
git commit -m "chore: lease table final verification"
```
（无剩余改动则跳过。）
