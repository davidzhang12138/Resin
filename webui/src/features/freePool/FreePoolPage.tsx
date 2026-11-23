import { useMutation, useQuery } from "@tanstack/react-query";
import { type ColumnDef } from "@tanstack/react-table";
import {
  Activity,
  AlertTriangle,
  CheckCircle2,
  Gauge,
  KeyRound,
  Play,
  Plus,
  RefreshCw,
  RotateCcw,
  Save,
  Search,
  ShieldCheck,
  Square,
  Trash2,
} from "lucide-react";
import { useMemo, useState } from "react";
import { Badge } from "../../components/ui/Badge";
import { Button } from "../../components/ui/Button";
import { Card } from "../../components/ui/Card";
import { DataTable } from "../../components/ui/DataTable";
import { Input } from "../../components/ui/Input";
import { Select } from "../../components/ui/Select";
import { Switch } from "../../components/ui/Switch";
import { ToastContainer } from "../../components/ui/Toast";
import { useToast } from "../../hooks/useToast";
import { useI18n } from "../../i18n";
import { formatApiErrorMessage } from "../../lib/error-message";
import { formatDateTime } from "../../lib/time";
import {
  getFreePoolStatus,
  patchFreePoolConfig,
  stopFreePoolRun,
  triggerFreePoolRun,
} from "./api";
import type {
  FreePoolConfig,
  FreePoolRelayMode,
  FreePoolSource,
  FreePoolSourceKind,
  FreePoolVerifiedNode,
} from "./types";

function phaseLabel(phase: string, t: (key: string) => string): string {
  switch (phase) {
    case "fetching":
      return t("抓取源中");
    case "probing":
      return t("筛选探测中");
    case "syncing":
      return t("同步订阅中");
    case "error":
      return t("上次失败");
    default:
      return t("空闲");
  }
}

function phaseBadgeVariant(
  running: boolean,
  phase: string,
): "info" | "danger" | "success" | "neutral" {
  if (running) return "info";
  if (phase === "error") return "danger";
  return "success";
}

export function FreePoolPage() {
  const { t } = useI18n();
  const { toasts, showToast, dismissToast } = useToast();
  const [draftConfig, setDraftConfig] = useState<FreePoolConfig | null>(null);
  const [nodeFilter, setNodeFilter] = useState("");

  const statusQuery = useQuery({
    queryKey: ["free-pool-status"],
    queryFn: getFreePoolStatus,
    refetchInterval: (query) => (query.state.data?.running ? 2000 : 15000),
  });

  const draft = draftConfig ?? statusQuery.data?.config ?? null;
  const dirty = draftConfig !== null;

  const saveMutation = useMutation({
    mutationFn: async () => {
      if (!draft) throw new Error(t("配置尚未加载"));
      return patchFreePoolConfig(draft);
    },
    onSuccess: () => {
      setDraftConfig(null);
      void statusQuery.refetch();
      showToast("success", t("免费节点池配置已保存"));
    },
    onError: (error) => {
      showToast("error", formatApiErrorMessage(error, t));
    },
  });

  const runMutation = useMutation({
    mutationFn: async () => {
      if (dirty && draft) {
        await patchFreePoolConfig(draft);
        setDraftConfig(null);
      }
      return triggerFreePoolRun();
    },
    onSuccess: () => {
      void statusQuery.refetch();
      showToast("success", t("已开始采集并筛选免费节点"));
    },
    onError: (error) => {
      showToast("error", formatApiErrorMessage(error, t));
    },
  });

  const stopMutation = useMutation({
    mutationFn: stopFreePoolRun,
    onSuccess: () => {
      void statusQuery.refetch();
      showToast("success", t("已请求停止当前筛选任务"));
    },
    onError: (error) => {
      showToast("error", formatApiErrorMessage(error, t));
    },
  });

  const updateDraft = (updater: (prev: FreePoolConfig) => FreePoolConfig) => {
    setDraftConfig((prev) => {
      const base = prev ?? (statusQuery.data ? structuredClone(statusQuery.data.config) : null);
      if (!base) return null;
      return updater(base);
    });
  };

  const resetDraft = () => {
    setDraftConfig(null);
  };

  const addSource = () => {
    updateDraft((prev) => ({
      ...prev,
      sources: [
        ...prev.sources,
        {
          id: `custom-${Date.now()}`,
          name: "",
          url: "https://",
          kind: "auto",
          enabled: true,
        },
      ],
    }));
  };

  const updateSource = (idx: number, patch: Partial<FreePoolSource>) => {
    updateDraft((prev) => ({
      ...prev,
      sources: prev.sources.map((src, i) => (i === idx ? { ...src, ...patch } : src)),
    }));
  };

  const removeSource = (idx: number) => {
    updateDraft((prev) => ({
      ...prev,
      sources: prev.sources.filter((_, i) => i !== idx),
    }));
  };

  const status = statusQuery.data;
  const stats = status?.stats;
  const running = Boolean(status?.running);
  const relayCount = status?.relay_node_count ?? 0;

  const sourceStatById = useMemo(() => {
    const map = new Map<string, { sanitized: number; error?: string }>();
    for (const s of stats?.sources ?? []) {
      map.set(s.id, { sanitized: s.sanitized, error: s.error });
    }
    return map;
  }, [stats?.sources]);

  const filteredNodes = useMemo(() => {
    const list = status?.nodes ?? [];
    const q = nodeFilter.trim().toLowerCase();
    if (!q) return list;
    return list.filter(
      (n) =>
        n.protocol.toLowerCase().includes(q) ||
        n.tag.toLowerCase().includes(q) ||
        n.server.toLowerCase().includes(q) ||
        n.source_name.toLowerCase().includes(q),
    );
  }, [status?.nodes, nodeFilter]);

  const columns = useMemo<ColumnDef<FreePoolVerifiedNode>[]>(
    () => [
      {
        accessorKey: "protocol",
        header: () => t("协议"),
        cell: ({ row }) => <Badge variant="neutral">{row.original.protocol}</Badge>,
      },
      {
        accessorKey: "tag",
        header: () => t("标签"),
        cell: ({ row }) => <span className="freepool-cell-tag">{row.original.tag}</span>,
      },
      {
        id: "server",
        header: () => t("服务器"),
        cell: ({ row }) => (
          <code className="freepool-mono">
            {row.original.server}:{row.original.server_port}
          </code>
        ),
      },
      {
        accessorKey: "source_name",
        header: () => t("来源"),
        cell: ({ row }) => <span>{row.original.source_name}</span>,
      },
      {
        accessorKey: "latency_ms",
        header: () => t("HTTPS 延迟"),
        cell: ({ row }) => <code className="freepool-mono">{row.original.latency_ms} ms</code>,
      },
      {
        accessorKey: "ssh22_ok",
        header: () => "SSH 22",
        cell: ({ row }) =>
          row.original.ssh22_ok ? (
            <Badge variant="success">
              <CheckCircle2 size={12} style={{ marginRight: 4 }} />
              {row.original.ssh22_ms ? `${row.original.ssh22_ms}ms` : "OK"}
            </Badge>
          ) : (
            <Badge variant="neutral">-</Badge>
          ),
      },
    ],
    [t],
  );

  const progressPct =
    stats && stats.candidates > 0
      ? Math.min(100, Math.round((stats.probed / stats.candidates) * 100))
      : 0;

  const activeFrontSummary = useMemo(() => {
    if (!draft) return "-";
    if (draft.sync_chain_proxy) {
      return `chain_relay (${relayCount})`;
    }
    if (draft.sync_detour_tag.trim()) {
      return draft.sync_detour_tag.trim();
    }
    return stats?.used_relay_desc || t("直连");
  }, [draft, relayCount, stats?.used_relay_desc, t]);

  const showMissingRelayWarning = Boolean(status && draft?.sync_chain_proxy && relayCount === 0);

  return (
    <section className="syscfg-page freepool-page">
      <header className="module-header">
        <div>
          <h2>{t("免费节点池")}</h2>
          <p className="module-description">
            {t("自动采集公开免费 SOCKS5 与代理节点，经现有机场前置完成严格 TLS 反劫持与 22 端口 SSH 筛选后，同步为链式落地订阅。")}
          </p>
        </div>
        <div className="freepool-header-actions">
          <Button
            variant="secondary"
            size="sm"
            onClick={() => void statusQuery.refetch()}
            disabled={statusQuery.isFetching}
          >
            <RefreshCw size={15} className={statusQuery.isFetching ? "spin" : undefined} />
            {t("刷新")}
          </Button>
          {running ? (
            <Button
              variant="danger"
              size="sm"
              onClick={() => void stopMutation.mutateAsync()}
              disabled={stopMutation.isPending}
            >
              <Square size={14} />
              {t("停止任务")}
            </Button>
          ) : (
            <Button
              variant="primary"
              size="sm"
              onClick={() => void runMutation.mutateAsync()}
              disabled={runMutation.isPending || !draft}
            >
              <Play size={14} />
              {runMutation.isPending ? t("启动中...") : t("立即采集筛选")}
            </Button>
          )}
        </div>
      </header>

      <ToastContainer toasts={toasts} onDismiss={dismissToast} />

      {statusQuery.isError ? (
        <div className="callout callout-error">
          <AlertTriangle size={16} />
          <span>{formatApiErrorMessage(statusQuery.error, t)}</span>
        </div>
      ) : null}

      {showMissingRelayWarning ? (
        <div className="callout callout-warning">
          <AlertTriangle size={16} />
          <span>
            {t("当前 Resin 还没有开启「作为线路节点 (chain_relay)」的健康机场订阅。请先在「订阅管理」中将机场前置订阅设为线路节点，免费落地池才会自动走「前置 → 免费节点」链路。")}
          </span>
        </div>
      ) : null}

      {status?.last_error ? (
        <div className="callout callout-error">
          <AlertTriangle size={16} />
          <span>
            {t("上次执行错误")}：{status.last_error}
          </span>
        </div>
      ) : null}

      {/* Top KPI Cards — matching Dashboard design language */}
      <div className="dashboard-kpi-grid">
        <Card className="dashboard-kpi-card">
          <div className="dashboard-kpi-icon waves">
            <Activity size={17} />
          </div>
          <div className="freepool-kpi-body">
            <p className="dashboard-kpi-label">{t("探测进度")}</p>
            <p className="dashboard-kpi-value">
              {stats?.probed ?? 0} / {stats?.candidates ?? 0}
            </p>
            <p className="dashboard-kpi-sub">
              {t("原始解析")} {stats?.raw_parsed ?? 0} · {t("安全清洗后")} {stats?.sanitized ?? 0}
            </p>
            {running ? (
              <div className="freepool-progress-track" aria-label="progress">
                <div className="freepool-progress-fill" style={{ width: `${progressPct}%` }} />
              </div>
            ) : null}
          </div>
          <Badge
            className="dashboard-kpi-badge"
            variant={phaseBadgeVariant(running, status?.phase || "idle")}
          >
            {phaseLabel(status?.phase || "idle", t)}
          </Badge>
        </Card>

        <Card className="dashboard-kpi-card">
          <div className="dashboard-kpi-icon shield">
            <ShieldCheck size={17} />
          </div>
          <div className="freepool-kpi-body">
            <p className="dashboard-kpi-label">{t("HTTPS 严校通过")}</p>
            <p className="dashboard-kpi-value">{stats?.https_passed ?? 0}</p>
            <p className="dashboard-kpi-sub">
              {draft?.enforce_strict_tls ? t("严格 TLS 证书与反劫持校验") : t("按源配置校验")}
            </p>
          </div>
          <Badge
            className="dashboard-kpi-badge"
            variant={draft?.enforce_strict_tls ? "success" : "neutral"}
          >
            {draft?.enforce_strict_tls ? "Anti-MITM" : "TLS"}
          </Badge>
        </Card>

        <Card className="dashboard-kpi-card">
          <div className="dashboard-kpi-icon lease">
            <KeyRound size={17} />
          </div>
          <div className="freepool-kpi-body">
            <p className="dashboard-kpi-label">
              {running ? t("本轮 SSH 22 通过") : t("已同步可用节点")}
            </p>
            <p className="dashboard-kpi-value">
              {running ? stats?.ssh22_passed ?? 0 : status?.nodes.length ?? 0}
            </p>
            <p className="dashboard-kpi-sub">
              {running
                ? `${t("当前池内")} ${status?.nodes.length ?? 0}`
                : `${t("SSH 22 端口通过")} ${stats?.ssh22_passed ?? 0}`}
            </p>
          </div>
          <Badge
            className="dashboard-kpi-badge"
            variant={draft?.require_ssh_port22 ? "info" : "neutral"}
          >
            {draft?.require_ssh_port22 ? "SSH 22" : "HTTPS"}
          </Badge>
        </Card>

        <Card className="dashboard-kpi-card">
          <div className="dashboard-kpi-icon gauge">
            <Gauge size={17} />
          </div>
          <div className="freepool-kpi-body">
            <p className="dashboard-kpi-label">{t("前置链路")}</p>
            <p className="dashboard-kpi-value freepool-kpi-value-sm" title={activeFrontSummary}>
              {activeFrontSummary}
            </p>
            <p className="dashboard-kpi-sub">
              {status?.last_success_at
                ? `${t("上次成功")} ${formatDateTime(status.last_success_at)}`
                : t("尚未完成过同步")}
            </p>
          </div>
          <Badge
            className="dashboard-kpi-badge"
            variant={draft?.enabled ? "success" : "neutral"}
          >
            {draft?.enabled ? `${t("定时")} ${draft.interval}` : t("手动")}
          </Badge>
        </Card>
      </div>

      {/* Main Configuration Card — matching SystemConfigPage sections & 2-col form-grid */}
      {draft ? (
        <Card className="syscfg-form-card platform-directory-card">
          <div className="detail-header">
            <div>
              <h3>{t("维护与筛选配置")}</h3>
              <p>
                {t("默认开启严格 TLS 防 MITM 与 22 端口 SSH 连通性筛选。")}
                {status?.next_run_at
                  ? ` · ${t("下次计划")}：${formatDateTime(status.next_run_at)}`
                  : ""}
              </p>
            </div>
            <div className="freepool-header-actions">
              <Button
                variant="ghost"
                size="sm"
                onClick={resetDraft}
                disabled={!dirty || saveMutation.isPending}
              >
                <RotateCcw size={14} />
                {t("重置草稿")}
              </Button>
              <Button
                variant="primary"
                size="sm"
                onClick={() => void saveMutation.mutateAsync()}
                disabled={!dirty || saveMutation.isPending}
              >
                <Save size={14} />
                {saveMutation.isPending ? t("保存中...") : t("保存配置")}
              </Button>
            </div>
          </div>

          <section className="syscfg-section">
            <h4>{t("调度与安全校验")}</h4>
            <div className="subscription-switch-group freepool-switch-grid">
              <div className="subscription-switch-item">
                <label className="subscription-switch-label" htmlFor="fp-enabled">
                  {t("启用定时维护")}
                </label>
                <Switch
                  id="fp-enabled"
                  checked={draft.enabled}
                  onChange={(e) => updateDraft((p) => ({ ...p, enabled: e.target.checked }))}
                />
              </div>

              <div className="subscription-switch-item">
                <label className="subscription-switch-label" htmlFor="fp-ssh22">
                  {t("默认开启 SSH 22 端口筛选")}
                </label>
                <Switch
                  id="fp-ssh22"
                  checked={draft.require_ssh_port22}
                  onChange={(e) =>
                    updateDraft((p) => ({ ...p, require_ssh_port22: e.target.checked }))
                  }
                />
              </div>

              <div className="subscription-switch-item">
                <label className="subscription-switch-label" htmlFor="fp-strict-tls">
                  {t("严格 TLS 校验证书（防 MITM）")}
                </label>
                <Switch
                  id="fp-strict-tls"
                  checked={draft.enforce_strict_tls}
                  onChange={(e) =>
                    updateDraft((p) => ({ ...p, enforce_strict_tls: e.target.checked }))
                  }
                />
              </div>
            </div>

            <div className="form-grid">
              <div className="field-group">
                <label className="field-label" htmlFor="fp-interval">
                  {t("维护间隔")}
                </label>
                <Input
                  id="fp-interval"
                  value={draft.interval}
                  placeholder="30m"
                  onChange={(e) => updateDraft((p) => ({ ...p, interval: e.target.value }))}
                />
              </div>

              <div className="field-group">
                <label className="field-label" htmlFor="fp-ssh-target">
                  {t("SSH 探测目标")}
                </label>
                <Input
                  id="fp-ssh-target"
                  value={draft.ssh_check_target}
                  placeholder="github.com:22"
                  disabled={!draft.require_ssh_port22}
                  onChange={(e) => updateDraft((p) => ({ ...p, ssh_check_target: e.target.value }))}
                />
              </div>

              <div className="field-group field-span-2">
                <label className="field-label" htmlFor="fp-https-url">
                  {t("HTTPS 测活地址")}
                </label>
                <Input
                  id="fp-https-url"
                  value={draft.https_check_url}
                  onChange={(e) => updateDraft((p) => ({ ...p, https_check_url: e.target.value }))}
                />
              </div>
            </div>
          </section>

          <section className="syscfg-section">
            <h4>{t("探测性能与门槛")}</h4>
            <div className="form-grid">
              <div className="field-group">
                <label className="field-label" htmlFor="fp-max-candidates">
                  {t("单轮最多探测候选")}
                </label>
                <Input
                  id="fp-max-candidates"
                  type="number"
                  min={1}
                  max={10000}
                  value={draft.max_candidates}
                  onChange={(e) =>
                    updateDraft((p) => ({ ...p, max_candidates: Number(e.target.value) || 1 }))
                  }
                />
              </div>

              <div className="field-group">
                <label className="field-label" htmlFor="fp-max-keep">
                  {t("最多保留节点数")}
                </label>
                <Input
                  id="fp-max-keep"
                  type="number"
                  min={1}
                  max={2000}
                  value={draft.max_keep_nodes}
                  onChange={(e) =>
                    updateDraft((p) => ({ ...p, max_keep_nodes: Number(e.target.value) || 1 }))
                  }
                />
              </div>

              <div className="field-group">
                <label className="field-label" htmlFor="fp-concurrency">
                  {t("探测并发数")}
                </label>
                <Input
                  id="fp-concurrency"
                  type="number"
                  min={1}
                  max={256}
                  value={draft.concurrency}
                  onChange={(e) =>
                    updateDraft((p) => ({ ...p, concurrency: Number(e.target.value) || 1 }))
                  }
                />
              </div>

              <div className="field-group">
                <label className="field-label" htmlFor="fp-probe-timeout">
                  {t("单节点超时")}
                </label>
                <Input
                  id="fp-probe-timeout"
                  value={draft.probe_timeout}
                  placeholder="6s"
                  onChange={(e) => updateDraft((p) => ({ ...p, probe_timeout: e.target.value }))}
                />
              </div>

              <div className="field-group">
                <label className="field-label" htmlFor="fp-max-latency">
                  {t("最大允许延迟 (ms)")}
                </label>
                <Input
                  id="fp-max-latency"
                  type="number"
                  min={100}
                  max={60000}
                  value={draft.max_latency_ms}
                  onChange={(e) =>
                    updateDraft((p) => ({ ...p, max_latency_ms: Number(e.target.value) || 100 }))
                  }
                />
              </div>
            </div>
          </section>

          <section className="syscfg-section">
            <h4>{t("前置链路与订阅同步")}</h4>
            <div className="subscription-switch-group">
              <div className="subscription-switch-item">
                <label className="subscription-switch-label" htmlFor="fp-sync-chain">
                  {t("同步后开启链式落地 (chain_proxy)")}
                </label>
                <Switch
                  id="fp-sync-chain"
                  checked={draft.sync_chain_proxy}
                  onChange={(e) =>
                    updateDraft((p) => ({ ...p, sync_chain_proxy: e.target.checked }))
                  }
                />
              </div>

              <div className="subscription-switch-item">
                <label className="subscription-switch-label" htmlFor="fp-ensure-platform">
                  {t("自动创建独立平台")}
                </label>
                <Switch
                  id="fp-ensure-platform"
                  checked={draft.ensure_platform}
                  onChange={(e) =>
                    updateDraft((p) => ({ ...p, ensure_platform: e.target.checked }))
                  }
                />
              </div>
            </div>

            <div className="form-grid">
              <div className="field-group">
                <label className="field-label" htmlFor="fp-relay-mode">
                  {t("探测前置模式")}
                </label>
                <Select
                  id="fp-relay-mode"
                  value={draft.relay_mode}
                  onChange={(e) =>
                    updateDraft((p) => ({ ...p, relay_mode: e.target.value as FreePoolRelayMode }))
                  }
                >
                  <option value="auto">{t("自动（优先线路节点，其次平台）")}</option>
                  <option value="relay_only">{t("仅使用 chain_relay 线路节点")}</option>
                  <option value="platform">{t("使用指定平台节点")}</option>
                  <option value="direct">{t("直连探测（不走前置）")}</option>
                </Select>
              </div>

              <div className="field-group">
                <label className="field-label" htmlFor="fp-relay-platform">
                  {t("前置平台名（可选）")}
                </label>
                <Input
                  id="fp-relay-platform"
                  value={draft.relay_platform}
                  placeholder="Scan / Default"
                  onChange={(e) => updateDraft((p) => ({ ...p, relay_platform: e.target.value }))}
                />
              </div>

              <div className="field-group">
                <label className="field-label" htmlFor="fp-sub-name">
                  {t("同步订阅名称")}
                </label>
                <Input
                  id="fp-sub-name"
                  value={draft.target_subscription_name}
                  onChange={(e) =>
                    updateDraft((p) => ({ ...p, target_subscription_name: e.target.value }))
                  }
                />
              </div>

              <div className="field-group">
                <label className="field-label" htmlFor="fp-evict-delay">
                  {t("临时订阅自动驱逐延迟")}
                </label>
                <Input
                  id="fp-evict-delay"
                  value={draft.sync_evict_delay}
                  placeholder="2h"
                  onChange={(e) => updateDraft((p) => ({ ...p, sync_evict_delay: e.target.value }))}
                />
              </div>

              <div className="field-group">
                <label className="field-label" htmlFor="fp-platform-name">
                  {t("目标平台名称")}
                </label>
                <Input
                  id="fp-platform-name"
                  value={draft.target_platform_name}
                  disabled={!draft.ensure_platform}
                  placeholder="FreePool"
                  onChange={(e) =>
                    updateDraft((p) => ({ ...p, target_platform_name: e.target.value }))
                  }
                />
              </div>

              {!draft.sync_chain_proxy ? (
                <div className="field-group">
                  <label className="field-label" htmlFor="fp-detour-tag">
                    {t("固定前置 Tag (detour，可选)")}
                  </label>
                  <Input
                    id="fp-detour-tag"
                    value={draft.sync_detour_tag}
                    placeholder={t("例如：⚡ 前置故障转移")}
                    onChange={(e) =>
                      updateDraft((p) => ({ ...p, sync_detour_tag: e.target.value }))
                    }
                  />
                </div>
              ) : null}
            </div>
          </section>
        </Card>
      ) : null}

      {/* Sources Management Card */}
      {draft ? (
        <Card className="syscfg-form-card platform-directory-card">
          <div className="detail-header">
            <div>
              <h3>
                {t("免费源列表")} ({draft.sources.filter((s) => s.enabled).length}/{draft.sources.length})
              </h3>
              <p>{t("支持 SOCKS5 纯文本列表（IP:PORT）和 V2Ray / Clash / sing-box 免费订阅。")}</p>
            </div>
            <Button variant="secondary" size="sm" onClick={addSource}>
              <Plus size={14} />
              {t("添加源")}
            </Button>
          </div>

          <div className="freepool-source-list">
            {draft.sources.map((src, idx) => {
              const stat = sourceStatById.get(src.id);
              return (
                <div key={src.id || idx} className="freepool-source-row">
                  <Switch
                    checked={src.enabled}
                    onChange={(e) => updateSource(idx, { enabled: e.target.checked })}
                  />
                  <Input
                    value={src.name}
                    placeholder={t("名称")}
                    onChange={(e) => updateSource(idx, { name: e.target.value })}
                  />
                  <Select
                    value={src.kind}
                    onChange={(e) =>
                      updateSource(idx, { kind: e.target.value as FreePoolSourceKind })
                    }
                  >
                    <option value="socks5_list">{t("SOCKS5 列表")}</option>
                    <option value="subscription">{t("代理节点订阅")}</option>
                    <option value="auto">{t("自动识别")}</option>
                  </Select>
                  <Input
                    value={src.url}
                    placeholder="https://..."
                    onChange={(e) => updateSource(idx, { url: e.target.value })}
                  />
                  <div className="freepool-source-meta">
                    {stat?.error ? (
                      <Badge variant="danger" title={stat.error}>
                        {t("失败")}
                      </Badge>
                    ) : stat ? (
                      <Badge variant="neutral">{stat.sanitized}</Badge>
                    ) : null}
                    <Button
                      variant="ghost"
                      size="sm"
                      onClick={() => removeSource(idx)}
                      title={t("删除")}
                      aria-label={t("删除")}
                    >
                      <Trash2 size={15} />
                    </Button>
                  </div>
                </div>
              );
            })}
          </div>
        </Card>
      ) : null}

      {/* Verified Nodes Table Card — using native DataTable */}
      <Card className="platform-list-card platform-directory-card">
        <div className="list-card-header">
          <div>
            <h3>
              {t("已通过筛选的免费节点")} ({status?.nodes.length ?? 0})
            </h3>
            <p>{t("按实测 HTTPS 延迟升序排列；已自动写入目标本地订阅。")}</p>
          </div>
          <div className="freepool-table-search">
            <Search size={14} />
            <Input
              value={nodeFilter}
              placeholder={t("过滤协议 / 标签 / IP / 来源")}
              onChange={(e) => setNodeFilter(e.target.value)}
            />
          </div>
        </div>

        {filteredNodes.length > 0 ? (
          <DataTable
            data={filteredNodes}
            columns={columns}
            getRowId={(row) => row.hash}
            wrapClassName="freepool-table-wrap"
          />
        ) : (
          <div className="empty-box" style={{ marginTop: "12px" }}>
            <ShieldCheck size={16} />
            <p>
              {status?.nodes.length
                ? t("没有匹配当前过滤条件的节点")
                : t("暂无已筛选节点，点击右上角「立即采集筛选」开始。")}
            </p>
          </div>
        )}
      </Card>
    </section>
  );
}
