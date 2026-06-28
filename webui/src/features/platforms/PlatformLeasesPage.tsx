import { useMemo, useState, type CSSProperties } from "react";
import { Link, useParams } from "react-router-dom";
import { createColumnHelper } from "@tanstack/react-table";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { MoreHorizontal, RefreshCw } from "lucide-react";
import { Button } from "../../components/ui/Button";
import { Card } from "../../components/ui/Card";
import { DataTable } from "../../components/ui/DataTable";
import { Input } from "../../components/ui/Input";
import { OffsetPagination } from "../../components/ui/OffsetPagination";
import { ToastContainer } from "../../components/ui/Toast";
import { useToast } from "../../hooks/useToast";
import { useI18n } from "../../i18n";
import { formatRelativeTimeAt } from "../../lib/time";
import { deleteLease, listPlatformLeases } from "./api";
import type { LeaseResponse } from "./types";
import { ReassignLeaseDialog } from "./ReassignLeaseDialog";

const PAGE_SIZE = 20;
const PAGE_SIZE_OPTIONS = [10, 20, 50] as const;

const NODE_FILTER_ITEM_STYLE: CSSProperties = {
  flex: "1 1 120px",
  minWidth: "80px",
  display: "flex",
  flexDirection: "column",
  gap: "0.25rem",
};

// Module-level impure read so React Compiler's purity rule does not flag
// Date.now() inside render/callback bodies.
const nowMs = () => Date.now();

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
  const [pageSize, setPageSize] = useState(PAGE_SIZE);
  const [egressFilter, setEgressFilter] = useState("");
  const [now, setNow] = useState(nowMs);
  const [menuFor, setMenuFor] = useState<string | null>(null);
  const [reassignFor, setReassignFor] = useState<LeaseResponse | null>(null);
  const [selected, setSelected] = useState<Set<string>>(new Set());

  const queryKey = useMemo(
    () => ["platform-leases", platformId, accountKeyword, sortBy, sortOrder, page, pageSize] as const,
    [platformId, accountKeyword, sortBy, sortOrder, page, pageSize],
  );

  const leasesQuery = useQuery({
    queryKey,
    queryFn: () =>
      listPlatformLeases(platformId!, {
        account: accountKeyword || undefined,
        fuzzy: true,
        sort_by: sortBy,
        sort_order: sortOrder,
        limit: pageSize,
        offset: page * pageSize,
      }),
    enabled: Boolean(platformId),
    placeholderData: (prev) => prev,
  });

  const pageData = leasesQuery.data ?? { items: [], total: 0, limit: pageSize, offset: page * pageSize };
  const items = egressFilter.trim()
    ? pageData.items.filter((l) => l.egress_ip.includes(egressFilter.trim()))
    : pageData.items;
  const totalPages = Math.max(1, Math.ceil(pageData.total / pageSize));

  const refresh = async () => {
    setNow(nowMs());
    setSelected(new Set());
    await queryClient.invalidateQueries({ queryKey: ["platform-leases"] });
  };

  const removeLease = async (lease: LeaseResponse) => {
    if (!window.confirm(t("确认清除租约 {{account}}？", { account: lease.account }))) return;
    try {
      await deleteLease(platformId!, lease.account);
      showToast("success", t("租约 {{account}} 已清除", { account: lease.account }));
    } catch (err) {
      showToast("error", t("清除租约失败"));
      console.error(err);
    } finally {
      await refresh();
    }
  };

  const bulkClear = async () => {
    if (selected.size === 0) return;
    if (!window.confirm(t("确认清除选中的 {{n}} 条租约？", { n: selected.size }))) return;
    const accounts = [...selected];
    const results = await Promise.allSettled(accounts.map((a) => deleteLease(platformId!, a)));
    const fulfilled = results.filter((r) => r.status === "fulfilled").length;
    const failed = results.length - fulfilled;
    if (failed === 0) {
      showToast("success", t("已清除 {{n}} 条租约", { n: fulfilled }));
    } else if (fulfilled === 0) {
      showToast("error", t("批量清除失败"));
    } else {
      showToast("error", t("已清除 {{ok}} 条，失败 {{fail}} 条", { ok: fulfilled, fail: failed }));
    }
    if (failed) console.error("bulk clear partial failure", results);
    await refresh();
  };

  const col = createColumnHelper<LeaseResponse>();
  const columns = [
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
        return <span title={l.node_hash}>{l.node_tag || l.node_hash.slice(0, 8)}</span>;
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
      cell: (info) => <span title={info.getValue()}>{formatRelativeTimeAt(info.getValue(), new Date(now))}</span>,
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
    <section className="nodes-page">
      <header className="module-header">
        <div>
          <h2>{t("活跃租约")}</h2>
          <p className="module-description">
            <Link to={`/platforms/${platformId}`}>{t("返回平台详情")}</Link>
          </p>
        </div>
        <Button size="sm" variant="secondary" onClick={refresh} disabled={leasesQuery.isFetching}>
          <RefreshCw size={16} className={leasesQuery.isFetching ? "spin" : undefined} />
          {t("刷新")}
        </Button>
      </header>

      <Card className="filter-card platform-list-card platform-directory-card">
        <div className="list-card-header">
          <div>
            <h3>{t("租约列表")}</h3>
            <p>{t("共 {{total}} 条活跃租约", { total: pageData.total })}</p>
          </div>

          <div
            className="nodes-inline-filters"
            style={{
              display: "flex",
              flexWrap: "wrap",
              gap: "0.5rem",
              alignItems: "flex-end",
            }}
          >
            <div style={NODE_FILTER_ITEM_STYLE}>
              <label style={{ fontSize: "0.75rem", color: "var(--text-secondary)" }}>{t("账号")}</label>
              <Input
                value={accountKeyword}
                onChange={(e) => { setAccountKeyword(e.target.value); setPage(0); setSelected(new Set()); }}
                placeholder={t("模糊搜索")}
              />
            </div>

            <div style={NODE_FILTER_ITEM_STYLE}>
              <label style={{ fontSize: "0.75rem", color: "var(--text-secondary)" }}>{t("出口 IP")}</label>
              <Input
                value={egressFilter}
                onChange={(e) => setEgressFilter(e.target.value)}
                placeholder={t("本地过滤")}
              />
            </div>

            <div style={{ display: "flex", gap: "0.5rem", marginBottom: "0.125rem", marginLeft: "auto" }}>
              <Button size="sm" variant="secondary" disabled={selected.size === 0} onClick={bulkClear}>
                {t("批量清除")} ({selected.size})
              </Button>
            </div>
          </div>
        </div>
      </Card>

      {items.length ? (
        <Card className="nodes-table-card">
          <DataTable data={items} columns={columns} getRowId={(l) => l.account} />
        </Card>
      ) : (
        <Card><p className="platform-monitor-kpi-sub">{leasesQuery.isLoading ? t("加载中…") : t("租约列表为空")}</p></Card>
      )}

      <OffsetPagination
        page={page}
        totalPages={totalPages}
        totalItems={pageData.total}
        pageSize={pageSize}
        pageSizeOptions={PAGE_SIZE_OPTIONS}
        onPageChange={(p) => { setPage(p); setSelected(new Set()); }}
        onPageSizeChange={(s) => { setPageSize(s); setPage(0); setSelected(new Set()); }}
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
    </section>
  );
}
