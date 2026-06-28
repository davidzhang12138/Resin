import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { ArrowRight, Check, Search, X } from "lucide-react";
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

  const filtered = useMemo(() => {
    const all: NodeSummary[] = nodesQuery.data?.items ?? [];
    const kw = keyword.trim().toLowerCase();
    if (!kw) return all;
    return all.filter((n) => nodeDisplayTag(n).toLowerCase().includes(kw));
  }, [nodesQuery.data, keyword]);

  const selectedNode = filtered.find((n) => n.node_hash === selectedHash) ?? null;

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
    <div className="modal-overlay" role="dialog" aria-modal="true" onClick={onClose}>
      <Card className="modal-card reassign-dialog" onClick={(e) => e.stopPropagation()}>
        <div className="reassign-header">
          <div className="reassign-header-text">
            <h3>{t("重指租约节点")}</h3>
            <p className="reassign-account">{lease.account}</p>
          </div>
          <Button variant="ghost" size="sm" aria-label={t("关闭")} onClick={onClose}>
            <X size={16} />
          </Button>
        </div>

        <div className="reassign-current">
          <span className="reassign-current-label">{t("当前节点")}</span>
          <div className="reassign-current-node">
            <span className="reassign-current-tag">{lease.node_tag || lease.node_hash.slice(0, 8)}</span>
            <ArrowRight size={14} className="reassign-arrow" />
            <span className="reassign-current-tag reassign-current-tag-next">
              {selectedNode ? nodeDisplayTag(selectedNode) : t("选择新节点")}
            </span>
          </div>
          <div className="reassign-current-meta">
            <span>{t("出口 IP")}: {lease.egress_ip}</span>
            {selectedNode?.egress_ip ? (
              <>
                <ArrowRight size={12} className="reassign-arrow" />
                <span>{selectedNode.egress_ip}</span>
              </>
            ) : null}
          </div>
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
                    <span className="reassign-row-check">
                      {selected ? <Check size={14} /> : null}
                    </span>
                    <span className="reassign-row-tag" title={n.node_hash}>{nodeDisplayTag(n)}</span>
                    {n.region ? <Badge variant="neutral" className="reassign-row-region">{n.region}</Badge> : null}
                    <span className="reassign-row-ip">{n.egress_ip ?? "-"}</span>
                    <span className="reassign-row-latency">{formatLatency(n.reference_latency_ms)}</span>
                    <Badge variant={health.variant} className="reassign-row-health">{t(health.key)}</Badge>
                  </button>
                );
              })
            )}
          </div>
          <p className="reassign-list-count muted">
            {t("共 {{n}} 个可选节点", { n: filtered.length })}
          </p>
        </div>

        <div className="detail-actions reassign-actions">
          <Button variant="secondary" onClick={onClose} disabled={submitting}>
            {t("取消")}
          </Button>
          <Button onClick={confirm} disabled={!selectedHash || submitting}>
            {submitting ? t("提交中…") : t("确认重指")}
          </Button>
        </div>
      </Card>
    </div>
  );
}
