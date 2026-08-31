import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { ArrowRight, Check, Gauge, MapPin, Search, Server, X } from "lucide-react";
import { Button } from "../../components/ui/Button";
import { Card } from "../../components/ui/Card";
import { Input } from "../../components/ui/Input";
import { Badge } from "../../components/ui/Badge";
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

const EMPTY_NODE_LIST: NodeSummary[] = [];

function nodeDisplayTag(n: NodeSummary): string {
  if (n.display_tag && n.display_tag.trim()) return n.display_tag;
  if (n.tags.length) return n.tags[0].tag;
  return n.node_hash.slice(0, 8);
}

function nodeHealth(n: NodeSummary): { variant: "success" | "warning" | "danger" | "muted"; key: string } {
  if (!n.has_outbound) return { variant: "muted", key: "无出口" };
  if (n.circuit_open_since) return { variant: "danger", key: "熔断" };
  if (n.failure_count > 0) return { variant: "warning", key: "不稳定" };
  return { variant: "success", key: "健康" };
}

function formatLatency(ms?: number): string {
  if (typeof ms !== "number" || !Number.isFinite(ms)) return "-";
  if (ms < 1) return "<1ms";
  if (ms < 1000) return `${Math.round(ms)}ms`;
  return `${(ms / 1000).toFixed(1)}s`;
}

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

  const allNodes = nodesQuery.data?.items ?? EMPTY_NODE_LIST;
  const filtered = useMemo(() => {
    const kw = keyword.trim().toLowerCase();
    if (!kw) return allNodes;
    return allNodes.filter((n) => nodeDisplayTag(n).toLowerCase().includes(kw));
  }, [allNodes, keyword]);

  const selectedNode = allNodes.find((n) => n.node_hash === selectedHash) ?? null;

  const confirm = async () => {
    if (!selectedHash || submitting) return;
    setSubmitting(true);
    try {
      const updated = await reassignLease(platformId, lease.account, { node_hash: selectedHash });
      showToast(
        "success",
        t("租约已重指到 {{tag}}", { tag: updated.node_tag || selectedHash.slice(0, 8) }),
      );
      onReassigned();
      onClose();
    } catch (err) {
      showToast("error", t("重指租约失败"));
      console.error(err);
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div
      className="modal-overlay reassign-overlay"
      role="dialog"
      aria-modal="true"
      aria-labelledby="reassign-dialog-title"
      aria-describedby="reassign-dialog-description"
      onClick={onClose}
    >
      <Card className="modal-card reassign-dialog" onClick={(e) => e.stopPropagation()}>
        <header className="reassign-header">
          <div className="reassign-header-text">
            <div className="reassign-kicker">
              <span className="reassign-title-mark" aria-hidden="true">
                <Server size={15} />
              </span>
              <span>{t("租约节点交接")}</span>
            </div>
            <h3 id="reassign-dialog-title">{t("重指租约节点")}</h3>
            <div className="reassign-account-line">
              <span className="reassign-account-label">{t("账号")}</span>
              <code className="reassign-account">{lease.account}</code>
            </div>
          </div>
          <Button className="reassign-close" variant="ghost" size="sm" aria-label={t("关闭")} onClick={onClose}>
            <X size={16} />
          </Button>
        </header>

        <div className="reassign-route" aria-label={t("租约节点交接")}>
          <div className="reassign-route-card reassign-route-card-current">
            <div className="reassign-route-label">
              <span className="reassign-route-step">01</span>
              <span>{t("当前绑定")}</span>
            </div>
            <strong className="reassign-route-node" title={lease.node_tag || lease.node_hash}>
              {lease.node_tag || lease.node_hash.slice(0, 8)}
            </strong>
            <span className="reassign-route-ip">
              <span>{t("出口 IP")}</span>
              <code>{lease.egress_ip || "—"}</code>
            </span>
          </div>

          <div className="reassign-route-transfer" aria-hidden="true">
            <span>{t("重指节点")}</span>
            <ArrowRight size={17} />
          </div>

          <div className={`reassign-route-card reassign-route-card-target${selectedNode ? " reassign-route-card-target-selected" : ""}`}>
            <div className="reassign-route-label">
              <span className="reassign-route-step">02</span>
              <span>{t("目标节点")}</span>
            </div>
            <strong className={`reassign-route-node${selectedNode ? "" : " reassign-route-placeholder"}`}>
              {selectedNode ? nodeDisplayTag(selectedNode) : t("选择新节点")}
            </strong>
            <span className="reassign-route-ip">
              <span>{t("出口 IP")}</span>
              <code>{selectedNode?.egress_ip || "—"}</code>
            </span>
          </div>
        </div>

        <section className="reassign-picker-section">
          <div className="reassign-section-head">
            <div>
              <div className="reassign-section-title">{t("目标节点")}</div>
              <p id="reassign-dialog-description">{t("选择一个有出口的可路由节点")}</p>
            </div>
            <span className="reassign-section-count">
              <strong>{filtered.length}</strong>
              <span>{t("可选节点")}</span>
            </span>
          </div>

          <div className="reassign-picker">
            <div className="reassign-search">
              <Search size={15} className="reassign-search-icon" />
              <Input
                value={keyword}
                onChange={(e) => setKeyword(e.target.value)}
                placeholder={t("按节点名过滤")}
                className="reassign-search-input"
                aria-label={t("搜索节点")}
              />
              {keyword ? (
                <button type="button" className="reassign-search-clear" aria-label={t("清除")} onClick={() => setKeyword("")}>
                  <X size={14} />
                </button>
              ) : null}
            </div>

            <div className="reassign-list" role="listbox" aria-label={t("可选节点")}>
              {nodesQuery.isLoading ? (
                <p className="muted reassign-list-hint">{t("加载中…")}</p>
              ) : nodesQuery.isError ? (
                <p className="muted reassign-list-hint">{t("节点列表加载失败")}</p>
              ) : filtered.length === 0 ? (
                <p className="muted reassign-list-hint">{t("无可选节点")}</p>
              ) : (
                filtered.map((n) => {
                  const health = nodeHealth(n);
                  const selected = selectedHash === n.node_hash;
                  return (
                    <button
                      type="button"
                      key={n.node_hash}
                      role="option"
                      aria-selected={selected}
                      className={`reassign-row${selected ? " reassign-row-selected" : ""}`}
                      onClick={() => setSelectedHash(n.node_hash)}
                    >
                      <span className="reassign-row-check" aria-hidden="true">
                        {selected ? <Check size={13} /> : null}
                      </span>
                      <span className="reassign-row-main">
                        <span className="reassign-row-tag" title={n.node_hash}>{nodeDisplayTag(n)}</span>
                        <span className="reassign-row-meta">
                          {n.region ? (
                            <span className="reassign-row-region">
                              <MapPin size={11} />
                              {n.region}
                            </span>
                          ) : null}
                          <span className="reassign-row-ip">{n.egress_ip ?? "-"}</span>
                          <span className="reassign-row-latency">
                            <Gauge size={11} />
                            {formatLatency(n.reference_latency_ms)}
                          </span>
                        </span>
                      </span>
                      <Badge variant={health.variant} className="reassign-row-health">
                        <span className={`reassign-health-dot reassign-health-dot-${health.variant}`} aria-hidden="true" />
                        {t(health.key)}
                      </Badge>
                    </button>
                  );
                })
              )}
            </div>
            <p className="reassign-list-count muted">
              {t("共 {{n}} 个可选节点", { n: filtered.length })}
            </p>
          </div>
        </section>

        <footer className="reassign-footer">
          <div className="reassign-footer-note">
            <span className="reassign-footer-note-mark" aria-hidden="true"><Check size={12} /></span>
            <span>{t("重指后会立即续期租约，并使用目标节点的出口 IP。")}</span>
          </div>
          <div className="detail-actions reassign-actions">
            <Button variant="secondary" onClick={onClose} disabled={submitting}>
              {t("取消")}
            </Button>
            <Button className="reassign-confirm" onClick={confirm} disabled={!selectedHash || submitting}>
              {submitting ? t("提交中…") : t("确认重指")}
            </Button>
          </div>
        </footer>
      </Card>
    </div>
  );
}
