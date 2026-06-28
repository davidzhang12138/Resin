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

function nodeDisplayTag(n: NodeSummary): string {
  if (n.display_tag && n.display_tag.trim()) return n.display_tag;
  if (n.tags.length) return n.tags[0].tag;
  return n.node_hash.slice(0, 8);
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
  const nodes: NodeSummary[] = nodesQuery.data?.items ?? [];

  const filtered = useMemo(() => {
    const kw = keyword.trim().toLowerCase();
    if (!kw) return nodes;
    return nodes.filter((n) => nodeDisplayTag(n).toLowerCase().includes(kw));
  }, [nodes, keyword]);

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
    <div className="modal-overlay" role="dialog" aria-modal="true">
      <Card className="modal-card">
        <div className="modal-header">
          <h3>{t("重指租约节点 — {{account}}", { account: lease.account })}</h3>
          <Button variant="ghost" size="sm" onClick={onClose}>
            {t("关闭")}
          </Button>
        </div>
        <div className="form-grid">
          <p className="platform-monitor-kpi-sub">
            {t("当前节点")}: {lease.node_tag || lease.node_hash.slice(0, 8)} · {t("出口 IP")}: {lease.egress_ip}
          </p>
          <div>
            <label>{t("搜索节点")}</label>
            <Input
              value={keyword}
              onChange={(e) => setKeyword(e.target.value)}
              placeholder={t("按节点名过滤")}
            />
          </div>
          <div className="reassign-node-list">
            {filtered.length === 0 ? (
              <p className="platform-monitor-kpi-sub">{t("无可选节点")}</p>
            ) : (
              filtered.map((n) => (
                <label
                  key={n.node_hash}
                  className={`reassign-node-option${selectedHash === n.node_hash ? " selected" : ""}`}
                >
                  <input
                    type="radio"
                    name="reassign-target"
                    value={n.node_hash}
                    checked={selectedHash === n.node_hash}
                    onChange={() => setSelectedHash(n.node_hash)}
                  />
                  <span>{nodeDisplayTag(n)}</span>
                  <span className="platform-monitor-kpi-sub">{n.egress_ip ?? "-"}</span>
                </label>
              ))
            )}
          </div>
          <div className="detail-actions">
            <Button onClick={confirm} disabled={!selectedHash || submitting}>
              {t("确认重指")}
            </Button>
            <Button variant="secondary" onClick={onClose}>
              {t("取消")}
            </Button>
          </div>
        </div>
      </Card>
    </div>
  );
}
