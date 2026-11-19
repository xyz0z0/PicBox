"use client";

import { useCallback, useEffect, useState } from "react";
import { ConfirmDialog } from "@/app/components/Dialogs";
import {
  colors,
  buttonStyle as btn,
  compactInputStyle as inputStyle,
  panelStyle,
  tableStyle,
  thStyle,
  tdStyle,
} from "@/app/components/ui";
import { writeClipboardText } from "@/lib/clipboardText";

type CodeUser = {
  id: number;
  email: string | null;
  username: string;
};

type ActivationCode = {
  id: string;
  maskedCode: string;
  note: string | null;
  createdAt: string;
  expiresAt: string | null;
  usedAt: string | null;
  revokedAt: string | null;
  createdBy: CodeUser;
  usedBy: CodeUser | null;
};

type GeneratedCode = {
  code: string;
  maskedCode: string;
  expiresAt: string | null;
};

type Status = "all" | "unused" | "used" | "revoked" | "expired";
const PAGE_SIZE = 20;

function displayName(user: CodeUser) {
  return user.email ?? user.username;
}

function formatTime(value: string | null) {
  if (!value) return "-";
  return new Date(value).toLocaleString("zh-CN", {
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
  });
}

function codeStatus(code: ActivationCode, now: Date): Exclude<Status, "all"> {
  if (code.revokedAt) return "revoked";
  if (code.usedAt) return "used";
  if (code.expiresAt && new Date(code.expiresAt) <= now) return "expired";
  return "unused";
}

const STATUS_LABEL: Record<Exclude<Status, "all">, string> = {
  unused: "可用",
  used: "已使用",
  revoked: "已撤销",
  expired: "已过期",
};

const STATUS_COLOR: Record<Exclude<Status, "all">, string> = {
  unused: colors.success,
  used: colors.muted,
  revoked: colors.danger,
  expired: "var(--ph-warning)",
};

export default function ActivationCodesTab() {
  const [codes, setCodes] = useState<ActivationCode[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [status, setStatus] = useState<Status>("all");
  const [queryInput, setQueryInput] = useState("");
  const [query, setQuery] = useState("");
  const [page, setPage] = useState(1);
  const [total, setTotal] = useState(0);
  const [totalPages, setTotalPages] = useState(1);
  const [notice, setNotice] = useState<{
    type: "success" | "warn" | "error";
    text: string;
  } | null>(null);

  const [count, setCount] = useState("1");
  const [expiresInDays, setExpiresInDays] = useState("7");
  const [note, setNote] = useState("");
  const [generating, setGenerating] = useState(false);
  const [generateError, setGenerateError] = useState<string | null>(null);
  const [generated, setGenerated] = useState<GeneratedCode[] | null>(null);
  const [copiedAll, setCopiedAll] = useState(false);

  const [revokeTarget, setRevokeTarget] = useState<ActivationCode | null>(null);
  const [revokeBusy, setRevokeBusy] = useState(false);
  const [revokeError, setRevokeError] = useState<string | null>(null);

  const fetchCodes = useCallback(
    async (targetPage: number, targetStatus: Status, search: string) => {
      const params = new URLSearchParams({
        page: String(targetPage),
        limit: String(PAGE_SIZE),
        status: targetStatus,
      });
      if (search) params.set("q", search);
      const res = await fetch(
        `/api/admin/activation-codes?${params.toString()}`
      );
      const data = await res.json().catch(() => ({}));
      if (!res.ok) {
        throw new Error(data?.error?.message || "加载激活码失败");
      }
      return {
        codes: (data.codes ?? []) as ActivationCode[],
        total: Number(data.total) || 0,
        page: Number(data.page) || 1,
        totalPages: Number(data.totalPages) || 1,
      };
    },
    []
  );

  const load = useCallback(
    async (targetPage: number, targetStatus: Status, search: string) => {
      setLoading(true);
      setError(null);
      try {
        const data = await fetchCodes(targetPage, targetStatus, search);
        setCodes(data.codes);
        setTotal(data.total);
        setPage(data.page);
        setTotalPages(data.totalPages);
      } catch (e: any) {
        setError(e?.message || "加载激活码失败");
      } finally {
        setLoading(false);
      }
    },
    [fetchCodes]
  );

  useEffect(() => {
    void load(page, status, query);
  }, [load, page, status, query]);

  useEffect(() => {
    const next = queryInput.trim();
    const timer = window.setTimeout(() => {
      setQuery(next);
      setPage(1);
    }, 300);
    return () => window.clearTimeout(timer);
  }, [queryInput]);

  async function generateCodes() {
    setGenerating(true);
    setGenerateError(null);
    setNotice(null);
    try {
      const res = await fetch("/api/admin/activation-codes", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          count: Number(count),
          expiresInDays: Number(expiresInDays),
          note,
        }),
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok) {
        setGenerateError(data?.error?.message || "生成激活码失败");
        return;
      }
      setGenerated((data.codes ?? []) as GeneratedCode[]);
      setCopiedAll(false);
      setNote("");
      setPage(1);
      await load(1, status, query);
    } catch {
      setGenerateError("网络异常，生成激活码失败");
    } finally {
      setGenerating(false);
    }
  }

  async function copyAll() {
    if (!generated?.length) return;
    const copied = await writeClipboardText(
      generated.map((entry) => entry.code).join("\n")
    );
    if (!copied) {
      setNotice({ type: "error", text: "复制失败，请手动选中激活码复制" });
      return;
    }
    setCopiedAll(true);
  }

  async function revokeCode() {
    if (!revokeTarget) return;
    setRevokeBusy(true);
    setRevokeError(null);
    try {
      const res = await fetch(
        `/api/admin/activation-codes/${revokeTarget.id}`,
        {
          method: "PATCH",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({ operation: "revoke" }),
        }
      );
      const data = await res.json().catch(() => ({}));
      if (!res.ok) {
        setRevokeError(data?.error?.message || "撤销失败");
        return;
      }
      setRevokeTarget(null);
      setNotice({
        type: "success",
        text: data?.changed
          ? "激活码已撤销"
          : "该激活码已经是撤销状态",
      });
      await load(page, status, query);
    } catch {
      setRevokeError("网络异常，撤销失败");
    } finally {
      setRevokeBusy(false);
    }
  }

  const now = new Date();

  return (
    <div style={{ display: "grid", gap: 14 }}>
      <div style={{ ...panelStyle, overflowX: "visible" }}>
        <div style={{ display: "flex", gap: 10, flexWrap: "wrap", alignItems: "flex-end" }}>
          <label style={{ display: "grid", gap: 4, fontSize: 12, color: colors.muted }}>
            数量
            <input
              value={count}
              onChange={(event) => setCount(event.target.value)}
              inputMode="numeric"
              style={{ ...inputStyle, width: 90 }}
            />
          </label>
          <label style={{ display: "grid", gap: 4, fontSize: 12, color: colors.muted }}>
            有效期（天，0=永久）
            <input
              value={expiresInDays}
              onChange={(event) => setExpiresInDays(event.target.value)}
              inputMode="numeric"
              style={{ ...inputStyle, width: 140 }}
            />
          </label>
          <label style={{ display: "grid", gap: 4, fontSize: 12, color: colors.muted, flex: "1 1 220px" }}>
            备注（可选）
            <input
              value={note}
              onChange={(event) => setNote(event.target.value)}
              maxLength={200}
              placeholder="例如：给朋友的邀请码"
              style={inputStyle}
            />
          </label>
          <button
            type="button"
            style={btn("primary")}
            disabled={generating}
            onClick={() => void generateCodes()}
          >
            {generating ? "生成中…" : "生成激活码"}
          </button>
        </div>
        {generateError && (
          <p role="alert" style={{ margin: "10px 0 0", fontSize: 13, color: colors.danger }}>
            {generateError}
          </p>
        )}
      </div>

      {generated && generated.length > 0 && (
        <div
          style={{
            ...panelStyle,
            overflowX: "visible",
            borderColor: "var(--ph-warning-border)",
            background: "var(--ph-warning-soft)",
          }}
        >
          <div style={{ display: "flex", justifyContent: "space-between", gap: 10, flexWrap: "wrap" }}>
            <strong style={{ color: "var(--ph-warning-text)", fontSize: 13 }}>
              ⚠️ 完整激活码只显示这一次，关闭后无法再次查看
            </strong>
            <div style={{ display: "flex", gap: 8 }}>
              <button type="button" style={btn("outline")} onClick={() => void copyAll()}>
                {copiedAll ? "已复制全部" : "复制全部"}
              </button>
              <button
                type="button"
                style={btn("outline")}
                onClick={() => setGenerated(null)}
              >
                关闭
              </button>
            </div>
          </div>
          <div
            style={{
              marginTop: 10,
              display: "grid",
              gap: 6,
              maxHeight: 240,
              overflowY: "auto",
              fontFamily: "monospace",
              fontSize: 13,
            }}
          >
            {generated.map((entry) => (
              <div key={entry.code} style={{ display: "flex", justifyContent: "space-between", gap: 10 }}>
                <span>{entry.code}</span>
                <span style={{ color: colors.muted }}>
                  {entry.expiresAt ? `至 ${formatTime(entry.expiresAt)}` : "永久"}
                </span>
              </div>
            ))}
          </div>
        </div>
      )}

      <div style={{ ...panelStyle }}>
        <div style={{ display: "flex", gap: 8, marginBottom: 10, flexWrap: "wrap" }}>
          <input
            value={queryInput}
            onChange={(event) => setQueryInput(event.target.value)}
            placeholder="搜索掩码或备注"
            aria-label="搜索激活码"
            style={{ ...inputStyle, flex: "1 1 220px", maxWidth: 320 }}
          />
          <select
            value={status}
            onChange={(event) => {
              setStatus(event.target.value as Status);
              setPage(1);
            }}
            aria-label="激活码状态筛选"
            style={inputStyle}
          >
            <option value="all">全部状态</option>
            <option value="unused">可用</option>
            <option value="used">已使用</option>
            <option value="revoked">已撤销</option>
            <option value="expired">已过期</option>
          </select>
        </div>

        {notice && (
          <p
            role={notice.type === "error" ? "alert" : "status"}
            style={{
              margin: "0 0 10px",
              fontSize: 13,
              color:
                notice.type === "error"
                  ? colors.danger
                  : notice.type === "warn"
                  ? "var(--ph-warning)"
                  : colors.success,
            }}
          >
            {notice.text}
          </p>
        )}
        {error && (
          <p role="alert" style={{ margin: "0 0 10px", fontSize: 13, color: colors.danger }}>
            {error}{" "}
            <button style={btn("outline")} onClick={() => void load(page, status, query)}>
              重试
            </button>
          </p>
        )}

        <p className="admin-table-hint">左右滑动可查看全部列</p>
        <table className="app-table" style={tableStyle(960)}>
          <thead>
            <tr>
              <th style={thStyle}>激活码</th>
              <th style={thStyle}>状态</th>
              <th style={thStyle}>备注</th>
              <th style={thStyle}>创建人</th>
              <th style={thStyle}>创建时间</th>
              <th style={thStyle}>过期时间</th>
              <th style={thStyle}>使用者</th>
              <th style={thStyle}>操作</th>
            </tr>
          </thead>
          <tbody>
            {codes.map((code) => {
              const state = codeStatus(code, now);
              return (
                <tr key={code.id}>
                  <td style={{ ...tdStyle, fontFamily: "monospace" }}>{code.maskedCode}</td>
                  <td style={{ ...tdStyle, color: STATUS_COLOR[state] }}>{STATUS_LABEL[state]}</td>
                  <td style={tdStyle}>{code.note || "-"}</td>
                  <td style={tdStyle}>{displayName(code.createdBy)}</td>
                  <td style={tdStyle}>{formatTime(code.createdAt)}</td>
                  <td style={tdStyle}>{code.expiresAt ? formatTime(code.expiresAt) : "永久"}</td>
                  <td style={tdStyle}>
                    {code.usedBy
                      ? `${displayName(code.usedBy)}（${formatTime(code.usedAt)}）`
                      : "-"}
                  </td>
                  <td style={tdStyle}>
                    {state === "unused" || state === "expired" ? (
                      <button
                        type="button"
                        style={{ ...btn("danger"), padding: "4px 8px", fontSize: 12 }}
                        onClick={() => {
                          setRevokeTarget(code);
                          setRevokeError(null);
                        }}
                      >
                        撤销
                      </button>
                    ) : (
                      <span style={{ color: colors.muted }}>-</span>
                    )}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>

        {!loading && codes.length === 0 && (
          <p style={{ color: colors.muted, padding: "16px 0", margin: 0 }}>
            暂无激活码
          </p>
        )}

        <div
          style={{
            display: "flex",
            alignItems: "center",
            justifyContent: "space-between",
            gap: 8,
            flexWrap: "wrap",
            marginTop: 12,
          }}
        >
          <span style={{ color: colors.muted, fontSize: 12 }}>
            共 {total} 个激活码 · 第 {page} / {totalPages} 页
            {loading ? " · 更新中…" : ""}
          </span>
          <div style={{ display: "flex", gap: 6 }}>
            <button
              style={btn("outline")}
              disabled={loading || page <= 1}
              onClick={() => setPage((current) => Math.max(1, current - 1))}
            >
              上一页
            </button>
            <button
              style={btn("outline")}
              disabled={loading || page >= totalPages}
              onClick={() => setPage((current) => Math.min(totalPages, current + 1))}
            >
              下一页
            </button>
          </div>
        </div>
      </div>

      {revokeTarget && (
        <ConfirmDialog
          title="撤销激活码"
          message={`确定撤销 ${revokeTarget.maskedCode}？撤销后不能再用于注册。`}
          confirmLabel="撤销"
          danger
          busy={revokeBusy}
          error={revokeError}
          onConfirm={() => void revokeCode()}
          onClose={() => {
            if (!revokeBusy) {
              setRevokeTarget(null);
              setRevokeError(null);
            }
          }}
        />
      )}
    </div>
  );
}
