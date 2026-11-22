import { useMutation, useQuery } from "@tanstack/react-query";
import {
  AlertTriangle,
  CheckCircle2,
  Play,
  Plus,
  RefreshCw,
  Save,
  ShieldCheck,
  Square,
  Trash2,
} from "lucide-react";
import { useEffect, useState } from "react";
import { Badge } from "../../components/ui/Badge";
import { Button } from "../../components/ui/Button";
import { Card } from "../../components/ui/Card";
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
  const [draft, setDraft] = useState<FreePoolConfig | null>(null);
  const [dirty, setDirty] = useState(false);

  const statusQuery = useQuery({
    queryKey: ["free-pool-status"],
    queryFn: getFreePoolStatus,
    refetchInterval: (query) => (query.state.data?.running ? 2000 : 15000),
  });

  useEffect(() => {
    if (statusQuery.data && !dirty) {
      setDraft(structuredClone(statusQuery.data.config));
    }
  }, [statusQuery.data, dirty]);

  const saveMutation = useMutation({
    mutationFn: async () => {
      if (!draft) throw new Error(t("配置尚未加载"));
      return patchFreePoolConfig(draft);
    },
    onSuccess: (updated) => {
      setDraft(structuredClone(updated.config));
      setDirty(false);
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
        const updated = await patchFreePoolConfig(draft);
        setDraft(structuredClone(updated.config));
        setDirty(false);
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
    setDraft((prev) => {
      if (!prev) return prev;
      return updater(prev);
    });
    setDirty(true);
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

  return (
    <section className="geoip-page" style={{ display: "flex", flexDirection: "column", gap: "1rem" }}>
      <header className="module-header">
        <div>
          <h2>{t("免费节点池")}</h2>
          <p className="module-description">
            {t("自动采集公开免费 SOCKS5 与代理节点，经现有机场前置完成严格 TLS 反劫持与 22 端口 SSH 筛选后，同步为链式落地订阅。")}
          </p>
        </div>
        <div style={{ display: "flex", gap: "8px", alignItems: "center" }}>
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

      {status && draft?.sync_chain_proxy && relayCount === 0 ? (
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

      {/* Status & Progress Summary */}
      <Card style={{ display: "flex", flexDirection: "column", gap: "1rem" }}>
        <div className="list-card-header">
          <div style={{ display: "flex", alignItems: "center", gap: "8px" }}>
            <ShieldCheck size={18} />
            <h3>{t("运行状态")}</h3>
            <Badge variant={phaseBadgeVariant(running, status?.phase || "idle")}>
              {phaseLabel(status?.phase || "idle", t)}
            </Badge>
            <Badge variant={relayCount > 0 ? "success" : "warning"}>
              {t("可用前置线路")}：{relayCount}
            </Badge>
          </div>
          <div style={{ fontSize: "0.85rem", opacity: 0.8 }}>
            {status?.last_success_at
              ? `${t("上次成功")}：${formatDateTime(status.last_success_at)}`
              : t("尚未完成过同步")}
            {status?.next_run_at ? ` · ${t("下次计划")}：${formatDateTime(status.next_run_at)}` : ""}
          </div>
        </div>

        <div
          style={{
            display: "grid",
            gridTemplateColumns: "repeat(auto-fit, minmax(130px, 1fr))",
            gap: "12px",
          }}
        >
          <div className="geoip-kv">
            <span>{t("原始解析")}</span>
            <p>{stats?.raw_parsed ?? 0}</p>
          </div>
          <div className="geoip-kv">
            <span>{t("安全清洗后")}</span>
            <p>{stats?.sanitized ?? 0}</p>
          </div>
          <div className="geoip-kv">
            <span>{t("探测进度")}</span>
            <p>
              {stats?.probed ?? 0} / {stats?.candidates ?? 0}
            </p>
          </div>
          <div className="geoip-kv">
            <span>{t("HTTPS 严校通过")}</span>
            <p>{stats?.https_passed ?? 0}</p>
          </div>
          <div className="geoip-kv">
            <span>{t("SSH 22 端口通过")}</span>
            <p>{stats?.ssh22_passed ?? 0}</p>
          </div>
          <div className="geoip-kv">
            <span>{t("已保留同步")}</span>
            <p>{stats?.kept ?? status?.nodes.length ?? 0}</p>
          </div>
          <div className="geoip-kv">
            <span>{t("探测前置")}</span>
            <p>{stats?.used_relay_desc || "-"}</p>
          </div>
        </div>
      </Card>

      {/* Configuration Card */}
      {draft ? (
        <Card style={{ display: "flex", flexDirection: "column", gap: "1rem" }}>
          <div className="list-card-header">
            <div>
              <h3>{t("维护与筛选配置")}</h3>
              <p>{t("默认开启严格 TLS 防 MITM 与 22 端口 SSH 连通性筛选。")}</p>
            </div>
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

          <div
            style={{
              display: "grid",
              gridTemplateColumns: "repeat(auto-fit, minmax(220px, 1fr))",
              gap: "14px",
            }}
          >
            <div className="field-group">
              <label>{t("启用定时维护")}</label>
              <div style={{ display: "flex", alignItems: "center", gap: "10px", paddingTop: "6px" }}>
                <Switch
                  checked={draft.enabled}
                  onChange={(e) => updateDraft((p) => ({ ...p, enabled: e.target.checked }))}
                />
                <span style={{ fontSize: "0.85rem" }}>
                  {draft.enabled ? t("已开启自动维护") : t("仅手动触发")}
                </span>
              </div>
            </div>

            <div className="field-group">
              <label>{t("维护间隔")}</label>
              <Input
                value={draft.interval}
                placeholder="30m"
                onChange={(e) => updateDraft((p) => ({ ...p, interval: e.target.value }))}
              />
            </div>

            <div className="field-group">
              <label>{t("默认开启 SSH 22 端口筛选")}</label>
              <div style={{ display: "flex", alignItems: "center", gap: "10px", paddingTop: "6px" }}>
                <Switch
                  checked={draft.require_ssh_port22}
                  onChange={(e) =>
                    updateDraft((p) => ({ ...p, require_ssh_port22: e.target.checked }))
                  }
                />
                <span style={{ fontSize: "0.85rem" }}>
                  {draft.require_ssh_port22 ? t("验证 SSH-2.0 Banner") : t("仅验证 HTTPS")}
                </span>
              </div>
            </div>

            <div className="field-group">
              <label>{t("SSH 探测目标")}</label>
              <Input
                value={draft.ssh_check_target}
                placeholder="github.com:22"
                disabled={!draft.require_ssh_port22}
                onChange={(e) => updateDraft((p) => ({ ...p, ssh_check_target: e.target.value }))}
              />
            </div>

            <div className="field-group">
              <label>{t("严格 TLS 校验证书（防 MITM）")}</label>
              <div style={{ display: "flex", alignItems: "center", gap: "10px", paddingTop: "6px" }}>
                <Switch
                  checked={draft.enforce_strict_tls}
                  onChange={(e) =>
                    updateDraft((p) => ({ ...p, enforce_strict_tls: e.target.checked }))
                  }
                />
                <span style={{ fontSize: "0.85rem" }}>
                  {draft.enforce_strict_tls ? t("强制关闭 skip-cert-verify") : t("按源配置")}
                </span>
              </div>
            </div>

            <div className="field-group">
              <label>{t("HTTPS 测活地址")}</label>
              <Input
                value={draft.https_check_url}
                onChange={(e) => updateDraft((p) => ({ ...p, https_check_url: e.target.value }))}
              />
            </div>

            <div className="field-group">
              <label>{t("探测前置模式")}</label>
              <Select
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
              <label>{t("前置平台名（可选）")}</label>
              <Input
                value={draft.relay_platform}
                placeholder="Scan / Default"
                onChange={(e) => updateDraft((p) => ({ ...p, relay_platform: e.target.value }))}
              />
            </div>

            <div className="field-group">
              <label>{t("单轮最多探测候选")}</label>
              <Input
                type="number"
                value={draft.max_candidates}
                onChange={(e) =>
                  updateDraft((p) => ({ ...p, max_candidates: Number(e.target.value) || 1 }))
                }
              />
            </div>

            <div className="field-group">
              <label>{t("最多保留节点数")}</label>
              <Input
                type="number"
                value={draft.max_keep_nodes}
                onChange={(e) =>
                  updateDraft((p) => ({ ...p, max_keep_nodes: Number(e.target.value) || 1 }))
                }
              />
            </div>

            <div className="field-group">
              <label>{t("探测并发数")}</label>
              <Input
                type="number"
                value={draft.concurrency}
                onChange={(e) =>
                  updateDraft((p) => ({ ...p, concurrency: Number(e.target.value) || 1 }))
                }
              />
            </div>

            <div className="field-group">
              <label>{t("最大允许延迟 (ms)")}</label>
              <Input
                type="number"
                value={draft.max_latency_ms}
                onChange={(e) =>
                  updateDraft((p) => ({ ...p, max_latency_ms: Number(e.target.value) || 100 }))
                }
              />
            </div>

            <div className="field-group">
              <label>{t("单节点超时")}</label>
              <Input
                value={draft.probe_timeout}
                placeholder="8s"
                onChange={(e) => updateDraft((p) => ({ ...p, probe_timeout: e.target.value }))}
              />
            </div>

            <div className="field-group">
              <label>{t("同步订阅名称")}</label>
              <Input
                value={draft.target_subscription_name}
                onChange={(e) =>
                  updateDraft((p) => ({ ...p, target_subscription_name: e.target.value }))
                }
              />
            </div>

            <div className="field-group">
              <label>{t("同步后开启链式落地 (chain_proxy)")}</label>
              <div style={{ display: "flex", alignItems: "center", gap: "10px", paddingTop: "6px" }}>
                <Switch
                  checked={draft.sync_chain_proxy}
                  onChange={(e) =>
                    updateDraft((p) => ({ ...p, sync_chain_proxy: e.target.checked }))
                  }
                />
                <span style={{ fontSize: "0.85rem" }}>
                  {draft.sync_chain_proxy ? t("走 Resin 线路跳板") : t("使用指定 detour 或直连")}
                </span>
              </div>
            </div>

            {!draft.sync_chain_proxy ? (
              <div className="field-group">
                <label>{t("固定前置 Tag (detour，可选)")}</label>
                <Input
                  value={draft.sync_detour_tag}
                  placeholder={t("例如：⚡ 前置故障转移")}
                  onChange={(e) => updateDraft((p) => ({ ...p, sync_detour_tag: e.target.value }))}
                />
              </div>
            ) : null}

            <div className="field-group">
              <label>{t("临时订阅自动驱逐延迟")}</label>
              <Input
                value={draft.sync_evict_delay}
                placeholder="2h"
                onChange={(e) => updateDraft((p) => ({ ...p, sync_evict_delay: e.target.value }))}
              />
            </div>

            <div className="field-group">
              <label>{t("自动创建独立平台")}</label>
              <div style={{ display: "flex", alignItems: "center", gap: "10px", paddingTop: "6px" }}>
                <Switch
                  checked={draft.ensure_platform}
                  onChange={(e) =>
                    updateDraft((p) => ({ ...p, ensure_platform: e.target.checked }))
                  }
                />
                <Input
                  value={draft.target_platform_name}
                  disabled={!draft.ensure_platform}
                  placeholder="FreePool"
                  onChange={(e) =>
                    updateDraft((p) => ({ ...p, target_platform_name: e.target.value }))
                  }
                />
              </div>
            </div>
          </div>
        </Card>
      ) : null}

      {/* Sources Management Card */}
      {draft ? (
        <Card style={{ display: "flex", flexDirection: "column", gap: "1rem" }}>
          <div className="list-card-header">
            <div>
              <h3>{t("免费源列表")}</h3>
              <p>{t("支持 SOCKS5 纯文本列表（IP:PORT）和 V2Ray / Clash / sing-box 免费订阅。")}</p>
            </div>
            <Button variant="secondary" size="sm" onClick={addSource}>
              <Plus size={14} />
              {t("添加源")}
            </Button>
          </div>

          <div style={{ display: "flex", flexDirection: "column", gap: "10px" }}>
            {draft.sources.map((src, idx) => (
              <div
                key={src.id || idx}
                style={{
                  display: "grid",
                  gridTemplateColumns: "auto 160px 150px 1fr auto",
                  gap: "8px",
                  alignItems: "center",
                }}
              >
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
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={() => removeSource(idx)}
                  title={t("删除")}
                >
                  <Trash2 size={15} />
                </Button>
              </div>
            ))}
          </div>
        </Card>
      ) : null}

      {/* Verified Nodes List */}
      <Card style={{ display: "flex", flexDirection: "column", gap: "1rem" }}>
        <div className="list-card-header">
          <div>
            <h3>
              {t("已通过筛选的免费节点")} ({status?.nodes.length ?? 0})
            </h3>
            <p>{t("按实测 HTTPS 延迟升序排列；已自动写入目标本地订阅。")}</p>
          </div>
        </div>

        {status && status.nodes.length > 0 ? (
          <div style={{ overflowX: "auto" }}>
            <table className="data-table" style={{ width: "100%", borderCollapse: "collapse" }}>
              <thead>
                <tr>
                  <th style={{ textAlign: "left", padding: "8px" }}>{t("协议")}</th>
                  <th style={{ textAlign: "left", padding: "8px" }}>{t("标签")}</th>
                  <th style={{ textAlign: "left", padding: "8px" }}>{t("服务器")}</th>
                  <th style={{ textAlign: "left", padding: "8px" }}>{t("来源")}</th>
                  <th style={{ textAlign: "right", padding: "8px" }}>{t("HTTPS 延迟")}</th>
                  <th style={{ textAlign: "center", padding: "8px" }}>SSH 22</th>
                </tr>
              </thead>
              <tbody>
                {status.nodes.map((n) => (
                  <tr key={n.hash} style={{ borderTop: "1px solid var(--border-subtle, #e5e7eb)" }}>
                    <td style={{ padding: "8px" }}>
                      <Badge variant="neutral">{n.protocol}</Badge>
                    </td>
                    <td style={{ padding: "8px", fontSize: "0.85rem" }}>{n.tag}</td>
                    <td style={{ padding: "8px", fontFamily: "monospace", fontSize: "0.85rem" }}>
                      {n.server}:{n.server_port}
                    </td>
                    <td style={{ padding: "8px", fontSize: "0.85rem" }}>{n.source_name}</td>
                    <td style={{ padding: "8px", textAlign: "right", fontFamily: "monospace" }}>
                      {n.latency_ms} ms
                    </td>
                    <td style={{ padding: "8px", textAlign: "center" }}>
                      {n.ssh22_ok ? (
                        <Badge variant="success">
                          <CheckCircle2 size={12} style={{ marginRight: 4 }} />
                          {n.ssh22_ms ? `${n.ssh22_ms}ms` : "OK"}
                        </Badge>
                      ) : (
                        <Badge variant="neutral">-</Badge>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : (
          <div className="empty-box">
            <ShieldCheck size={16} />
            <p>{t("暂无已筛选节点，点击右上角「立即采集筛选」开始。")}</p>
          </div>
        )}
      </Card>
    </section>
  );
}
