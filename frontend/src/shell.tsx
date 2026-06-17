import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ChangeEvent,
  type ClipboardEvent,
  type DragEvent,
  type KeyboardEvent,
  type ReactNode,
} from "react";
import type { AccessRequest, Agent, ChatMessage, Participant, Room } from "./types";
import { Icon, OsGlyph } from "./icons";
import { Avatar, BrandMark, Chip, ModeBadge, OsAvatar, SignInPill, StatusDot } from "./ui";
import {
  agentTint,
  hueFromID,
  initials,
  relativeTime,
  shortRoomID,
} from "./lib";
import { useAuth } from "./auth";
import { UserMenu, detectQuickStartOS, type RoomRecord } from "./home";
import { joinAgentToRoom, listAgents } from "./api";

/* ── Sidebar ──────────────────────────────────────────────────────── */

interface RoomSidebarProps {
  room: Room;
  roomID: string;
  roomRecords: RoomRecord[];
  participants: Participant[];
  accessRequests: AccessRequest[];
  isOwner: boolean;
  dark: boolean;
  onHome: () => void;
  onNewRoom: () => void;
  onSwitchRoom: (room: string) => void;
  onForgetRoom: (roomID: string) => void;
  onCopyURL: () => void;
  onCopyID: () => void;
  onResolveRequest: (req: AccessRequest, decision: "approve" | "deny", persistence?: "once" | "persist") => void;
}

export function RoomSidebar({
  room,
  roomID,
  roomRecords,
  participants,
  accessRequests,
  isOwner,
  dark,
  onHome,
  onNewRoom,
  onSwitchRoom,
  onForgetRoom,
  onCopyURL,
  onCopyID,
  onResolveRequest,
}: RoomSidebarProps) {
  const { me } = useAuth();
  const authEnabled = me.auth_enabled;
  const agents = participants.filter((p) => p.kind === "agent");
  const users = participants.filter((p) => p.kind === "user");
  const [agentPickerOpen, setAgentPickerOpen] = useState(false);

  return (
    <aside className="sidebar">
      <div className="side-brand">
        <button className="brand-home" onClick={onHome} type="button" title="返回首页">
          <BrandMark size={32} />
        </button>
        <div className="side-brand-text">
          <strong>Agent Room</strong>
          <span>isolated agents, one shared room</span>
        </div>
      </div>

      <RoomSwitcher
        room={room}
        roomID={roomID}
        roomRecords={roomRecords}
        onSwitch={onSwitchRoom}
        onNewRoom={onNewRoom}
        onForget={onForgetRoom}
        onCopyID={onCopyID}
        onHome={onHome}
      />

      <section className="side-section">
        <div className="side-head">
          <span>
            <Icon name="users" size={14} /> Members
          </span>
          <Chip tone="mono">{users.length || 1}</Chip>
        </div>
        <div className="side-people">
          {users.length === 0 ? (
            <div className="side-empty">仅你一人</div>
          ) : (
            users.map((p) => <PersonRow key={p.id} person={p} dark={dark} />)
          )}
        </div>
      </section>

      <section className="side-section">
        <div className="side-head">
          <span>
            <Icon name="cpu" size={14} /> Bridges
          </span>
          <span className="side-head-actions">
            {me.authenticated && (
              <button
                type="button"
                className="side-add-agent"
                title="加入我的 Agent"
                aria-label="加入我的 Agent"
                onClick={() => setAgentPickerOpen((open) => !open)}
              >
                <Icon name="plus" size={13} />
              </button>
            )}
            <Chip tone="mono">{agents.length}</Chip>
          </span>
        </div>
        {agentPickerOpen && me.authenticated && (
          <SidebarAgentPicker
            roomID={roomID}
            participants={participants}
            currentLogin={me.user.login}
          />
        )}
        <div className="side-people">
          {agents.length === 0 ? (
            <div className="side-empty">暂无 Bridge 在线</div>
          ) : (
            agents.map((p) => <PersonRow key={p.id} person={p} dark={dark} />)
          )}
        </div>
      </section>

      <BridgeDownloadSection roomID={roomID} />

      <AccessControlSection
        room={room}
        authEnabled={authEnabled}
        isOwner={isOwner}
        accessRequests={accessRequests}
        dark={dark}
        onResolve={onResolveRequest}
      />
    </aside>
  );
}

/* ── Bridge download (sidebar quick access) ───────────────────────── */

// One-tap onboarding from inside a room, pinned to this exact room. All
// three OSes get a copyable download + `agent-room bridge -relay <origin>
// -room <id>` snippet (curl/bash on macOS/Linux, PowerShell on Windows).
// Nothing is baked into the binary itself — the relay address always comes
// from the page origin, so a stale download can never carry an outdated
// relay URL. The bridge upgrades the http(s) origin to ws(s) and appends
// the room path itself (see normalizeRelayWSURL).
// Defaults to the visitor's detected OS so the right thing shows first.
const SIDE_DL_OSES: { id: SideDlOS; label: string }[] = [
  { id: "mac", label: "macOS" },
  { id: "linux", label: "Linux" },
  { id: "windows", label: "Windows" },
];
type SideDlOS = "mac" | "linux" | "windows";

function unixConnectSnippet(origin: string, roomID: string): string {
  return [
    "# 1. 安装 CLI（curl 下载预编译二进制，无需 Go）",
    `curl -fsSL ${origin}/downloads/install.sh | bash`,
    'export PATH="$HOME/.local/bin:$PATH"',
    "",
    "# 2. 接入本房间（需本机已装并登录 Claude Code CLI）",
    `export IS_SANDBOX=1 && agent-room bridge -relay ${origin} -room ${roomID}`,
    "",
    "# 仅当 Executor（不跑 AI，被 @ 时执行命令）：追加 -bridge-mode executor",
  ].join("\n");
}

function windowsConnectSnippet(origin: string, roomID: string): string {
  return [
    "# 1. 下载 CLI（PowerShell，无需 Go）",
    `iwr ${origin}/downloads/windows -OutFile agent-room.exe`,
    "",
    "# 2. 接入本房间（需本机已装并登录 Claude Code CLI）",
    `.\\agent-room.exe bridge -relay ${origin} -room ${roomID}`,
    "",
    "# 仅当 Executor（不跑 AI，被 @ 时执行命令）：追加 -bridge-mode executor",
  ].join("\n");
}

function BridgeDownloadSection({ roomID }: { roomID: string }) {
  const origin = typeof window === "undefined" ? "" : window.location.origin || "";
  const [os, setOs] = useState<SideDlOS>(() => detectQuickStartOS());
  const [copied, setCopied] = useState(false);
  const snippet =
    os === "windows" ? windowsConnectSnippet(origin, roomID) : unixConnectSnippet(origin, roomID);

  function copy() {
    navigator.clipboard.writeText(snippet).then(
      () => {
        setCopied(true);
        window.setTimeout(() => setCopied(false), 1600);
      },
      () => setCopied(false),
    );
  }

  return (
    <section className="side-section">
      <div className="side-head">
        <span>
          <Icon name="download" size={14} /> 接入本机
        </span>
      </div>
      <div className="side-download">
        <div className="side-dl-tabs" role="tablist" aria-label="选择操作系统">
          {SIDE_DL_OSES.map((entry) => (
            <button
              key={entry.id}
              type="button"
              role="tab"
              aria-selected={os === entry.id}
              className={os === entry.id ? "on" : ""}
              onClick={() => setOs(entry.id)}
            >
              <OsGlyph os={entry.id} size={13} /> {entry.label}
            </button>
          ))}
        </div>
        <pre className="side-dl-code">{snippet}</pre>
        <button type="button" className="side-download-btn" onClick={copy}>
          <Icon name="copy" size={14} /> {copied ? "已复制" : "复制命令"}
        </button>
        <p className="side-download-note">
          已预填本房间与中转地址，粘贴到{os === "windows" ? " PowerShell " : "终端"}执行即接入。无需 Go 环境。
        </p>
      </div>
    </section>
  );
}

function SidebarAgentPicker({
  roomID,
  participants,
  currentLogin,
}: {
  roomID: string;
  participants: Participant[];
  currentLogin: string;
}) {
  const [agents, setAgents] = useState<Agent[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [joining, setJoining] = useState<string | null>(null);
  const [notice, setNotice] = useState("");
  const joinedIDs = useMemo(
    () => new Set(participants.filter((p) => p.kind === "agent").map((p) => p.id)),
    [participants],
  );

  const refresh = useCallback(async () => {
    try {
      const rows = await listAgents();
      setAgents(
        rows
          .filter((a) => !a.revoked && (!a.owner_login || a.owner_login === currentLogin))
          .sort((a, b) => Number(b.online) - Number(a.online) || a.agent_id.localeCompare(b.agent_id)),
      );
      setError("");
    } catch (err) {
      setError(err instanceof Error ? err.message : "加载 agent 失败");
    } finally {
      setLoading(false);
    }
  }, [currentLogin]);

  useEffect(() => {
    let alive = true;
    async function tick() {
      if (!alive) return;
      await refresh();
    }
    tick();
    const timer = window.setInterval(tick, 3000);
    return () => {
      alive = false;
      window.clearInterval(timer);
    };
  }, [refresh]);

  async function join(agent: Agent) {
    if (joining || joinedIDs.has(agent.agent_id) || agent.rooms?.includes(roomID)) return;
    setJoining(agent.agent_id);
    setNotice("");
    try {
      const res = await joinAgentToRoom(roomID, agent.agent_id);
      setNotice(res.delivered ? "已发送加入指令" : "已保存，agent 上线后会加入");
      await refresh();
    } catch (err) {
      setNotice(err instanceof Error ? err.message : "加入失败");
    } finally {
      setJoining(null);
    }
  }

  return (
    <div className="side-agent-picker">
      <div className="side-agent-picker-head">
        <span>我的 Agent</span>
        <button type="button" onClick={refresh} title="刷新" aria-label="刷新">
          <Icon name="refresh" size={12} />
        </button>
      </div>
      {loading ? (
        <div className="side-agent-empty">加载中...</div>
      ) : error ? (
        <div className="side-agent-empty">{error}</div>
      ) : agents.length === 0 ? (
        <div className="side-agent-empty">还没有注册的 Agent</div>
      ) : (
        <div className="side-agent-list">
          {agents.map((agent) => {
            const joined = joinedIDs.has(agent.agent_id) || agent.rooms?.includes(roomID);
            return (
              <div className="side-agent-row" key={agent.agent_id}>
                <span className="side-agent-live">
                  <StatusDot tone={agent.online ? "live" : "off"} pulse={agent.online} size={7} />
                </span>
                <span className="side-agent-main">
                  <strong title={agent.agent_id}>{agent.label || agent.agent_id}</strong>
                  <span>
                    {agent.online ? "在线" : "离线"} · {agent.provider || "agent"}
                  </span>
                </span>
                <button
                  type="button"
                  className={joined ? "side-agent-join is-joined" : "side-agent-join"}
                  disabled={joined || joining === agent.agent_id}
                  onClick={() => join(agent)}
                >
                  {joined ? "已加入" : joining === agent.agent_id ? "加入中" : "加入"}
                </button>
              </div>
            );
          })}
        </div>
      )}
      {notice && <div className="side-agent-notice">{notice}</div>}
    </div>
  );
}

/* ── Room switcher (top-left popover) ─────────────────────────────── */

// The top-left ROOM chip doubles as a switcher: it lists locally-remembered
// joined rooms (from `roomRecords`) so you can hop between them without going
// back home. Switching reuses the existing `enterRoom` pipeline (WS reconnect,
// history/participants reload, URL sync) — no new state, zero backend change.
interface RoomSwitcherProps {
  room: Room;
  roomID: string;
  roomRecords: RoomRecord[];
  onSwitch: (room: string) => void;
  onNewRoom: () => void;
  onForget: (roomID: string) => void;
  onCopyID: () => void;
  onHome: () => void;
}

function RoomSwitcher({
  room,
  roomID,
  roomRecords,
  onSwitch,
  onNewRoom,
  onForget,
  onCopyID,
  onHome,
}: RoomSwitcherProps) {
  const [open, setOpen] = useState(false);
  const [selected, setSelected] = useState(0);
  const [joinDraft, setJoinDraft] = useState("");
  const wrapRef = useRef<HTMLDivElement | null>(null);

  // Current room pinned first (checked), then the rest of the saved records.
  // app.tsx already sorts records most-recent-first and upserts the current
  // room, but we synthesize a fallback entry so a brand-new/unsaved room still
  // shows (single-room empty state).
  const items = useMemo(() => {
    const rest = roomRecords.filter((r) => r.room !== roomID);
    const current: RoomRecord =
      roomRecords.find((r) => r.room === roomID) || {
        room: roomID,
        createdAt: "",
        lastOpenedAt: "",
        title: room.title ?? undefined,
      };
    return [current, ...rest];
  }, [roomID, roomRecords, room.title]);

  // Reset the highlighted row each time the menu opens.
  useEffect(() => {
    if (open) setSelected(0);
  }, [open]);

  // Close on outside click / Escape.
  useEffect(() => {
    if (!open) return;
    function onDocClick(e: MouseEvent) {
      if (wrapRef.current && !wrapRef.current.contains(e.target as Node)) setOpen(false);
    }
    function onKey(e: globalThis.KeyboardEvent) {
      if (e.key === "Escape") setOpen(false);
    }
    document.addEventListener("mousedown", onDocClick);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDocClick);
      document.removeEventListener("keydown", onKey);
    };
  }, [open]);

  function pick(target: RoomRecord) {
    if (target.room !== roomID) onSwitch(target.room);
    setOpen(false);
  }

  // Arrow keys / Enter while the trigger (or list) is focused. Bubbles up from
  // the trigger button; the join input stops propagation so typing there never
  // navigates the list.
  function onMenuKeyDown(e: KeyboardEvent<HTMLDivElement>) {
    if (!open) return;
    if (e.key === "ArrowDown") {
      e.preventDefault();
      setSelected((s) => (s + 1) % items.length);
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setSelected((s) => (s - 1 + items.length) % items.length);
    } else if (e.key === "Enter") {
      e.preventDefault();
      const target = items[Math.min(selected, items.length - 1)];
      if (target) pick(target);
    }
  }

  function submitJoin() {
    const value = joinDraft.trim();
    if (!value) return;
    onSwitch(value);
    setJoinDraft("");
    setOpen(false);
  }

  return (
    <div className="side-room" ref={wrapRef}>
      <div className="room-switcher" onKeyDown={onMenuKeyDown}>
        <button
          className="room-chip"
          type="button"
          onClick={() => setOpen((v) => !v)}
          aria-haspopup="listbox"
          aria-expanded={open}
          title="切换房间"
        >
          <span className="room-chip-cap">ROOM</span>
          <span className="room-chip-id">{room.title || shortRoomID(roomID)}</span>
          <Icon name="chevronDown" size={14} className={open ? "spin-180" : ""} />
        </button>

        {open && (
          <div className="room-switch-menu" role="listbox">
            <div className="room-switch-list">
              {items.map((rec, i) => {
                const isCurrent = rec.room === roomID;
                return (
                  <div
                    key={rec.room}
                    className={`room-switch-opt${i === selected ? " on" : ""}${
                      isCurrent ? " current" : ""
                    }`}
                    role="option"
                    aria-selected={i === selected}
                  >
                    <button
                      type="button"
                      className="room-switch-pick"
                      onClick={() => pick(rec)}
                      onMouseEnter={() => setSelected(i)}
                      title={rec.room}
                    >
                      <span className="room-switch-check">
                        {isCurrent ? (
                          <Icon name="check" size={14} />
                        ) : (
                          <Icon name="hash" size={13} />
                        )}
                      </span>
                      <span className="room-switch-text">
                        <strong>{rec.title || shortRoomID(rec.room)}</strong>
                        <span>{isCurrent ? "当前房间" : relativeTime(rec.lastOpenedAt)}</span>
                      </span>
                    </button>
                    {!isCurrent && (
                      <button
                        type="button"
                        className="room-switch-forget"
                        onClick={() => onForget(rec.room)}
                        title="从列表移除"
                        aria-label="从列表移除"
                      >
                        <Icon name="x" size={13} />
                      </button>
                    )}
                  </div>
                );
              })}
            </div>

            <div className="room-switch-join">
              <input
                value={joinDraft}
                onChange={(e) => setJoinDraft(e.target.value)}
                onKeyDown={(e) => {
                  e.stopPropagation();
                  if (e.key === "Enter") {
                    e.preventDefault();
                    submitJoin();
                  } else if (e.key === "Escape") {
                    setOpen(false);
                  }
                }}
                placeholder="输入房间 ID 或链接加入"
                aria-label="输入房间 ID 或链接加入"
              />
              <button
                type="button"
                className="room-switch-join-btn"
                onClick={submitJoin}
                title="加入"
                aria-label="加入"
              >
                <Icon name="chevronRight" size={15} />
              </button>
            </div>

            <div className="room-switch-actions">
              <button
                type="button"
                onClick={() => {
                  onNewRoom();
                  setOpen(false);
                }}
              >
                <Icon name="plus" size={14} /> 新建房间
              </button>
              <button
                type="button"
                onClick={() => {
                  onHome();
                  setOpen(false);
                }}
              >
                <Icon name="home" size={14} /> 返回首页
              </button>
            </div>
          </div>
        )}
      </div>

      <button
        className="icon-btn icon-btn-line"
        type="button"
        onClick={onCopyID}
        title="复制房间 ID"
        aria-label="复制房间 ID"
      >
        <Icon name="copy" size={15} />
      </button>
    </div>
  );
}

/* ── Access control (always present at sidebar bottom) ─────────────── */

// AccessControlSection is the persistent bottom-left block. It always
// renders an "访问控制" header so the area is meaningful regardless of
// state, and adapts its body to who is looking:
//   - owner of a gated room   → live approval list
//   - owner of an open room    → hint to enable 访客审核 in Settings
//   - non-owner of gated room  → "由房主审批" lock note
//   - anonymous / auth off     → "链接即访问凭证" note
function AccessControlSection({
  room,
  authEnabled,
  isOwner,
  accessRequests,
  dark,
  onResolve,
}: {
  room: Room;
  authEnabled: boolean;
  isOwner: boolean;
  accessRequests: AccessRequest[];
  dark: boolean;
  onResolve: (req: AccessRequest, decision: "approve" | "deny", persistence?: "once" | "persist") => void;
}) {
  // Owner of a gated room gets the interactive approval list.
  if (authEnabled && room.gated && isOwner) {
    return <AccessRequestList requests={accessRequests} dark={dark} onResolve={onResolve} />;
  }

  let note: ReactNode;
  if (authEnabled && room.gated && !isOwner) {
    note = (
      <>
        <Icon name="lock" size={13} /> 外部访问申请由房主审批
      </>
    );
  } else if (authEnabled && isOwner) {
    // Owner, room not gated yet — point them at the toggle.
    note = (
      <>
        <Icon name="shield" size={13} /> 房间公开可访问 · 在 Settings 开启访客审核后在此审批
      </>
    );
  } else {
    // Anonymous room or auth disabled — the link itself is the credential.
    note = (
      <>
        <Icon name="lock" size={13} /> 房间链接即访问凭证 · 仅向受信任的人分享
      </>
    );
  }

  return (
    <section className="access">
      <div className="side-head access-head">
        <span>
          <Icon name="at" size={14} /> 访问控制
        </span>
      </div>
      <div className="access-note">{note}</div>
    </section>
  );
}

function PersonRow({ person, dark }: { person: Participant; dark: boolean }) {
  const hue = hueFromID(person.id);
  const tint = agentTint(hue, dark);
  const isAgent = person.kind === "agent";
  const mode = person.metadata?.mode;
  return (
    <div className="person">
      {isAgent ? (
        <OsAvatar
          person={{ id: person.id, label: person.label || person.id, kind: "agent", hue }}
          dark={dark}
          size={32}
          os={person.metadata?.os}
        />
      ) : (
        <Avatar
          person={{
            id: person.id,
            label: person.label || person.id,
            kind: person.kind === "user" ? "user" : person.kind === "agent" ? "agent" : "system",
            hue,
          }}
          dark={dark}
          size={32}
        />
      )}
      <div className="person-body">
        <strong style={{ color: tint.text }} title={person.label || person.id}>
          {person.label || person.id}
        </strong>
        <span>{personSubLine(person)}</span>
      </div>
      {isAgent ? (
        <div className="person-tags">
          {mode && <ModeBadge mode={mode} />}
          <span className="person-status">
            <StatusDot tone="live" pulse /> 在线
          </span>
        </div>
      ) : null}
    </div>
  );
}

function personSubLine(person: Participant): string {
  const parts: string[] = [];
  if ((person.connection_count || 0) > 1) {
    parts.push(`${person.connection_count} sessions`);
  } else {
    parts.push(relativeTime(person.last_seen_at));
  }
  if (person.metadata?.provider) parts.push(person.metadata.provider);
  if (person.metadata?.device) parts.push(person.metadata.device);
  return parts.join(" · ");
}

/* ── Access requests (owner side) ─────────────────────────────────── */

function AccessRequestList({
  requests,
  dark,
  onResolve,
}: {
  requests: AccessRequest[];
  dark: boolean;
  onResolve: (req: AccessRequest, decision: "approve" | "deny", persistence?: "once" | "persist") => void;
}) {
  const [choosing, setChoosing] = useState<string | null>(null);
  const pending = requests.filter((r) => r.status === "pending");
  return (
    <section className="access">
      <div className="side-head access-head">
        <span>
          <Icon name="at" size={14} /> 访问申请
        </span>
        {pending.length > 0 && <span className="access-badge">{pending.length}</span>}
      </div>
      {pending.length === 0 ? (
        <div className="side-empty">暂无待审批的申请</div>
      ) : (
        <div className="access-list">
          {pending.map((r) => {
            const hue = hueFromID(r.id);
            const tint = agentTint(hue, dark);
            const open = choosing === r.id;
            return (
              <div className={`access-req${open ? " is-open" : ""}`} key={r.id}>
                <div className="access-top">
                  <span
                    className="access-av"
                    style={{
                      background: tint.soft,
                      color: tint.text,
                      boxShadow: `inset 0 0 0 1px ${tint.line}`,
                    }}
                  >
                    {initials(r.requester_label)}
                  </span>
                  <div className="access-id">
                    <strong>@{r.requester_label}</strong>
                    <span>{r.via}</span>
                  </div>
                  <span className="access-ago">{relativeTime(r.created_at)}</span>
                </div>
                {r.location && (
                  <div className="access-meta">
                    <Icon name="cpu" size={11} /> {r.location}
                  </div>
                )}

                {!open ? (
                  <div className="access-actions">
                    <button
                      type="button"
                      className="abtn abtn-deny"
                      onClick={() => onResolve(r, "deny")}
                    >
                      <Icon name="x" size={13} /> 拒绝
                    </button>
                    <button
                      type="button"
                      className="abtn abtn-ok"
                      onClick={() => setChoosing(r.id)}
                    >
                      <Icon name="check" size={13} /> 批准…
                    </button>
                  </div>
                ) : (
                  <div className="access-grant">
                    <span className="access-grant-q">
                      以哪种方式批准 <strong>@{r.requester_label}</strong>?
                    </span>
                    <button
                      type="button"
                      className="grant-opt"
                      onClick={() => {
                        onResolve(r, "approve", "persist");
                        setChoosing(null);
                      }}
                    >
                      <span className="grant-ic">
                        <Icon name="shieldCheck" size={15} />
                      </span>
                      <span className="grant-text">
                        <strong>保留身份</strong>
                        <span>记住此人,后续可随时再进入房间</span>
                      </span>
                    </button>
                    <button
                      type="button"
                      className="grant-opt"
                      onClick={() => {
                        onResolve(r, "approve", "once");
                        setChoosing(null);
                      }}
                    >
                      <span className="grant-ic grant-ic-once">
                        <Icon name="clock" size={15} />
                      </span>
                      <span className="grant-text">
                        <strong>仅此一次</strong>
                        <span>本次会话有效,离开后需重新申请</span>
                      </span>
                    </button>
                    <button
                      type="button"
                      className="grant-cancel"
                      onClick={() => setChoosing(null)}
                    >
                      取消
                    </button>
                  </div>
                )}
              </div>
            );
          })}
        </div>
      )}
    </section>
  );
}

/* ── Room header ──────────────────────────────────────────────────── */

export type RoomTab = "chat" | "activity" | "approvals" | "summary" | "agents" | "settings";

const TABS: { id: RoomTab; label: string; icon: string }[] = [
  { id: "chat", label: "Chat", icon: "at" },
  { id: "activity", label: "Activity", icon: "activity" },
  { id: "approvals", label: "Approvals", icon: "shield" },
  { id: "summary", label: "Summary", icon: "sparkle" },
  { id: "agents", label: "Agents", icon: "bot" },
  { id: "settings", label: "Settings", icon: "settings" },
];

interface RoomHeaderProps {
  room: Room;
  roomID: string;
  tab: RoomTab;
  setTab: (t: RoomTab) => void;
  isOwner: boolean;
  connected: boolean;
  busy: boolean;
  onCopyURL: () => void;
  onRename: (next: string) => Promise<void> | void;
  headerRight?: ReactNode;
}

export function RoomHeader({
  room,
  roomID,
  tab,
  setTab,
  isOwner,
  connected,
  busy,
  onCopyURL,
  onRename,
  headerRight,
}: RoomHeaderProps) {
  return (
    <header className="room-head">
      <div className="room-head-top">
        <div className="room-head-title">
          <div className="room-title-row">
            <EditableRoomTitle
              title={room.title || "untitled room"}
              isOwner={isOwner}
              ended={room.ended}
              onRename={onRename}
            />
            <button type="button" className="btn btn-primary room-share-btn" onClick={onCopyURL}>
              <Icon name="link" size={15} /> 分享房间
            </button>
          </div>
          <span className="room-head-sub">
            Room {shortRoomID(roomID)}
            {room.owner ? ` · @${room.owner}` : " · anonymous"}
            {room.ended ? " · 已结束" : ""}
          </span>
        </div>
        <div className="room-head-actions">
          {room.ended ? (
            <span className="live-pill ended">
              <Icon name="lock" size={13} /> 已结束 · 只读
            </span>
          ) : (
            <span className={`live-pill${busy ? " on" : ""}`}>
              <StatusDot tone={connected ? "live" : "off"} pulse={connected} />
              {connected ? (busy ? "agents working" : "meeting live") : "离线"}
            </span>
          )}
          {headerRight}
        </div>
      </div>
      <div className="room-tabs">
        {TABS.map((tb) => (
          <button
            key={tb.id}
            type="button"
            className={`room-tab${tab === tb.id ? " on" : ""}`}
            onClick={() => setTab(tb.id)}
          >
            <Icon name={tb.icon} size={15} /> {tb.label}
          </button>
        ))}
      </div>
    </header>
  );
}

function EditableRoomTitle({
  title,
  isOwner,
  ended,
  onRename,
}: {
  title: string;
  isOwner: boolean;
  ended: boolean;
  onRename: (next: string) => Promise<void> | void;
}) {
  const canEdit = isOwner && !ended;
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState(title);
  const inputRef = useRef<HTMLInputElement | null>(null);

  useEffect(() => {
    setDraft(title);
  }, [title]);

  useEffect(() => {
    if (editing && inputRef.current) {
      inputRef.current.focus();
      inputRef.current.select();
    }
  }, [editing]);

  if (editing) {
    return (
      <input
        ref={inputRef}
        className="room-title-input"
        value={draft}
        maxLength={60}
        onChange={(e) => setDraft(e.target.value)}
        onBlur={async () => {
          if (draft.trim() && draft !== title) await onRename(draft.trim());
          setEditing(false);
        }}
        onKeyDown={async (e) => {
          if (e.key === "Enter") {
            e.preventDefault();
            if (draft.trim() && draft !== title) await onRename(draft.trim());
            setEditing(false);
          }
          if (e.key === "Escape") {
            e.preventDefault();
            setDraft(title);
            setEditing(false);
          }
        }}
      />
    );
  }
  return (
    <h1
      className={`room-title${canEdit ? " editable" : ""}`}
      onClick={() => canEdit && setEditing(true)}
      title={canEdit ? "点击重命名 · 回车保存" : undefined}
    >
      <span>{title}</span>
      {canEdit && <Icon name="pencil" size={15} className="room-title-pen" />}
    </h1>
  );
}

/* ── Composer ─────────────────────────────────────────────────────── */

export interface MentionOption {
  id: string;
  label: string;
  kind: "user" | "agent" | "system" | "room";
  detail: string;
}

// PendingAttachment 是已上传、待随下一条消息发送的图片附件。
export interface PendingAttachment {
  id: string;
  mime: string;
  size: number;
  name: string;
  url: string;
}

interface ComposerProps {
  content: string;
  ended: boolean;
  busy: boolean;
  textareaRef: React.RefObject<HTMLTextAreaElement | null>;
  mentionOpen: boolean;
  mentionSelected: number;
  mentionOptions: MentionOption[];
  attachments: PendingAttachment[];
  uploadingCount: number;
  replyTo: ChatMessage | null;
  replyPreview: string;
  onCancelReply: () => void;
  onAttachFiles: (files: File[]) => void;
  onRemoveAttachment: (id: string) => void;
  onContentChange: (event: ChangeEvent<HTMLTextAreaElement>) => void;
  onSend: () => void;
  onStop?: () => void;
  onKeyDown: (event: KeyboardEvent<HTMLTextAreaElement>) => void;
  onPickMention: (option: MentionOption) => void;
}

// imageFilesFrom 从 DataTransfer/FileList 里筛出图片文件(粘贴/拖拽共用)。
function imageFilesFrom(items: FileList | File[] | null | undefined): File[] {
  if (!items) return [];
  return Array.from(items).filter((file) => file.type.startsWith("image/"));
}

export function Composer({
  content,
  ended,
  busy,
  textareaRef,
  mentionOpen,
  mentionSelected,
  mentionOptions,
  attachments,
  uploadingCount,
  replyTo,
  replyPreview,
  onCancelReply,
  onAttachFiles,
  onRemoveAttachment,
  onContentChange,
  onSend,
  onStop,
  onKeyDown,
  onPickMention,
}: ComposerProps) {
  const fileInputRef = useRef<HTMLInputElement | null>(null);
  const [dragOver, setDragOver] = useState(false);

  function onPaste(event: ClipboardEvent<HTMLTextAreaElement>) {
    const files = imageFilesFrom(event.clipboardData?.files);
    if (files.length === 0) return;
    event.preventDefault();
    onAttachFiles(files);
  }

  function onDrop(event: DragEvent<HTMLElement>) {
    const files = imageFilesFrom(event.dataTransfer?.files);
    setDragOver(false);
    if (files.length === 0) return;
    event.preventDefault();
    onAttachFiles(files);
  }

  if (ended) {
    return (
      <footer className="composer">
        <div className="composer-rail">
          <div className="composer-ended">
            <Icon name="lock" size={15} /> 房间已结束 · 只读模式
          </div>
        </div>
      </footer>
    );
  }
  const placeholder = "发消息 — 用 @ 提及房间成员或召唤 agent";
  return (
    <footer
      className={dragOver ? "composer composer-dragover" : "composer"}
      onDragOver={(event) => {
        if (event.dataTransfer?.types.includes("Files")) {
          event.preventDefault();
          setDragOver(true);
        }
      }}
      onDragLeave={() => setDragOver(false)}
      onDrop={onDrop}
    >
      {(attachments.length > 0 || uploadingCount > 0) && (
        <div className="composer-attachments">
          {attachments.map((att) => (
            <span key={att.id} className="attach-thumb" title={att.name}>
              <img src={att.url} alt={att.name} />
              <button
                type="button"
                className="attach-remove"
                onClick={() => onRemoveAttachment(att.id)}
                title="移除图片"
                aria-label="移除图片"
              >
                ✕
              </button>
            </span>
          ))}
          {uploadingCount > 0 && (
            <span className="attach-thumb attach-uploading" aria-live="polite">
              <span className="messages-backfill-spin" />
            </span>
          )}
        </div>
      )}
      {replyTo && (
        <div className="composer-reply">
          <div>
            <span>回复 {replyTo.sender_id || "unknown"}</span>
            <p>{replyPreview}</p>
          </div>
          <button
            type="button"
            className="composer-reply-close"
            onClick={onCancelReply}
            title="取消回复"
            aria-label="取消回复"
          >
            <Icon name="x" size={14} />
          </button>
        </div>
      )}
      <div className="composer-rail">
        <div className="composer-box">
          {mentionOpen && (
            <div className="mention-menu" role="listbox" data-testid="mention-menu">
              {mentionOptions.length === 0 ? (
                <div className="mention-empty">No matching targets</div>
              ) : (
                mentionOptions.map((option, index) => (
                  <button
                    key={option.id}
                    type="button"
                    role="option"
                    aria-selected={index === mentionSelected}
                    className={`mention-opt${index === mentionSelected ? " on" : ""}`}
                    onMouseDown={(e) => {
                      e.preventDefault();
                      onPickMention(option);
                    }}
                  >
                    <span className="mention-at">@{option.id}</span>
                    <span className="mention-info">
                      <strong>{option.label}</strong>
                      <span>{option.detail}</span>
                    </span>
                    <Chip tone="mono">{option.kind}</Chip>
                  </button>
                ))
              )}
            </div>
          )}
          <textarea
            ref={textareaRef}
            value={content}
            rows={1}
            placeholder={busy ? "agent 回复中 · 可继续输入，回复后可发送" : placeholder}
            onChange={onContentChange}
            onKeyDown={onKeyDown}
            onPaste={onPaste}
          />
          <div className="composer-hint">
            <span>
              {busy ? (
                <>
                  <span className="composer-dot" /> agent 正在回复 · 输入会保留，回复后可发送
                </>
              ) : (
                <>
                  <kbd>@</kbd> 提及成员 · <kbd>Enter</kbd> 发送 · <kbd>Shift+Enter</kbd> 换行
                </>
              )}
            </span>
          </div>
        </div>
        <input
          ref={fileInputRef}
          type="file"
          accept="image/png,image/jpeg,image/webp,image/gif"
          multiple
          style={{ display: "none" }}
          onChange={(event) => {
            onAttachFiles(imageFilesFrom(event.currentTarget.files));
            event.currentTarget.value = "";
          }}
        />
        <button
          type="button"
          className="attach-btn"
          onClick={() => fileInputRef.current?.click()}
          disabled={false}
          title="发送图片（也可粘贴或拖拽）"
          aria-label="发送图片"
        >
          <Icon name="image" size={18} />
        </button>
        <button
          type="button"
          className={busy && onStop ? "send-btn send-btn-stop" : "send-btn"}
          onClick={busy && onStop ? onStop : onSend}
          disabled={busy ? !onStop : !content.trim() && attachments.length === 0}
          title={busy && onStop ? "停止" : "发送"}
        >
          <Icon name={busy ? "stop" : "send"} size={18} />
        </button>
      </div>
    </footer>
  );
}

/* ── Confirm modal ────────────────────────────────────────────────── */

export type ConfirmKind = "end" | "clearHistory" | "clearSummary" | "destroy";

const CONFIRM_COPY: Record<
  ConfirmKind,
  { icon: string; tone: "warn" | "danger"; title: string; body: string; cta: string; ctaClass: string }
> = {
  end: {
    icon: "lock",
    tone: "warn",
    title: "结束会议?",
    body:
      "房间会切换为只读:不能再发消息或运行命令,但完整历史与审计时间线都会保留,链接仍可查看。",
    cta: "结束会议",
    ctaClass: "btn-warn",
  },
  clearHistory: {
    icon: "trash",
    tone: "danger",
    title: "清空历史?",
    body:
      "聊天消息、活动日志和滚动摘要会被删除且无法恢复。房间、成员、权限配置和链接会保留。",
    cta: "清空历史",
    ctaClass: "btn-danger-solid",
  },
  clearSummary: {
    icon: "trash",
    tone: "danger",
    title: "清空摘要?",
    body:
      "当前滚动摘要会被删除且无法恢复。聊天历史会保留，后续新消息达到条件后可重新生成摘要。",
    cta: "清空摘要",
    ctaClass: "btn-danger-solid",
  },
  destroy: {
    icon: "alert",
    tone: "danger",
    title: "永久销毁房间?",
    body:
      "房间历史、审计记录和所有在线连接都会被永久删除且无法恢复。所有参与者会被立即断开。",
    cta: "永久销毁",
    ctaClass: "btn-danger-solid",
  },
};

export function ConfirmModal({
  kind,
  onCancel,
  onConfirm,
}: {
  kind: ConfirmKind;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  const c = CONFIRM_COPY[kind];
  useEffect(() => {
    function onKey(e: globalThis.KeyboardEvent) {
      if (e.key === "Escape") onCancel();
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onCancel]);
  return (
    <div className="modal-scrim" onClick={onCancel}>
      <div className={`modal modal-${c.tone}`} onClick={(e) => e.stopPropagation()}>
        <span className={`modal-icon modal-icon-${c.tone}`}>
          <Icon name={c.icon} size={20} />
        </span>
        <h3>{c.title}</h3>
        <p>{c.body}</p>
        <div className="modal-actions">
          <button type="button" className="btn btn-ghost" onClick={onCancel}>
            取消
          </button>
          <button type="button" className={`btn ${c.ctaClass}`} onClick={onConfirm}>
            {c.cta}
          </button>
        </div>
      </div>
    </div>
  );
}

/* ── Header sign-in pill / user menu wrapper ──────────────────────── */

export function HeaderAuthSlot() {
  const { me, signOut } = useAuth();
  if (!me.auth_enabled) return null;
  if (!me.authenticated) {
    return <SignInPill loginURL={me.login_url} provider={me.auth_provider} />;
  }
  return <UserMenu user={me.user} onSignOut={signOut} />;
}

/* ── Guest gate (visitor side of gated rooms) ─────────────────────── */

interface GuestGateProps {
  room: Room;
  status: "idle" | "pending" | "approved" | "denied";
  labelDraft: string;
  setLabelDraft: (v: string) => void;
  onRequest: () => void;
  authEnabled: boolean;
}

export function GuestGate({
  status,
  labelDraft,
  setLabelDraft,
  onRequest,
  authEnabled,
}: GuestGateProps) {
  if (status === "approved") return null;
  return (
    <div className="messages-rail">
      <div className="gate-request">
        <h3>
          <Icon name="shield" size={16} /> 房间需房主批准
        </h3>
        <p>
          这间房间已开启访客审核。请求加入后，房主会在侧边看到你的申请并决定放行；批准前你将处于等待状态。
        </p>
        {status === "denied" && (
          <p style={{ color: "var(--danger)" }}>
            <Icon name="x" size={14} /> 房主拒绝了你的申请。
          </p>
        )}
        {status === "pending" ? (
          <div className="gate-request-actions">
            <Chip tone="warn">
              <Icon name="clock" size={12} /> 等待房主批准…
            </Chip>
          </div>
        ) : (
          <div className="gate-request-actions">
            <input
              value={labelDraft}
              onChange={(e) => setLabelDraft(e.target.value)}
              placeholder={authEnabled ? "可选：自我介绍" : "可选：显示名"}
            />
            <button type="button" className="btn btn-primary" onClick={onRequest}>
              申请加入
            </button>
          </div>
        )}
      </div>
    </div>
  );
}
