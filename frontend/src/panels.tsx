import { useCallback, useEffect, useState } from "react";
import type { Agent, ChatMessage, Participant, Room, RoomSummary } from "./types";
import { Icon, OsGlyph, osLabel } from "./icons";
import { Markdown } from "./markdown";
import { Chip, ModeBadge, OsAvatar, StatusDot } from "./ui";
import {
  absoluteTime,
  agentTint,
  durationMsLabel,
  formatTime,
  hueFromID,
  normalizeOS,
  relativeAge,
  relativeTime,
} from "./lib";
import { isAdmin as isAdminMe, useAuth, useTheme } from "./auth";
import { splitPermissions, type PermissionReply } from "./messages";
import { joinAgentToRoom, listAgents, removeAgentFromRoom } from "./api";

/* ── Activity tab ──────────────────────────────────────────────────── */

interface TimelineItem {
  kind: "event" | "command" | "command_result" | "trace";
  icon: string;
  tone: "ok" | "warn" | "danger" | "live" | "sys";
  label: string;
  sub?: string;
  at?: string;
  who?: string;
  meta?: string;
}

function buildTimeline(messages: ChatMessage[]): TimelineItem[] {
  const items: TimelineItem[] = [];
  for (const m of messages) {
    if (m.type === "presence" || m.type === "system") {
      items.push({
        kind: "event",
        icon: "users",
        tone: "sys",
        label: m.sender_id || "system",
        sub: m.content,
        at: m.created_at,
      });
    } else if (m.type === "command") {
      items.push({
        kind: "command",
        icon: "terminal",
        tone: "live",
        label: `${m.sender_id} 发起命令`,
        sub: m.content?.split("\n")[0],
        at: m.created_at,
        who: m.sender_id,
      });
    } else if (m.type === "command_result") {
      const exitCode = m.metadata?.exit_code || "?";
      const ok = exitCode === "0";
      items.push({
        kind: "command_result",
        icon: "terminal",
        tone: ok ? "ok" : "danger",
        label: ok ? `命令完成 · exit ${exitCode}` : `命令失败 · exit ${exitCode}`,
        sub: m.content?.split("\n")[0],
        at: m.created_at,
        who: m.sender_id,
        meta: durationMsLabel(m.metadata?.duration_ms),
      });
    } else if (m.type === "trace" && m.metadata?.phase === "done") {
      items.push({
        kind: "trace",
        icon: "brain",
        tone: "sys",
        label: `${m.sender_id} 思考完成`,
        sub: m.content,
        at: m.created_at,
        who: m.sender_id,
      });
    }
  }
  return items.reverse();
}

export function ActivityPanel({ messages }: { messages: ChatMessage[] }) {
  const { theme } = useTheme();
  const dark = theme !== "paper";
  const items = buildTimeline(messages);
  const commands = messages.filter((m) => m.type === "command");
  const results = messages.filter((m) => m.type === "command_result");
  const completed = results.filter((m) => m.metadata?.exit_code === "0").length;
  const failed = results.length - completed;
  const stats = [
    { n: commands.length, l: "commands" },
    { n: completed, l: "completed" },
    { n: failed, l: "failed" },
    { n: messages.length, l: "messages" },
  ];
  return (
    <div className="panel-wrap">
      <div className="stat-row">
        {stats.map((s) => (
          <div className="stat" key={s.l}>
            <strong>{s.n}</strong>
            <span>{s.l}</span>
          </div>
        ))}
      </div>
      <div className="tl">
        {items.length === 0 && (
          <div className="panel-empty">还没有活动。发一条消息召唤 agent 试试。</div>
        )}
        {items.map((it, i) => {
          const tint = it.who ? agentTint(hueFromID(it.who), dark) : null;
          return (
            <div className="tl-item" key={i}>
              <span className={`tl-node tl-${it.tone}`}>
                <Icon name={it.icon} size={13} />
              </span>
              <div className="tl-body">
                <div className="tl-line">
                  <strong>{it.label}</strong>
                  {it.meta && <Chip tone="mono">{it.meta}</Chip>}
                  <time>{formatTime(it.at)}</time>
                </div>
                {it.sub && (
                  <span className="tl-sub" style={tint ? { color: tint.text } : undefined}>
                    {it.sub}
                  </span>
                )}
              </div>
            </div>
          );
        })}
      </div>
    </div>
  );
}

/* ── Approvals tab ─────────────────────────────────────────────────── */

// ApprovalsPanel 是房间审批的完整视图:顶部统计 + 「永久放行」规则列表(来自
// allow_always 点选,bridge 重启后其进程内规则会清空,此处是房间消息聚合的累
// 计视图)+ 按时间倒序的审批记录明细(agent/工具/命令/决议/审批人/时间)。
// 待审批的请求由右侧 PermissionToasts 负责操作,这里只读展示全貌。
export function ApprovalsPanel({ messages }: { messages: ChatMessage[] }) {
  const { pending, resolved, rules } = splitPermissions(messages);
  const approved = resolved.filter((r) => r.reply !== "deny").length;
  const denied = resolved.length - approved;
  const stats = [
    { n: pending.length, l: "待审批" },
    { n: approved, l: "已批准" },
    { n: denied, l: "已拒绝" },
    { n: rules.length, l: "永久规则" },
  ];

  type Row = {
    msg: ChatMessage;
    status: "pending" | PermissionReply;
    by?: string;
    at?: string;
    patterns?: string[];
  };
  const rows: Row[] = [
    ...pending.map((msg): Row => ({ msg, status: "pending" })),
    ...resolved.map(
      (r): Row => ({ msg: r.msg, status: r.reply, by: r.by, at: r.at, patterns: r.patterns }),
    ),
  ].sort((a, b) => (b.msg.created_at || "").localeCompare(a.msg.created_at || ""));

  const statusChip = (status: Row["status"]) => {
    switch (status) {
      case "pending":
        return (
          <Chip tone="warn">
            <Icon name="alert" size={11} /> 待审批
          </Chip>
        );
      case "allow_once":
        return (
          <Chip tone="ok">
            <Icon name="check" size={11} /> 批准一次
          </Chip>
        );
      case "allow_always":
        return (
          <Chip tone="ok">
            <Icon name="shieldCheck" size={11} /> 总是批准
          </Chip>
        );
      default:
        return (
          <Chip tone="danger">
            <Icon name="x" size={11} /> 已拒绝
          </Chip>
        );
    }
  };

  return (
    <div className="panel-wrap">
      <div className="stat-row">
        {stats.map((s) => (
          <div className="stat" key={s.l}>
            <strong>{s.n}</strong>
            <span>{s.l}</span>
          </div>
        ))}
      </div>

      {rules.length > 0 && (
        <section className="appr-rules">
          <div className="appr-rules-head">
            <Icon name="shieldCheck" size={13} /> 永久放行规则
            <span className="appr-rules-note">「总是批准」点选累计 · bridge 重启后进程内规则重新生效需再次批准</span>
          </div>
          {rules.map((rule) => (
            <div className="appr-rule" key={`${rule.agent}-${rule.tool}-${rule.pattern}`}>
              <Chip tone="mono">{rule.tool || "工具"}</Chip>
              <code className="appr-rule-pattern" title={rule.pattern}>
                {rule.pattern}
              </code>
              <span className="appr-rule-by" title={`agent ${rule.agent}`}>
                {rule.by || "?"} 批准
              </span>
            </div>
          ))}
        </section>
      )}

      <div className="appr-list">
        {rows.length === 0 && <div className="panel-empty">还没有审批记录。</div>}
        {rows.map((row) => {
          const msg = row.msg;
          const tool = msg.metadata?.tool || "";
          const input = (msg.metadata?.input || "").trim();
          const id = msg.metadata?.permission_id || "";
          return (
            <div className="appr-item" key={id || msg.id}>
              <div className="appr-line">
                <strong>{msg.sender_id || "agent"}</strong>
                {tool && <Chip tone="mono">{tool}</Chip>}
                {statusChip(row.status)}
                {row.by && <span className="appr-by">by {row.by}</span>}
                <span className="appr-flex" />
                {id && (
                  <span className="appr-hash" title={id}>
                    #{id.slice(-8)}
                  </span>
                )}
                <time>{formatTime(row.at || msg.created_at)}</time>
              </div>
              {input && <pre className="appr-pre">{input}</pre>}
              {row.status === "allow_always" && row.patterns && row.patterns.length > 0 && (
                <div className="appr-patterns">
                  放行模式:
                  {row.patterns.map((p) => (
                    <code key={p}>{p}</code>
                  ))}
                </div>
              )}
            </div>
          );
        })}
      </div>
    </div>
  );
}

/* ── Summary tab ───────────────────────────────────────────────────── */

interface SummaryPanelProps {
  summary: RoomSummary | null;
  loading: boolean;
  onRefresh: () => void;
  canManage?: boolean;
  onClear?: () => void;
}

// SummaryPanel surfaces the relay's rolling LLM digest of the room, plus when
// it was last regenerated. The summary is produced server-side every N
// conversational messages (or on a time interval); the UI is read-only.
export function SummaryPanel({ summary, loading, onRefresh, canManage = false, onClear }: SummaryPanelProps) {
  const text = summary?.summary?.trim() || "";
  const updatedAt = summary?.updated_at || null;
  const ago = relativeAge(updatedAt);
  const exact = absoluteTime(updatedAt);
  const covered = summary?.covered_seq ?? 0;

  return (
    <div className="panel-wrap summary-panel">
      <div className="summary-head">
        <div className="summary-head-title">
          <span className="summary-mark">
            <Icon name="sparkle" size={16} />
          </span>
          <div>
            <strong>房间摘要</strong>
            <span>LLM 滚动生成的长期上下文,供 agent 与成员快速回顾</span>
          </div>
        </div>
        <div className="summary-actions">
          {canManage && onClear && (
            <button
              type="button"
              className="icon-btn icon-btn-line"
              onClick={onClear}
              disabled={loading || !text}
              title="清空摘要"
              aria-label="清空摘要"
            >
              <Icon name="trash" size={15} />
            </button>
          )}
          <button
            type="button"
            className="icon-btn icon-btn-line"
            onClick={onRefresh}
            disabled={loading}
            title="刷新摘要"
            aria-label="刷新摘要"
          >
            <Icon name="refresh" size={15} className={loading ? "spin" : ""} />
          </button>
        </div>
      </div>

      {text ? (
        <>
          <div className="summary-meta">
            <span className="summary-meta-item">
              <Icon name="clock" size={13} />
              {ago ? (
                <>
                  上次更新 <strong>{ago}</strong>
                  {exact && <em title={exact}>· {exact}</em>}
                </>
              ) : (
                "更新时间未知"
              )}
            </span>
            <span className="summary-meta-item">
              <Icon name="hash" size={13} /> 已覆盖到消息 <strong>#{covered}</strong>
            </span>
          </div>
          <Markdown text={text} className="summary-body" />
        </>
      ) : (
        <div className="panel-empty">
          {loading
            ? "正在加载摘要…"
            : "还没有生成摘要。房间累计若干条对话后,relay 会自动生成并在这里展示(需在 relay 侧配置 LLM)。"}
        </div>
      )}
    </div>
  );
}

/* ── Agents tab ────────────────────────────────────────────────────── */

interface AgentsPanelProps {
  participants: Participant[];
  onCopyMention: (id: string) => void;
  roomID: string;
  currentLogin: string | null;
}

export function AgentsPanel({ participants, onCopyMention, roomID, currentLogin }: AgentsPanelProps) {
  const { theme } = useTheme();
  const dark = theme !== "paper";
  const agents = participants.filter((p) => p.kind === "agent");

  return (
    <div className="panel-wrap">
      {agents.length === 0 ? (
        <div className="panel-empty">暂无 Bridge 在线 — 在本机启动 Bridge 加入房间。</div>
      ) : (
        <div className="agent-grid">
          {agents.map((p) => {
            const hue = hueFromID(p.id);
            const tint = agentTint(hue, dark);
            const mode = p.metadata?.mode || "agent";
            const os = normalizeOS(p.metadata?.os);
            const isOwn = currentLogin != null && p.metadata?.owner_login === currentLogin;
            return (
              <AgentCard
                key={p.id}
                p={p}
                hue={hue}
                tint={tint}
                mode={mode}
                os={os}
                dark={dark}
                isOwn={isOwn}
                roomID={roomID}
                onCopyMention={onCopyMention}
              />
            );
          })}
        </div>
      )}
      <JoinMyAgentSection
        roomID={roomID}
        currentLogin={currentLogin}
        participants={participants}
      />
    </div>
  );
}

/* ── 单个在线 agent 卡片 ──────────────────────────────────────────── */

function AgentCard({
  p,
  hue,
  tint,
  mode,
  os,
  dark,
  isOwn,
  roomID,
  onCopyMention,
}: {
  p: Participant;
  hue: number;
  tint: { solid: string; text: string };
  mode: string;
  os: string | null;
  dark: boolean;
  isOwn: boolean;
  roomID: string;
  onCopyMention: (id: string) => void;
}) {
  const [confirming, setConfirming] = useState(false);
  const [removing, setRemoving] = useState(false);
  const [removeMsg, setRemoveMsg] = useState<string | null>(null);
  const provider = p.metadata?.provider || p.metadata?.role || "agent";
  const version = p.metadata?.version;
  const device = p.metadata?.device;
  const capabilities = p.metadata?.capabilities;

  const handleRemove = useCallback(() => {
    setRemoving(true);
    setRemoveMsg(null);
    removeAgentFromRoom(roomID, p.id)
      .then(() => {
        setRemoveMsg("已发出移除指令，等待 presence 离开事件…");
        setConfirming(false);
      })
      .catch((err: unknown) => {
        setRemoveMsg(err instanceof Error ? err.message : "移出失败，请稍后重试");
        setConfirming(false);
      })
      .finally(() => setRemoving(false));
  }, [roomID, p.id]);

  return (
    <div className="agent-card" style={{ ["--tint" as string]: tint.solid }}>
      <div className="agent-card-top">
        <div className="agent-card-identity">
          <OsAvatar
            person={{ id: p.id, label: p.label || p.id, kind: "agent", hue }}
            dark={dark}
            size={42}
            os={p.metadata?.os}
          />
          <div className="agent-card-id">
            <strong style={{ color: tint.text }} title={p.label || p.id}>
              {p.label || p.id}
            </strong>
            <span>{provider}</span>
          </div>
        </div>
        <ModeBadge mode={mode} full />
      </div>
      <div className="agent-meta">
        <span className="agent-meta-item agent-live">
          <StatusDot tone="live" pulse /> 在线 · {relativeTime(p.last_seen_at)}
        </span>
        {os && (
          <span className="agent-meta-item">
            <OsGlyph os={os} size={14} /> {osLabel(os)}
          </span>
        )}
        {device && (
          <span className="agent-meta-item" title={device}>
            <Icon name="cpu" size={13} /> {device}
          </span>
        )}
        {version && (
          <span className="agent-meta-item agent-ver" title={version}>
            <Icon name="branch" size={13} /> v{version}
          </span>
        )}
        {capabilities && (
          <span className="agent-cap" title={capabilities}>
            {capabilities}
          </span>
        )}
      </div>
      <div className="agent-card-foot">
        <button
          type="button"
          className="agent-mention"
          title={`复制 @${p.id}`}
          onClick={() => onCopyMention(p.id)}
        >
          <code>@{p.id}</code>
          <Icon name="copy" size={13} />
        </button>
        {isOwn && (
          <div className="agent-card-remove">
            {confirming ? (
              <span className="agent-remove-confirm">
                <button
                  type="button"
                  className="btn btn-sm btn-danger"
                  onClick={handleRemove}
                  disabled={removing}
                >
                  {removing ? "移出中…" : "确认移出"}
                </button>
                <button
                  type="button"
                  className="btn btn-sm"
                  onClick={() => setConfirming(false)}
                  disabled={removing}
                >
                  取消
                </button>
              </span>
            ) : (
              <button
                type="button"
                className="btn btn-sm"
                onClick={() => setConfirming(true)}
              >
                <Icon name="x" size={13} /> 移出本房间
              </button>
            )}
          </div>
        )}
      </div>
      {isOwn && removeMsg && (
        <div className="agent-card-message">
          <span className="agent-join-msg">{removeMsg}</span>
        </div>
      )}
      {!isOwn && (
        <div className="agent-card-view-only">
          <Icon name="lock" size={12} /> 仅 Agent 所有人可移出
        </div>
      )}
    </div>
  );
}

/* ── 加入我的 Agent 区块 ──────────────────────────────────────────── */

function JoinMyAgentSection({
  roomID,
  currentLogin,
  participants,
}: {
  roomID: string;
  currentLogin: string | null;
  participants: Participant[];
}) {
  const [myAgents, setMyAgents] = useState<Agent[]>([]);
  const [loaded, setLoaded] = useState(false);

  useEffect(() => {
    if (!currentLogin) return;
    let cancelled = false;
    const refresh = () => listAgents()
      .then((list) => {
        if (cancelled) return;
        setMyAgents(list);
        setLoaded(true);
      })
      .catch(() => {
        if (cancelled) return;
        // 静默失败：房间页不打扰
        setLoaded(false);
      });
    refresh();
    const timer = window.setInterval(refresh, 3000);
    return () => {
      cancelled = true;
      window.clearInterval(timer);
    };
  }, [currentLogin]);

  if (!currentLogin || !loaded) return null;

  const joinable = myAgents.filter(
    (a) => !participants.some((p) => p.kind === "agent" && p.id === a.agent_id),
  );

  if (joinable.length === 0) return null;

  return (
    <div className="join-agents-section">
      <div className="join-agents-head">
        <Icon name="bot" size={14} /> 加入我的 Agent
      </div>
      <ul className="join-agents-list">
        {joinable.map((agent) => (
          <JoinAgentRow
            key={agent.agent_id}
            agent={agent}
            roomID={roomID}
          />
        ))}
      </ul>
    </div>
  );
}

function JoinAgentRow({ agent, roomID }: { agent: Agent; roomID: string }) {
  const [pending, setPending] = useState(false);
  const [msg, setMsg] = useState<string | null>(null);

  const shortID = (id: string) =>
    id.length <= 16 ? id : `${id.slice(0, 8)}…${id.slice(-4)}`;

  const handleJoin = useCallback(() => {
    setPending(true);
    setMsg(null);
    joinAgentToRoom(roomID, agent.agent_id)
      .then((res) => {
        if (res.delivered) {
          setMsg("已下发加入指令，agent 连接中…");
        } else {
          setMsg("已登记。agent 当前离线，上线后将自动加入本房间");
        }
      })
      .catch((err: unknown) => {
        const raw = err instanceof Error ? err.message : "操作失败";
        // 提取 403 后端文案
        const match = raw.match(/→ \d+: (.+)/);
        setMsg(match ? match[1] : raw);
      })
      .finally(() => setPending(false));
  }, [roomID, agent.agent_id]);

  return (
    <li className="join-agent-row">
      <span className="join-agent-label">
        <StatusDot tone={agent.online ? "live" : "off"} pulse={agent.online} size={8} />
        {agent.label || shortID(agent.agent_id)}
      </span>
      <button
        type="button"
        className="btn btn-sm"
        onClick={handleJoin}
        disabled={pending || msg != null}
      >
        {pending ? "加入中…" : "加入本房间"}
      </button>
      {msg && <span className="agent-join-msg">{msg}</span>}
    </li>
  );
}

/* ── Settings tab ──────────────────────────────────────────────────── */

interface SettingsPanelProps {
  room: Room;
  roomURL: string;
  onCopyURL: () => void;
  onTitleSave: (title: string) => Promise<void>;
  onToggleGated: (next: boolean) => Promise<void>;
  onEnd: () => void;
  onClearHistory: () => void;
  onDestroy: () => void;
}

export function SettingsPanel({
  room,
  roomURL,
  onCopyURL,
  onToggleGated,
  onEnd,
  onClearHistory,
  onDestroy,
}: SettingsPanelProps) {
  const { me } = useAuth();
  const authEnabled = me.auth_enabled;
  const isOwner =
    me.authenticated && Boolean(room.owner) && me.user.login === room.owner;
  const admin = isAdminMe(me);
  // Admins manage every room as if they owned it; canManage drives all the
  // edit affordances below, while `admin && !isOwner` tunes the labels.
  const canManage = isOwner || admin;
  const ended = room.ended;

  return (
    <div className="panel-wrap settings">
      <section className="set-section">
        <h3>房间链接</h3>
        <p>浏览器和本地 Bridge 共用同一个地址，链接本身就是访问凭证。</p>
        <div className="copy-row">
          <code className="copy-field">{roomURL}</code>
          <button type="button" className="btn btn-primary" onClick={onCopyURL}>
            <Icon name="copy" size={15} /> 复制
          </button>
        </div>
      </section>

      {authEnabled && (
        <section className="set-section">
          <div className="set-section-head">
            <h3>权限与配置</h3>
            {isOwner ? (
              <Chip tone="ok">
                <Icon name="shieldCheck" size={12} /> 你是房主
              </Chip>
            ) : admin ? (
              <Chip tone="ok">
                <Icon name="shieldCheck" size={12} /> 管理员
              </Chip>
            ) : (
              <Chip tone="warn">
                <Icon name="lock" size={12} /> 仅房主可修改
              </Chip>
            )}
          </div>
          {admin && !isOwner && (
            <div className="lock-banner">
              <Icon name="shieldCheck" size={15} />
              <span>
                你以<strong>管理员</strong>身份访问
                {room.owner ? <> 房主 <strong>@{room.owner}</strong> 的房间</> : "这间匿名房间"}
                ，对房间配置拥有完整权限。
              </span>
            </div>
          )}
          {!canManage && room.owner && (
            <div className="lock-banner">
              <Icon name="lock" size={15} />
              <span>
                只有房主 <strong>@{room.owner}</strong> 能修改房间权限与配置。下列设置对你只读。
              </span>
            </div>
          )}
          <div className="perm-list">
            <div className={`perm${!canManage || ended ? " is-locked" : ""}`}>
              <div className="perm-text">
                <strong>新访客需 Owner 批准</strong>
                <span>开启后，所有进入房间的人都要先经过你审批才能看消息。</span>
              </div>
              <Switch
                on={room.gated}
                disabled={!canManage || ended}
                onToggle={(next) => onToggleGated(next)}
              />
            </div>
          </div>
        </section>
      )}

      <section className="set-section danger">
        <h3>房间生命周期</h3>
        {canManage ? (
          <>
            <div className="life-row">
              <div className="life-text">
                <strong>清空历史</strong>
                <span>删除聊天、活动日志与滚动摘要，保留房间、成员和权限配置。</span>
              </div>
              <button type="button" className="btn btn-danger" onClick={onClearHistory}>
                <Icon name="trash" size={14} /> 清空历史
              </button>
            </div>
            <div className="life-row">
              <div className="life-text">
                <strong>结束会议</strong>
                <span>切换为只读：保留全部历史，但不能再发消息或跑命令。</span>
              </div>
              <button
                type="button"
                className="btn btn-warn"
                disabled={ended}
                onClick={onEnd}
              >
                <Icon name="stop" size={14} /> {ended ? "已结束" : "结束会议"}
              </button>
            </div>
            <div className="life-row danger-row">
              <div className="life-text">
                <strong>销毁房间</strong>
                <span>永久删除房间历史、审计与所有连接，无法恢复。</span>
              </div>
              <button type="button" className="btn btn-danger" onClick={onDestroy}>
                <Icon name="alert" size={14} /> 销毁房间
              </button>
            </div>
          </>
        ) : (
          <div className="life-row">
            <div className="life-text">
              <strong>退出房间</strong>
              <span>
                {authEnabled
                  ? "作为成员，你可以随时离开。结束或销毁房间只有房主能操作。"
                  : "这是一间匿名房间，没有房主控制；离开浏览器即可退出。"}
              </span>
            </div>
          </div>
        )}
      </section>
    </div>
  );
}

function Switch({
  on,
  disabled,
  onToggle,
}: {
  on: boolean;
  disabled?: boolean;
  onToggle: (next: boolean) => void;
}) {
  return (
    <button
      type="button"
      className={`switch${on ? " on" : ""}${disabled ? " disabled" : ""}`}
      role="switch"
      aria-checked={on}
      disabled={disabled}
      onClick={() => !disabled && onToggle(!on)}
    >
      <span className="switch-knob" />
    </button>
  );
}

/* ── Dev-only Tweaks panel ─────────────────────────────────────────── */

export function DevTweaksPanel() {
  const { theme, setTheme, density, setDensity, accentHue, setAccentHue } = useTheme();
  const [open, setOpen] = useState(false);
  if (!open) {
    return (
      <button type="button" className="dev-tweaks-chip" onClick={() => setOpen(true)}>
        <Icon name="settings" size={13} /> tweaks (dev)
      </button>
    );
  }
  return (
    <div className="dev-tweaks">
      <div className="dev-tweaks-head">
        <strong>Tweaks · dev only</strong>
        <button type="button" onClick={() => setOpen(false)}>
          ✕
        </button>
      </div>
      <div className="dev-tweaks-row">
        <label>theme</label>
        <div className="dev-tweaks-seg">
          {(["paper", "operator", "signal"] as const).map((t) => (
            <button
              key={t}
              type="button"
              className={t === theme ? "on" : ""}
              onClick={() => setTheme(t)}
            >
              {t}
            </button>
          ))}
        </div>
      </div>
      <div className="dev-tweaks-row">
        <label>density</label>
        <div className="dev-tweaks-seg">
          {(["compact", "regular", "comfy"] as const).map((d) => (
            <button
              key={d}
              type="button"
              className={d === density ? "on" : ""}
              onClick={() => setDensity(d)}
            >
              {d}
            </button>
          ))}
        </div>
      </div>
      <div className="dev-tweaks-row">
        <label>accent hue</label>
        <div className="dev-tweaks-seg">
          {["152", "205", "28", "320", "265"].map((h) => (
            <button
              key={h}
              type="button"
              className={h === accentHue ? "on" : ""}
              onClick={() => setAccentHue(h)}
              style={{ color: `oklch(0.6 0.16 ${h})` }}
            >
              {h}
            </button>
          ))}
        </div>
      </div>
    </div>
  );
}
