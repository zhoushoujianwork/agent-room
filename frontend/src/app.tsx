import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type ChangeEvent,
  type KeyboardEvent,
} from "react";
import type {
  AccessRequest,
  AccessDecisionEvent,
  AdminUsersReport,
  ChatMessage,
  Me,
  Participant,
  Room,
  RoomSummary,
  UserRoom,
} from "./types";
import {
  cleanRoom,
  copyText,
  hasRoomInLocation,
  makeID,
  roomFromLocation,
  wsURL,
} from "./lib";
import {
  createAccessRequest,
  createRoom,
  clearRoomHistory,
  clearRoomSummary,
  decideAccessRequest,
  deleteRoom,
  forgetMyRoom,
  getRoom,
  listMyRooms,
  listAccessRequests,
  listAllRooms,
  loadAdminUsers,
  loadHistory,
  loadHistoryBefore,
  loadParticipants,
  loadSummary,
  patchRoom,
  postMessage,
  rememberRoom,
  uploadAttachment,
} from "./api";
import { AuthProvider, isAdmin as isAdminMe, isOwnerOf, ThemeProvider, useAuth, useTheme } from "./auth";
import { AdminPage, HomePage, MyRoomsPage, type RoomRecord } from "./home";
import { AgentsPage } from "./agents";
import {
  Composer,
  ConfirmModal,
  type ConfirmKind,
  GuestGate,
  HeaderAuthSlot,
  type MentionOption,
  type PendingAttachment,
  RoomHeader,
  RoomSidebar,
  type RoomTab,
} from "./shell";
import {
  CommandRunCard,
  encodePatternList,
  EventItem,
  groupMessages,
  MessageItem,
  parseAttachments,
  PermissionToasts,
  splitPermissions,
  type PermissionReply,
  ThinkingStream,
} from "./messages";
import {
  ActivityPanel,
  AgentsPanel,
  ApprovalsPanel,
  DevTweaksPanel,
  SettingsPanel,
  SummaryPanel,
} from "./panels";
import { Icon } from "./icons";

type AppView = "home" | "room" | "rooms" | "agents" | "admin";

const ROOM_TABS: RoomTab[] = ["chat", "activity", "approvals", "summary", "agents", "settings"];

// Page size for the admin "all rooms" infinite-scroll list.
const ADMIN_ROOMS_PAGE = 50;

function tabFromLocation(): RoomTab {
  const t = new URLSearchParams(window.location.search).get("tab");
  return t && (ROOM_TABS as string[]).includes(t) ? (t as RoomTab) : "chat";
}

// staticViewFromLocation maps non-room paths to their view: /rooms is the
// saved-rooms list, /admin the admin console, anything else lands home.
function staticViewFromLocation(): AppView {
  const path = window.location.pathname.replace(/\/+$/, "");
  if (path === "/rooms") return "rooms";
  if (path === "/agents") return "agents";
  if (path === "/admin") return "admin";
  return "home";
}

function roomPath(room: string, tab?: RoomTab): string {
  const params = new URLSearchParams({ room: cleanRoom(room) });
  // Chat is the default landing tab, so keep its URL clean (no ?tab=chat).
  if (tab && tab !== "chat") params.set("tab", tab);
  return `/?${params.toString()}`;
}

function loginURLForReturn(loginURL: string, returnTo: string): string {
  try {
    const url = new URL(loginURL, window.location.origin);
    url.searchParams.set("state", returnTo);
    return `${url.pathname}${url.search}${url.hash}`;
  } catch {
    return loginURL;
  }
}

function roomFromInput(value: string): string {
  const raw = value.trim();
  if (!raw) return "";
  try {
    const url = new URL(raw);
    const roomParam = url.searchParams.get("room");
    if (roomParam) return cleanRoom(roomParam);
    const pathMatch = url.pathname.match(/^\/rooms\/([^/]+)/);
    if (pathMatch) return cleanRoom(decodeURIComponent(pathMatch[1]));
  } catch {
    // plain id
  }
  return cleanRoom(raw);
}

function loadRoomRecords(): RoomRecord[] {
  try {
    const parsed = JSON.parse(localStorage.getItem("agent-room.room-records") || "[]") as Partial<RoomRecord>[];
    if (!Array.isArray(parsed)) return [];
    return parsed
      .filter((item): item is RoomRecord => Boolean(item.room))
      .map((item) => ({
        room: cleanRoom(item.room),
        createdAt: item.createdAt || item.lastOpenedAt || new Date().toISOString(),
        lastOpenedAt: item.lastOpenedAt || item.createdAt || new Date().toISOString(),
        title: item.title,
        githubURL: typeof item.githubURL === "string" ? item.githubURL : undefined,
      }))
      .sort((a, b) => Date.parse(b.lastOpenedAt) - Date.parse(a.lastOpenedAt))
      .slice(0, 30);
  } catch {
    return [];
  }
}

function saveRoomRecords(records: RoomRecord[]): RoomRecord[] {
  const next = [...records]
    .sort((a, b) => Date.parse(b.lastOpenedAt) - Date.parse(a.lastOpenedAt))
    .slice(0, 30);
  localStorage.setItem("agent-room.room-records", JSON.stringify(next));
  return next;
}

function upsertRoomRecord(
  records: RoomRecord[],
  room: string,
  title?: string | null,
  githubURL?: string | null,
): RoomRecord[] {
  const nextRoom = cleanRoom(room);
  const now = new Date().toISOString();
  const existing = records.find((item) => item.room === nextRoom);
  const nextRecord: RoomRecord = {
    room: nextRoom,
    createdAt: existing?.createdAt || now,
    lastOpenedAt: now,
    title: title ?? existing?.title,
    githubURL: githubURL ?? existing?.githubURL,
  };
  return saveRoomRecords([nextRecord, ...records.filter((item) => item.room !== nextRoom)]);
}

function roomRecordsFromUserRooms(rooms: UserRoom[]): RoomRecord[] {
  return rooms
    .map((room) => ({
      room: cleanRoom(room.room_id),
      createdAt: room.created_at,
      lastOpenedAt: room.last_opened_at || room.created_at,
      title: room.title ?? undefined,
      githubURL: room.github_url ?? undefined,
    }))
    .sort((a, b) => Date.parse(b.lastOpenedAt) - Date.parse(a.lastOpenedAt))
    .slice(0, 30);
}

function roomRecordURL(record: RoomRecord): string {
  return `${window.location.origin}${roomPath(record.room)}`;
}

/* ── Mention helpers (ported from previous main.tsx) ───────────────── */

interface MentionState {
  active: boolean;
  query: string;
  start: number;
  end: number;
  selected: number;
}

function closedMention(): MentionState {
  return { active: false, query: "", start: 0, end: 0, selected: 0 };
}

function activeMentionAtCaret(content: string, caret: number): Pick<MentionState, "query" | "start" | "end"> | null {
  const before = content.slice(0, caret);
  const at = before.lastIndexOf("@");
  if (at < 0) return null;
  if (at > 0 && !/\s/.test(content[at - 1])) return null;
  const query = before.slice(at + 1);
  if (!/^[a-zA-Z0-9_-]{0,64}$/.test(query)) return null;
  return { query, start: at, end: caret };
}

function mentionEndFromContent(content: string, start: number): number {
  if (start < 0 || content[start] !== "@") return start;
  let end = start + 1;
  while (end < content.length && /[a-zA-Z0-9_-]/.test(content[end])) end += 1;
  return end;
}

function buildMentionOptions(participants: Participant[], query: string, viewerID: string): MentionOption[] {
  const normalized = query.trim().toLowerCase();
  const options: MentionOption[] = [
    { id: "all", label: "All participants", kind: "room", detail: "Room broadcast" },
  ];
  const seen = new Set(options.map((option) => option.id));
  [...participants]
    .sort((left, right) => {
      if (left.kind !== right.kind) return left.kind === "agent" ? -1 : 1;
      return (left.label || left.id).localeCompare(right.label || right.id);
    })
    .forEach((participant) => {
      if (participant.kind !== "agent" && participant.kind !== "user") return;
      if (participant.kind === "user" && participant.id === viewerID) return;
      if (!/^[a-zA-Z0-9_-]{1,64}$/.test(participant.id) || seen.has(participant.id)) return;
      seen.add(participant.id);
      options.push({
        id: participant.id,
        label: participant.label || participant.id,
        kind: participant.kind,
        detail:
          participant.kind === "user"
            ? participant.metadata?.principal_email || "online member"
            : participant.metadata?.capabilities || participant.metadata?.provider || "online",
      });
    });
  return options
    .filter((option) => {
      if (!normalized) return true;
      return [option.id, option.label, option.detail].some((value) =>
        value.toLowerCase().includes(normalized),
      );
    })
    .slice(0, 8);
}

function escapeRegExp(value: string): string {
  return value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

function messagePreview(message: ChatMessage): string {
  const text = (message.content || "").trim();
  if (text) return text.length > 80 ? `${text.slice(0, 80)}…` : text;
  const attachments = parseAttachments(message);
  if (attachments.length > 0) return attachments.length === 1 ? "[图片]" : `[${attachments.length} 张图片]`;
  return "[消息]";
}

function mentionsViewer(message: ChatMessage, viewerID: string, me: Me): boolean {
  if (message.type !== "chat" || message.sender_id === viewerID) return false;
  const candidates = new Set<string>([viewerID]);
  if (me.authenticated) {
    candidates.add(me.user.login);
    if (me.user.name) candidates.add(me.user.name);
  }
  for (const raw of candidates) {
    const value = raw.trim();
    if (!value) continue;
    const pattern = new RegExp(`(^|\\s)@${escapeRegExp(value)}(?=\\s|$|[,:;.!?，。！？、])`, "i");
    if (pattern.test(message.content || "")) return true;
  }
  return false;
}

function extractMentionTarget(text: string): string | null {
  const matches = text.matchAll(/(?:^|\s)@([a-zA-Z0-9_-]{1,64})(?=\s|$|[,:;.!?])/g);
  for (const match of matches) {
    const target = match[1].trim();
    if (!target || target.toLowerCase() === "all" || target.toLowerCase() === "room") continue;
    return target;
  }
  return null;
}

/* ── Inner app (consumes auth + theme contexts) ────────────────────── */

function InnerApp() {
  const { me, loading: authLoading } = useAuth();
  const { theme } = useTheme();
  const dark = theme !== "paper";

  const initialRoom = useMemo(() => (hasRoomInLocation() ? roomFromLocation() : ""), []);
  const [appView, setAppView] = useState<AppView>(() =>
    initialRoom ? "room" : staticViewFromLocation(),
  );
  const [roomID, setRoomID] = useState<string>(initialRoom);
  const [roomDraft, setRoomDraft] = useState<string>("");
  const [room, setRoom] = useState<Room | null>(null);

  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [participants, setParticipants] = useState<Participant[]>([]);
  const [connected, setConnected] = useState(false);
  const [roomRecords, setRoomRecords] = useState<RoomRecord[]>(() => loadRoomRecords());
  const [auditRoom, setAuditRoom] = useState<string | null>(null);
  // Admin "all rooms" pagination: pages of ADMIN_ROOMS_PAGE accumulate into
  // adminRooms; a short page flips adminHasMore off, adminLoadingMore guards
  // against overlapping fetches from the scroll sentinel.
  const [adminRooms, setAdminRooms] = useState<Room[] | null>(null);
  const [adminUsers, setAdminUsers] = useState<AdminUsersReport | null>(null);
  const [adminHasMore, setAdminHasMore] = useState(false);
  const [adminLoadingMore, setAdminLoadingMore] = useState(false);

  // Backfill (scroll-up history) state. hasMore is false once a page comes
  // back short, marking the top of the room; loadingMore guards against
  // overlapping fetches while one page is in flight.
  const [loadingMore, setLoadingMore] = useState(false);
  const [hasMore, setHasMore] = useState(true);

  const [content, setContent] = useState("");
  // 待发送的图片附件:已上传到 relay(字节在平台侧)、随下一条消息的
  // metadata.attachments 引用发出。uploadingCount 驱动缩略图条的上传中占位。
  const [pendingAttachments, setPendingAttachments] = useState<PendingAttachment[]>([]);
  const [uploadingCount, setUploadingCount] = useState(0);
  const [replyTo, setReplyTo] = useState<ChatMessage | null>(null);
  const [dismissedMentions, setDismissedMentions] = useState<Set<string>>(() => new Set());
  const [mention, setMention] = useState<MentionState>(closedMention());
  const [creatingRoom, setCreatingRoom] = useState(false);
  const [toast, setToast] = useState("");
  const [tab, setTab] = useState<RoomTab>(() => (initialRoom ? tabFromLocation() : "chat"));
  const [confirm, setConfirm] = useState<ConfirmKind | null>(null);

  const [summary, setSummary] = useState<RoomSummary | null>(null);
  const [summaryLoading, setSummaryLoading] = useState(false);

  const [accessRequests, setAccessRequests] = useState<AccessRequest[]>([]);
  const [guestStatus, setGuestStatus] = useState<"idle" | "pending" | "approved" | "denied">("idle");
  const [guestLabel, setGuestLabel] = useState("");
  const [guestRequestID, setGuestRequestID] = useState<string | null>(null);

  const [awaitingReply, setAwaitingReply] = useState(false);

  // 输入框历史上翻(shell 风格):↑ 召回自己发过的消息,↓ 翻回较新一条直至还原
  // 未发送的草稿。histIdx=null 表示不在浏览态;histStash 暂存进入浏览态前的草稿。
  const [histIdx, setHistIdx] = useState<number | null>(null);
  const histStash = useRef("");

  const wsRef = useRef<WebSocket | null>(null);
  const textareaRef = useRef<HTMLTextAreaElement | null>(null);
  const scrollRef = useRef<HTMLDivElement | null>(null);
  // Backfill bookkeeping kept in refs so the scroll handler reads fresh values
  // without re-subscribing. pendingPrepend records the pre-prepend scrollHeight
  // so a useLayoutEffect can restore the viewport after older messages mount
  // (otherwise the new content shoves the view downward by its own height).
  const loadingMoreRef = useRef(false);
  const hasMoreRef = useRef(true);
  const pendingPrependRef = useRef<number | null>(null);
  // pinnedToBottom tracks whether the user is reading the latest messages.
  // We only auto-scroll to the bottom on new messages while pinned, so a user
  // who has scrolled up to read history isn't yanked back down.
  const pinnedToBottomRef = useRef(true);
  // The agent id we're currently waiting on a reply from, plus a safety timer
  // so a silent/failed agent never leaves the composer locked forever.
  const awaitingTargetRef = useRef<string | null>(null);
  const awaitTimerRef = useRef<number | undefined>(undefined);

  const viewerID = useMemo(() => {
    if (me.authenticated) return me.user.login;
    const existing = localStorage.getItem("agent-room.viewer-id");
    if (existing) return existing;
    const next = makeID("viewer");
    localStorage.setItem("agent-room.viewer-id", next);
    return next;
  }, [me]);

  const wsIdentity = useMemo(() => {
    if (me.authenticated) {
      return {
        connectionID: viewerID,
        connectionLabel: me.user.name || me.user.login,
        principalID: me.user.login,
        principalLabel: me.user.login,
        principalEmail: me.user.email,
        principalName: me.user.name,
        audit: auditRoom === roomID,
      };
    }
    return {
      connectionID: viewerID,
      connectionLabel: viewerID,
      principalID: viewerID,
      principalLabel: viewerID,
    };
  }, [auditRoom, me, roomID, viewerID]);

  const admin = useMemo(() => isAdminMe(me), [me]);
  const isOwner = useMemo(() => isOwnerOf(me, room?.owner ?? null), [me, room?.owner]);
  // canManage folds admin into owner: an admin manages and enters every room
  // as if they owned it (settings, access requests, gated bypass).
  const canManage = isOwner || admin;
  // 匿名房间(owner==null:匿名建房 / relay 未配 auth)没有任何人 canManage,
  // 会让权限审批卡片对全员锁死、agent 的 RequestPermission 阻塞至超时。
  // 与现有安全模型一致——房间 URL(24-hex 随机 token)本身即访问凭证,能进房即已被信任——
  // 故 owner 为空时放开审批给房间内任意成员;有 owner 的房间维持 owner/admin 才能审批。
  const canApprove = useMemo(
    () => canManage || (room?.owner ?? null) == null,
    [canManage, room?.owner],
  );
  // 审批决议来自房间内持久化的 control(permission_reply) 消息,刷新/多标签页
  // 都能据此恢复"已处理"态,而非只靠点击者的本地组件状态。待审批的进右侧
  // PermissionToasts;完整记录(含永久放行规则)在 Approvals 页签查看。
  const perms = useMemo(() => splitPermissions(messages), [messages]);
  // 自己发过的聊天消息(时间序,相邻去重),供输入框 ↑/↓ 历史召回。来源是房间
  // 持久化消息,刷新后依然可上翻。
  const myHistory = useMemo(() => {
    const out: string[] = [];
    for (const msg of messages) {
      if (msg.type !== "chat" || msg.sender_id !== viewerID) continue;
      const text = (msg.content || "").trim();
      if (!text || out[out.length - 1] === text) continue;
      out.push(text);
    }
    return out;
  }, [messages, viewerID]);
  const gatedBlocked = useMemo(() => {
    if (!room) return false;
    if (!room.gated) return false;
    if (canManage) return false;
    return guestStatus !== "approved";
  }, [room, canManage, guestStatus]);

  function showToast(text: string) {
    setToast(text);
    window.setTimeout(() => setToast(""), 1600);
  }

  const hasSignedIdentity = me.authenticated;

  function requireSignedIdentity(returnTo = "/"): boolean {
    if (hasSignedIdentity) return true;
    if (me.auth_enabled) {
      window.location.href = loginURLForReturn(me.login_url, returnTo);
    } else {
      showToast("需要先登录才能进入房间");
    }
    return false;
  }

  // Direct deep-link to /agents while signed out: send the visitor to the
  // login flow with a return-to so they land back on the page (acceptance #5).
  // Runs before the generic bounce-home guard below so the URL is preserved.
  useEffect(() => {
    if (authLoading || hasSignedIdentity || appView !== "agents") return;
    if (me.auth_enabled) {
      window.location.href = loginURLForReturn(me.login_url, "/agents");
    }
  }, [appView, authLoading, hasSignedIdentity, me]);

  useEffect(() => {
    if (authLoading || hasSignedIdentity || appView === "home") return;
    setRoomID("");
    setMessages([]);
    setParticipants([]);
    setRoom(null);
    setAppView("home");
    if (window.location.pathname !== "/" || window.location.search !== "") {
      window.history.replaceState(null, "", "/");
    }
  }, [appView, authLoading, hasSignedIdentity]);

  useEffect(() => {
    if (authLoading || !me.authenticated) return;
    if (appView !== "rooms" && appView !== "room") return;
    let cancelled = false;
    listMyRooms()
      .then((rooms) => {
        if (cancelled) return;
        setRoomRecords(saveRoomRecords(roomRecordsFromUserRooms(rooms)));
      })
      .catch(() => undefined);
    return () => {
      cancelled = true;
    };
  }, [appView, authLoading, me]);

  /* ── Hydrate room metadata ─────────────────────────────────────── */
  useEffect(() => {
    if (appView !== "room" || !roomID) return;
    let cancelled = false;
    getRoom(roomID).then((next) => {
      if (cancelled) return;
      if (next) {
        setRoom(next);
        setGuestStatus(next.gated ? "idle" : "approved");
      } else {
        // Server has no record; treat as anonymous room with this id.
        setRoom({
          room_id: roomID,
          owner: null,
          title: null,
          github_url: null,
          gated: false,
          ended: false,
          created_at: new Date().toISOString(),
        });
        setGuestStatus("approved");
      }
    });
    return () => {
      cancelled = true;
    };
  }, [appView, roomID]);

  useEffect(() => {
    if (appView !== "room" || !roomID || !room || !me.authenticated) return;
    if (room.gated && !canManage && guestStatus !== "approved") return;
    rememberRoom(roomID).catch(() => undefined);
  }, [appView, canManage, guestStatus, me, room, roomID]);

  /* ── URL sync ─────────────────────────────────────────────────── */
  useEffect(() => {
    const syncRoute = () => {
      if (hasRoomInLocation()) {
        const next = roomFromLocation();
        setRoomID(next);
        setTab(tabFromLocation());
        setAppView("room");
        return;
      }
      setAppView(staticViewFromLocation());
    };
    window.addEventListener("popstate", syncRoute);
    return () => window.removeEventListener("popstate", syncRoute);
  }, []);

  useEffect(() => {
    if (!roomID) return;
    if (auditRoom !== roomID) {
      localStorage.setItem("agent-room.room", roomID);
    }
    if (appView === "room") {
      if (auditRoom !== roomID) {
        setRoomRecords((records) =>
          upsertRoomRecord(records, roomID, room?.title ?? undefined, room?.github_url ?? undefined),
        );
      }
      const desired = roomPath(roomID, tab);
      if (`${window.location.pathname}${window.location.search}` !== desired) {
        window.history.replaceState(null, "", desired);
      }
    }
  }, [appView, auditRoom, roomID, room?.github_url, room?.title, tab]);

  /* ── WebSocket ────────────────────────────────────────────────── */
  useEffect(() => {
    if (appView !== "room" || !roomID || gatedBlocked) return;
    let reconnect: number | undefined;
    let disposed = false;
    const connect = () => {
      if (disposed) return;
      wsRef.current?.close();
      setConnected(false);
      const ws = new WebSocket(wsURL(roomID, wsIdentity));
      wsRef.current = ws;
      ws.addEventListener("open", () => {
        if (!disposed) setConnected(true);
      });
      ws.addEventListener("message", (event) => {
        if (disposed) return;
        try {
          const data = JSON.parse(event.data);
          if (data && data.type === "access_decision") {
            handleAccessDecision(data as AccessDecisionEvent);
            return;
          }
          const msg = data as ChatMessage;
          setMessages((items) => {
            if (msg.id && items.some((item) => item.id === msg.id)) return items;
            const next = [...items, msg];
            // Cap the live tail only while pinned to the bottom. If the user
            // has scrolled up to read backfilled history, trimming the front
            // would discard what they just loaded and jump their viewport, so
            // keep the full list until they return to the bottom.
            return pinnedToBottomRef.current ? next.slice(-300) : next;
          });
          // Unlock the composer once the agent we summoned actually answers
          // (its real reply / command result, not the streaming trace steps),
          // or once its generation is stopped.
          const awaited = awaitingTargetRef.current;
          if (
            awaited &&
            msg.sender_id === awaited &&
            (msg.type === "chat" ||
              msg.type === "command_result" ||
              (msg.type === "trace" && msg.metadata?.phase === "stopped"))
          ) {
            endAwait();
          }
        } catch {
          // ignore malformed
        }
      });
      ws.addEventListener("close", () => {
        if (disposed) return;
        setConnected(false);
        endAwait();
        reconnect = window.setTimeout(connect, 1800);
      });
      ws.addEventListener("error", () => {
        if (!disposed) setConnected(false);
      });
    };
    connect();
    return () => {
      disposed = true;
      if (reconnect) window.clearTimeout(reconnect);
      reconnect = undefined;
      wsRef.current?.close();
      wsRef.current = null;
      setConnected(false);
      endAwait();
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [appView, roomID, wsIdentity, gatedBlocked]);

  /* ── History + participants polling ────────────────────────────── */
  useEffect(() => {
    if (appView !== "room" || !roomID || gatedBlocked) return;
    // Reset paging on (re)entry: assume more history until a load proves
    // otherwise, and start pinned to the bottom so the first paint scrolls down.
    setHasMore(true);
    hasMoreRef.current = true;
    pinnedToBottomRef.current = true;
    pendingPrependRef.current = null;
    loadHistory(roomID)
      .then((items) => {
        setMessages(items);
        // A short first page (< the 240 cap loadHistory requests) means the
        // whole room fits already — nothing older to fetch.
        const full = items.length >= 240;
        setHasMore(full);
        hasMoreRef.current = full;
      })
      .catch(() => showToast("History failed"));
  }, [appView, roomID, gatedBlocked]);

  useEffect(() => {
    if (appView !== "room" || !roomID || gatedBlocked) return;
    const load = () => loadParticipants(roomID).then(setParticipants).catch(() => undefined);
    load();
    const timer = window.setInterval(load, 3000);
    return () => window.clearInterval(timer);
  }, [appView, roomID, gatedBlocked]);

  /* ── Owner access-request list polling ─────────────────────────── */
  useEffect(() => {
    if (appView !== "room" || !roomID) return;
    if (!canManage || !room?.gated) {
      setAccessRequests([]);
      return;
    }
    const load = () => listAccessRequests(roomID).then(setAccessRequests).catch(() => undefined);
    load();
    const timer = window.setInterval(load, 4000);
    return () => window.clearInterval(timer);
  }, [appView, roomID, canManage, room?.gated]);

  /* ── Room summary (load on the Summary tab, then poll) ─────────── */
  const refreshSummary = useCallback(() => {
    if (!roomID) return;
    setSummaryLoading(true);
    loadSummary(roomID)
      .then((next) => {
        if (next) setSummary(next);
      })
      .finally(() => setSummaryLoading(false));
  }, [roomID]);

  useEffect(() => {
    setSummary(null);
  }, [roomID]);

  useEffect(() => {
    if (appView !== "room" || !roomID || gatedBlocked || tab !== "summary") return;
    refreshSummary();
    // The relay regenerates summaries on its own cadence; a slow poll keeps the
    // open tab fresh without hammering the endpoint.
    const timer = window.setInterval(refreshSummary, 20000);
    return () => window.clearInterval(timer);
  }, [appView, roomID, gatedBlocked, tab, refreshSummary]);

  /* ── Scroll management ─────────────────────────────────────────── */
  // Runs synchronously after DOM mutations so the user never sees an
  // intermediate scroll position. Two cases:
  //  • a backfill just prepended older messages → restore the prior distance
  //    from the bottom. That distance is invariant under prepending (and under
  //    removing the loading indicator), both of which happen above the
  //    viewport, so the content the user was reading stays put.
  //  • normal new/changed messages → stick to the bottom only if the user was
  //    already pinned there; otherwise leave their scroll alone.
  useLayoutEffect(() => {
    const el = scrollRef.current;
    if (!el) return;
    const pending = pendingPrependRef.current;
    if (pending !== null) {
      el.scrollTop = el.scrollHeight - pending;
      pendingPrependRef.current = null;
      return;
    }
    if (pinnedToBottomRef.current) {
      el.scrollTop = el.scrollHeight;
    }
  }, [messages, tab]);

  // loadMore fetches the page of history older than the oldest message we
  // hold and prepends it. Guarded by refs so overlapping scroll events don't
  // fire duplicate requests, and short pages flip hasMore off (top of room).
  const loadMore = useCallback(() => {
    const el = scrollRef.current;
    if (!el || loadingMoreRef.current || !hasMoreRef.current) return;
    const oldest = messages[0];
    if (!oldest?.id) return;
    loadingMoreRef.current = true;
    setLoadingMore(true);
    const PAGE = 120;
    loadHistoryBefore(roomID, oldest.id, PAGE)
      .then((older) => {
        if (older.length === 0) {
          setHasMore(false);
          hasMoreRef.current = false;
          return;
        }
        if (older.length < PAGE) {
          setHasMore(false);
          hasMoreRef.current = false;
        }
        // Arm the scroll-restore against the height *now* (the prepend hasn't
        // committed yet). We stash (scrollHeight - scrollTop): the layout
        // effect restores scrollTop = newScrollHeight - stashed, which holds
        // the distance from the bottom constant regardless of where in the top
        // zone the user triggered the load, so the viewport stays put.
        const sc = scrollRef.current;
        pendingPrependRef.current = sc ? sc.scrollHeight - sc.scrollTop : null;
        setMessages((items) => {
          const seen = new Set(items.map((m) => m.id));
          const fresh = older.filter((m) => !m.id || !seen.has(m.id));
          if (fresh.length === 0) {
            pendingPrependRef.current = null;
            return items;
          }
          return [...fresh, ...items];
        });
      })
      .catch(() => {
        pendingPrependRef.current = null;
        showToast("加载更早消息失败");
      })
      .finally(() => {
        loadingMoreRef.current = false;
        setLoadingMore(false);
      });
  }, [messages, roomID]);

  // Track whether the user is pinned to the bottom, and trigger backfill as
  // they approach the top. The 80px threshold near the bottom tolerates
  // sub-pixel rounding; the 120px top threshold prefetches before they hit 0.
  function onMessagesScroll() {
    const el = scrollRef.current;
    if (!el) return;
    const distanceFromBottom = el.scrollHeight - el.scrollTop - el.clientHeight;
    pinnedToBottomRef.current = distanceFromBottom < 80;
    if (el.scrollTop < 120) loadMore();
  }

  /* ── Mention bookkeeping ───────────────────────────────────────── */
  const mentionOptions = useMemo(
    () => buildMentionOptions(participants, mention.query, viewerID),
    [participants, mention.query, viewerID],
  );
  useEffect(() => {
    if (!mention.active || mention.selected < mentionOptions.length) return;
    setMention((current) => ({ ...current, selected: 0 }));
  }, [mention.active, mention.selected, mentionOptions.length]);

  const mentionAlerts = useMemo(
    () =>
      messages
        .filter((message) => {
          const key = message.id || `${message.sender_id}:${message.created_at || message.content}`;
          return !dismissedMentions.has(key) && mentionsViewer(message, viewerID, me);
        })
        .slice(-3),
    [dismissedMentions, me, messages, viewerID],
  );

  function dismissMention(message: ChatMessage) {
    const key = message.id || `${message.sender_id}:${message.created_at || message.content}`;
    setDismissedMentions((items) => {
      const next = new Set(items);
      next.add(key);
      return next;
    });
    if (message.id) {
      document.getElementById(`msg-${message.id}`)?.scrollIntoView({ block: "center", behavior: "smooth" });
    }
  }

  /* ── Sticky summon target ──────────────────────────────────────── */
  const agents = useMemo(
    () => participants.filter((p) => p.kind === "agent"),
    [participants],
  );

  /* ── Composer handlers ─────────────────────────────────────────── */

  // 附件随房间走:切房间时丢弃别的房间里没发出去的图(字节已在 relay,但
  // 引用只对原房间有效)。
  useEffect(() => {
    setPendingAttachments([]);
    setUploadingCount(0);
    setReplyTo(null);
    setDismissedMentions(new Set());
  }, [roomID]);

  function attachFiles(files: File[]) {
    if (!roomID || room?.ended) return;
    for (const file of files) {
      if (file.size > 5 * 1024 * 1024) {
        showToast(`图片超过 5MB：${file.name}`);
        continue;
      }
      setUploadingCount((n) => n + 1);
      uploadAttachment(roomID, file)
        .then((up) => {
          setPendingAttachments((items) => [
            ...items,
            { id: up.id, mime: up.mime, size: up.size, name: file.name, url: up.url },
          ]);
        })
        .catch((err: Error) => {
          showToast(`图片上传失败：${err.message}`);
        })
        .finally(() => setUploadingCount((n) => Math.max(0, n - 1)));
    }
  }

  function removeAttachment(id: string) {
    setPendingAttachments((items) => items.filter((item) => item.id !== id));
  }

  function syncMention(nextContent: string, caret: number) {
    const next = activeMentionAtCaret(nextContent, caret);
    if (!next) {
      setMention(closedMention());
      return;
    }
    setMention((current) => ({
      active: true,
      query: next.query,
      start: next.start,
      end: next.end,
      selected: current.active && current.start === next.start ? current.selected : 0,
    }));
  }

  function onContentChange(event: ChangeEvent<HTMLTextAreaElement>) {
    const el = event.currentTarget;
    setContent(el.value);
    // 手动编辑即退出历史浏览态:召回的文本就地变成可编辑草稿。
    setHistIdx(null);
    syncMention(el.value, el.selectionStart ?? el.value.length);
  }

  // recallHistory 处理输入框的 shell 风格历史召回。仅在光标无选区且位于首行
  // (↑)/末行(↓)时接管方向键,多行草稿内的正常移动不受影响。返回 true 表示已
  // 处理(调用方需 preventDefault)。
  function recallHistory(el: HTMLTextAreaElement, dir: -1 | 1): boolean {
    if (myHistory.length === 0) return false;
    if (el.selectionStart !== el.selectionEnd) return false;
    const caret = el.selectionStart ?? 0;
    if (dir === -1 && el.value.slice(0, caret).includes("\n")) return false;
    if (dir === 1 && el.value.slice(caret).includes("\n")) return false;

    let next: number | null;
    if (dir === -1) {
      if (histIdx === null) {
        histStash.current = el.value; // 进入浏览态,暂存草稿
        next = myHistory.length - 1;
      } else if (histIdx === 0) {
        return true; // 已是最早一条,吞掉按键防止光标跳动
      } else {
        next = histIdx - 1;
      }
    } else {
      if (histIdx === null) return false; // 不在浏览态,↓ 走默认行为
      next = histIdx + 1 >= myHistory.length ? null : histIdx + 1;
    }

    const text = next === null ? histStash.current : myHistory[next];
    setHistIdx(next);
    setContent(text);
    setMention(closedMention());
    window.requestAnimationFrame(() => {
      const area = textareaRef.current;
      if (area) area.setSelectionRange(text.length, text.length);
    });
    return true;
  }

  function insertMention(option: MentionOption) {
    if (!mention.active) return;
    const insert = `@${option.id} `;
    const end = mentionEndFromContent(content, mention.start);
    const next = `${content.slice(0, mention.start)}${insert}${content.slice(end)}`;
    const caret = mention.start + insert.length;
    setContent(next);
    setMention(closedMention());
    window.requestAnimationFrame(() => {
      textareaRef.current?.focus();
      textareaRef.current?.setSelectionRange(caret, caret);
    });
  }

  function startReply(message: ChatMessage) {
    setReplyTo(message);
    const id = message.sender_id?.trim();
    if (id && id !== viewerID) {
      const mentionText = `@${id} `;
      setContent((current) => {
        const trimmedStart = current.trimStart();
        if (trimmedStart.startsWith(mentionText) || trimmedStart.startsWith(`@${id}\n`)) {
          return current;
        }
        return current.trim().length > 0 ? `${mentionText}${current}` : mentionText;
      });
    }
    window.requestAnimationFrame(() => {
      const area = textareaRef.current;
      area?.focus();
      if (area) area.setSelectionRange(area.value.length, area.value.length);
    });
  }

  function onKeyDown(event: KeyboardEvent<HTMLTextAreaElement>) {
    // While an IME candidate window is open (e.g. typing Chinese), Enter
    // confirms the candidate — it must not send the message.
    if (event.nativeEvent.isComposing) return;
    if (mention.active) {
      if (event.key === "ArrowDown" && mentionOptions.length > 0) {
        event.preventDefault();
        setMention((c) => ({ ...c, selected: (c.selected + 1) % mentionOptions.length }));
        return;
      }
      if (event.key === "ArrowUp" && mentionOptions.length > 0) {
        event.preventDefault();
        setMention((c) => ({
          ...c,
          selected: (c.selected - 1 + mentionOptions.length) % mentionOptions.length,
        }));
        return;
      }
      if ((event.key === "Enter" || event.key === "Tab") && mentionOptions.length > 0) {
        event.preventDefault();
        insertMention(mentionOptions[Math.min(mention.selected, mentionOptions.length - 1)]);
        return;
      }
      if (event.key === "Escape") {
        event.preventDefault();
        setMention(closedMention());
        return;
      }
    }
    if (event.key === "ArrowUp" && recallHistory(event.currentTarget, -1)) {
      event.preventDefault();
      return;
    }
    if (event.key === "ArrowDown" && recallHistory(event.currentTarget, 1)) {
      event.preventDefault();
      return;
    }
    if (event.key === "Enter" && !event.shiftKey) {
      event.preventDefault();
      sendMessage();
    }
  }

  function beginAwait(targetID: string) {
    awaitingTargetRef.current = targetID;
    setAwaitingReply(true);
    if (awaitTimerRef.current) window.clearTimeout(awaitTimerRef.current);
    // Release the lock automatically if the agent never answers (e.g. crashed
    // bridge) so the composer can't get stuck.
    awaitTimerRef.current = window.setTimeout(endAwait, 120_000);
  }

  function endAwait() {
    if (awaitTimerRef.current) window.clearTimeout(awaitTimerRef.current);
    awaitTimerRef.current = undefined;
    awaitingTargetRef.current = null;
    setAwaitingReply(false);
  }

  function sendMessage() {
    // 纯图消息允许空文本,但补一个占位文案:agent 的 ShouldReply 要求非空
    // content,否则带图召唤 agent 会被静默忽略。
    const text = content.trim() || (pendingAttachments.length > 0 ? "[图片]" : "");
    if (!text || !roomID || room?.ended || awaitingReply) return;
    if (uploadingCount > 0) {
      showToast("图片还在上传中…");
      return;
    }
    const targetID = extractMentionTarget(text);
    const hasAgentTarget = Boolean(targetID && agents.some((agent) => agent.id === targetID));
    const turnBudget = hasAgentTarget ? (agents.length > 1 ? 50 : 1) : 0;
    const metadata: Record<string, string> = {
      source: "web",
      connection_id: viewerID,
    };
    if (replyTo) {
      metadata.reply_to = replyTo.id || replyTo.created_at || messagePreview(replyTo);
      metadata.reply_sender = replyTo.sender_id || "";
      metadata.reply_preview = messagePreview(replyTo);
    }
    if (pendingAttachments.length > 0) {
      // 与 Go 侧 models.AttachmentRef 对偶的 JSON 数组;只发引用,字节已在平台。
      metadata.attachments = JSON.stringify(
        pendingAttachments.map((att) => ({
          id: att.id,
          mime: att.mime,
          size: att.size,
          name: att.name,
        })),
      );
    }
    const msg: ChatMessage = {
      room_id: roomID,
      type: "chat",
      sender_id: viewerID,
      sender_kind: "user",
      target_id: targetID || undefined,
      content: text,
      reply_requested: hasAgentTarget,
      turn_budget: turnBudget,
      metadata,
    };
    if (wsRef.current?.readyState === WebSocket.OPEN) {
      wsRef.current.send(JSON.stringify(msg));
    } else {
      postMessage(roomID, msg).catch(() => {
        showToast("Send failed");
        endAwait();
      });
    }
    // Clear the draft immediately and, when we summoned an agent, lock the
    // composer until its reply (or command result) lands.
    setContent("");
    setReplyTo(null);
    setPendingAttachments([]);
    setMention(closedMention());
    setHistIdx(null);
    histStash.current = "";
    if (hasAgentTarget && targetID) beginAwait(targetID);
  }

  // stopGeneration interrupts an in-flight agent reply by sending a control
  // message. replyTo, when known (from the agent's trace stream), pins the
  // stop to that exact generation; omitting it cancels whatever the agent is
  // currently running. The bridge ignores stops that match nothing.
  function stopGeneration(targetID: string, replyTo?: string, commandID?: string) {
    if (!roomID || !targetID) return;
    const metadata: Record<string, string> = {
      operation: "stop",
      source: "web",
    };
    if (replyTo) metadata.reply_to = replyTo;
    if (commandID) metadata.command_id = commandID;
    const msg: ChatMessage = {
      room_id: roomID,
      type: "control",
      sender_id: viewerID,
      sender_kind: "user",
      target_id: targetID,
      content: "stop",
      reply_requested: false,
      turn_budget: 0,
      metadata,
    };
    if (wsRef.current?.readyState === WebSocket.OPEN) {
      wsRef.current.send(JSON.stringify(msg));
    } else {
      postMessage(roomID, msg).catch(() => showToast("Stop failed"));
    }
    if (!commandID || awaitingTargetRef.current === targetID) endAwait();
  }

  // replyPermission 回应一条权限审批请求(phase=permission_request 的 trace)。
  // 它发送一条 control(operation=permission_reply),target 指向发起审批的 agent
  // (即 trace 的 sender_id),让 relay 把决议路由回去。
  function replyPermission(request: ChatMessage, reply: PermissionReply, patterns?: string[]) {
    if (!roomID) return;
    const targetID = request.sender_id;
    const permissionID = request.metadata?.permission_id || "";
    if (!targetID || !permissionID) return;
    const metadata: Record<string, string> = {
      operation: "permission_reply",
      permission_id: permissionID,
      reply,
      source: "web",
    };
    // allow_always 携带审批人点选的放行模式, bridge 校验后记入规则。编码与
    // metadata.always 对偶: 含多行模式时 JSON 数组, 否则换行分隔(兼容旧 bridge)。
    if (reply === "allow_always" && patterns && patterns.length > 0) {
      metadata.patterns = encodePatternList(patterns);
    }
    const msg: ChatMessage = {
      room_id: roomID,
      type: "control",
      sender_id: viewerID,
      sender_kind: "user",
      target_id: targetID,
      content: reply,
      reply_requested: false,
      turn_budget: 0,
      metadata,
    };
    if (wsRef.current?.readyState === WebSocket.OPEN) {
      wsRef.current.send(JSON.stringify(msg));
    } else {
      postMessage(roomID, msg).catch(() => showToast("审批发送失败"));
    }
  }

  /* ── Room actions ──────────────────────────────────────────────── */

  function enterRoom(value?: string, audit = false) {
    const nextRoom = roomFromInput(value ?? roomDraft);
    if (!nextRoom) {
      showToast("Paste a room ID or link");
      return;
    }
    const nextURL = roomPath(nextRoom);
    if (!requireSignedIdentity(nextURL)) return;
    setRoomID(nextRoom);
    setAuditRoom(audit && admin ? nextRoom : null);
    setAppView("room");
    setRoomDraft("");
    setTab("chat");
    setMessages([]);
    setParticipants([]);
    setRoom(null);
    setGuestStatus("idle");
    setGuestRequestID(null);
    if (`${window.location.pathname}${window.location.search}` !== nextURL) {
      window.history.pushState(null, "", nextURL);
    }
  }

  function openRoomInNewTab(value?: string) {
    const nextRoom = roomFromInput(value ?? roomDraft);
    if (!nextRoom) {
      showToast("Paste a room ID or link");
      return;
    }
    const nextURL = roomPath(nextRoom);
    if (!requireSignedIdentity(nextURL)) return;
    const opened = window.open(nextURL, "_blank", "noopener,noreferrer");
    if (!opened) showToast("浏览器阻止了新页面");
  }

  async function createAndEnterRoom() {
    if (!requireSignedIdentity("/")) return;
    if (creatingRoom) return;
    setCreatingRoom(true);
    try {
      const next = await createRoom();
      const nextID = cleanRoom(next.room_id);
      setRoom(next);
      setRoomID(nextID);
      setAuditRoom(null);
      setAppView("room");
      setTab("chat");
      setMessages([]);
      setParticipants([]);
      setGuestStatus("approved");
      setRoomDraft("");
      const nextURL = roomPath(nextID);
      if (`${window.location.pathname}${window.location.search}` !== nextURL) {
        window.history.pushState(null, "", nextURL);
      }
    } catch {
      showToast("Create room failed");
    } finally {
      setCreatingRoom(false);
    }
  }

  function goHome() {
    setAppView("home");
    setAuditRoom(null);
    setRoomID("");
    setMessages([]);
    setParticipants([]);
    setRoom(null);
    window.history.pushState(null, "", "/");
  }

  function openRooms() {
    if (!requireSignedIdentity("/rooms")) return;
    setAppView("rooms");
    setAuditRoom(null);
    setRoomID("");
    setMessages([]);
    setParticipants([]);
    setRoom(null);
    window.history.pushState(null, "", "/rooms");
  }

  function openAgents() {
    if (!requireSignedIdentity("/agents")) return;
    setAppView("agents");
    setAuditRoom(null);
    setRoomID("");
    setMessages([]);
    setParticipants([]);
    setRoom(null);
    window.history.pushState(null, "", "/agents");
  }

  const refreshAdminRooms = useCallback(() => {
    listAllRooms(0, ADMIN_ROOMS_PAGE).then((page) => {
      setAdminRooms(page);
      setAdminHasMore(page.length === ADMIN_ROOMS_PAGE);
    });
  }, []);

  const refreshAdminUsers = useCallback(() => {
    loadAdminUsers().then(setAdminUsers);
  }, []);

  const loadMoreAdminRooms = useCallback(() => {
    if (adminLoadingMore || !adminHasMore || adminRooms === null) return;
    setAdminLoadingMore(true);
    listAllRooms(adminRooms.length, ADMIN_ROOMS_PAGE)
      .then((page) => {
        setAdminRooms((prev) => {
          // De-dupe by id: rooms created between pages shift offsets, which
          // can make a page re-include a room we already have.
          const base = prev ?? [];
          const seen = new Set(base.map((r) => r.room_id));
          return [...base, ...page.filter((r) => !seen.has(r.room_id))];
        });
        setAdminHasMore(page.length === ADMIN_ROOMS_PAGE);
      })
      .finally(() => setAdminLoadingMore(false));
  }, [adminLoadingMore, adminHasMore, adminRooms]);

  function openAdmin() {
    if (!admin) return;
    setAdminRooms(null);
    setAdminUsers(null);
    setAdminHasMore(false);
    setAppView("admin");
    if (window.location.pathname !== "/admin") {
      window.history.pushState(null, "", "/admin");
    }
  }

  // Owns the admin room list: covers both openAdmin (adminRooms reset to
  // null) and direct loads of /admin. Waits for /v1/me; non-admins are
  // bounced back home so the page never renders for them.
  useEffect(() => {
    if (appView !== "admin" || authLoading) return;
    if (!admin) {
      setAppView("home");
      window.history.replaceState(null, "", "/");
      return;
    }
    if (adminRooms === null) refreshAdminRooms();
    if (adminUsers === null) refreshAdminUsers();
  }, [appView, authLoading, admin, adminRooms, adminUsers, refreshAdminRooms, refreshAdminUsers]);

  const roomURL = useMemo(
    () => (roomID ? `${window.location.origin}${roomPath(roomID)}` : ""),
    [roomID],
  );

  function copyRoomURL() {
    if (!roomURL) return;
    copyText(roomURL).then(
      () => showToast("Room address copied"),
      () => showToast("Copy failed"),
    );
  }

  function copyRoomID() {
    if (!roomID) return;
    copyText(roomID).then(
      () => showToast("房间 ID 已复制"),
      () => showToast("复制失败"),
    );
  }

  function copySavedRoomURL(record: RoomRecord) {
    copyText(roomRecordURL(record)).then(
      () => showToast("Room address copied"),
      () => showToast("Copy failed"),
    );
  }

  function forgetRoomRecord(rid: string) {
    setRoomRecords((records) => saveRoomRecords(records.filter((record) => record.room !== rid)));
    if (me.authenticated) {
      forgetMyRoom(rid).catch(() => showToast("本地已移除，云端同步失败"));
    }
    showToast("Room removed from your list");
  }

  async function renameRoom(next: string) {
    if (!roomID || !room) return;
    try {
      const updated = await patchRoom(roomID, { title: next });
      setRoom(updated);
      setRoomRecords((records) =>
        upsertRoomRecord(records, roomID, updated.title ?? undefined, updated.github_url ?? undefined),
      );
      showToast("房间已重命名");
    } catch {
      showToast("重命名失败");
    }
  }

  async function saveRoomGitHubURL(next: string) {
    if (!roomID || !room) return;
    try {
      const updated = await patchRoom(roomID, { github_url: next.trim() });
      setRoom(updated);
      setRoomRecords((records) =>
        upsertRoomRecord(records, roomID, updated.title ?? undefined, updated.github_url ?? undefined),
      );
      showToast(updated.github_url ? "GitHub 链接已保存" : "GitHub 链接已移除");
    } catch {
      showToast("GitHub 链接保存失败");
    }
  }

  async function toggleGated(next: boolean) {
    if (!roomID) return;
    try {
      const updated = await patchRoom(roomID, { gated: next });
      setRoom(updated);
      showToast(next ? "已开启访客审核" : "已关闭访客审核");
    } catch {
      showToast("操作失败");
    }
  }

  async function endRoom() {
    if (!roomID) return;
    try {
      const updated = await patchRoom(roomID, { ended: true });
      setRoom(updated);
      setConfirm(null);
      showToast("房间已结束");
    } catch {
      showToast("操作失败");
    }
  }

  async function destroyRoom() {
    if (!roomID) return;
    try {
      await deleteRoom(roomID);
      forgetRoomRecord(roomID);
      goHome();
      showToast("房间已销毁");
    } catch {
      showToast("销毁失败");
    }
  }

  async function clearHistory() {
    if (!roomID) return;
    try {
      await clearRoomHistory(roomID);
      setMessages([]);
      setSummary(null);
      setReplyTo(null);
      setDismissedMentions(new Set());
      setHasMore(false);
      setConfirm(null);
      endAwait();
      showToast("历史记录已清空");
    } catch {
      showToast("清空历史失败");
    }
  }

  async function clearSummary() {
    if (!roomID) return;
    try {
      await clearRoomSummary(roomID);
      setSummary(null);
      setConfirm(null);
      showToast("摘要已清空");
    } catch {
      showToast("清空摘要失败");
    }
  }

  function confirmAction() {
    if (confirm === "destroy") return destroyRoom();
    if (confirm === "end") return endRoom();
    if (confirm === "clearHistory") return clearHistory();
    if (confirm === "clearSummary") return clearSummary();
  }

  function copyMention(id: string) {
    copyText(`@${id} `).then(
      () => showToast("已复制 @mention"),
      () => showToast("复制失败"),
    );
  }

  /* ── Access requests ───────────────────────────────────────────── */

  async function requestAccess() {
    if (!roomID) return;
    try {
      const req = await createAccessRequest(roomID, guestLabel || undefined);
      setGuestRequestID(req.id);
      setGuestStatus(req.status === "approved" ? "approved" : "pending");
      showToast("已提交申请，等待房主批准");
    } catch {
      showToast("申请失败");
    }
  }

  async function resolveRequest(
    req: AccessRequest,
    decision: "approve" | "deny",
    persistence?: "once" | "persist",
  ) {
    if (!roomID) return;
    try {
      await decideAccessRequest(roomID, req.id, decision, persistence ?? null);
      setAccessRequests((rs) => rs.filter((r) => r.id !== req.id));
      showToast(decision === "approve" ? `已批准 @${req.requester_label}` : `已拒绝 @${req.requester_label}`);
    } catch {
      showToast("操作失败");
    }
  }

  const handleAccessDecision = useCallback(
    (event: AccessDecisionEvent) => {
      if (!guestRequestID || event.request_id !== guestRequestID) return;
      if (event.status === "approved") {
        setGuestStatus("approved");
        showToast("房主已批准，正在加入…");
      } else {
        setGuestStatus("denied");
        showToast("房主拒绝了你的申请");
      }
    },
    [guestRequestID],
  );

  /* ── Render ────────────────────────────────────────────────────── */

  if (authLoading && appView === "home") {
    // Brief flash while /v1/me loads — keep neutral so we don't render a
    // misleading sign-in pill or owner toggles before the answer arrives.
  }

  if (appView === "admin") {
    return (
      <>
        <AdminPage
          rooms={adminRooms}
          users={adminUsers}
          hasMore={adminHasMore}
          loadingMore={adminLoadingMore}
          onLoadMore={loadMoreAdminRooms}
          onEnterRoom={enterRoom}
          onOpenRoomNewTab={openRoomInNewTab}
          onRefresh={refreshAdminRooms}
          onRefreshUsers={refreshAdminUsers}
          onOpenHome={goHome}
          onOpenRooms={openRooms}
          onOpenAgents={openAgents}
        />
        {toast && <div className="toast">{toast}</div>}
      </>
    );
  }

  if (appView === "agents") {
    return (
      <>
        <AgentsPage
          isAdmin={admin}
          onOpenHome={goHome}
          onOpenRooms={openRooms}
          onOpenAdmin={openAdmin}
          onEnterRoom={enterRoom}
          onShowToast={showToast}
        />
        {toast && <div className="toast">{toast}</div>}
      </>
    );
  }

  if (appView === "home") {
    return (
      <>
        <HomePage
          roomDraft={roomDraft}
          creatingRoom={creatingRoom}
          isAdmin={admin}
          onOpenRooms={openRooms}
          onOpenAgents={openAgents}
          onOpenAdmin={openAdmin}
          onEnterRoom={enterRoom}
          onCreateRoom={createAndEnterRoom}
          onRoomDraftChange={setRoomDraft}
          onShowToast={showToast}
        />
        {import.meta.env.DEV && <DevTweaksPanel />}
        {toast && <div className="toast">{toast}</div>}
      </>
    );
  }

  if (appView === "rooms") {
    return (
      <>
        <MyRoomsPage
          roomDraft={roomDraft}
          creatingRoom={creatingRoom}
          roomRecords={roomRecords}
          isAdmin={admin}
          onOpenAgents={openAgents}
          onOpenAdmin={openAdmin}
          onBackHome={goHome}
          onCopySavedRoomURL={copySavedRoomURL}
          onForgetRoomRecord={forgetRoomRecord}
          onEnterRoom={enterRoom}
          onOpenRoomNewTab={openRoomInNewTab}
          onCreateRoom={createAndEnterRoom}
          onRoomDraftChange={setRoomDraft}
        />
        {import.meta.env.DEV && <DevTweaksPanel />}
        {toast && <div className="toast">{toast}</div>}
      </>
    );
  }

  if (!room) {
    return (
      <>
        <div className="home">
          <main className="home-main" style={{ alignItems: "center", justifyContent: "center" }}>
            <div className="kicker">
              <span className="sdot sdot-live sdot-pulse" style={{ width: 8, height: 8 }} /> loading room…
            </div>
          </main>
        </div>
        {import.meta.env.DEV && <DevTweaksPanel />}
        {toast && <div className="toast">{toast}</div>}
      </>
    );
  }

  const busy = participants.some((p) => p.kind === "agent");

  return (
    <>
      <div className="shell">
        <RoomSidebar
          room={room}
          roomID={roomID}
          roomRecords={roomRecords}
          participants={participants}
          accessRequests={accessRequests}
          isOwner={canManage}
          dark={dark}
          onHome={goHome}
          onNewRoom={() => createAndEnterRoom()}
          onSwitchRoom={enterRoom}
          onOpenRoomNewTab={openRoomInNewTab}
          onForgetRoom={forgetRoomRecord}
          onCopyURL={copyRoomURL}
          onCopyID={copyRoomID}
          onResolveRequest={resolveRequest}
        />
        <main className="room-main">
          <RoomHeader
            room={room}
            roomID={roomID}
            tab={tab}
            setTab={setTab}
            isOwner={canManage}
            connected={connected}
            busy={busy}
            onCopyURL={copyRoomURL}
            onRename={renameRoom}
            headerRight={
              <>
                <HeaderAuthSlot />
              </>
            }
          />

          {tab === "chat" && (
            <>
              <section className="messages" ref={scrollRef} onScroll={onMessagesScroll}>
                {gatedBlocked ? (
                  <GuestGate
                    room={room}
                    status={guestStatus}
                    labelDraft={guestLabel}
                    setLabelDraft={setGuestLabel}
                    onRequest={requestAccess}
                    authEnabled={me.auth_enabled}
                  />
                ) : messages.length === 0 ? (
                  <div className="messages-empty">
                    <p>还没有消息。直接发一条，复制 Direct AI 提示，或本地启动 Bridge 加入。</p>
                    <p className="muted">房间链接本身就是访问凭证，请仅向受信任的人分享。</p>
                  </div>
                ) : (
                  <div className="messages-rail">
                    {mentionAlerts.length > 0 && (
                      <div className="mention-alert-stack" aria-live="polite">
                        {mentionAlerts.map((message) => (
                          <button
                            key={message.id || `${message.sender_id}:${message.created_at}`}
                            type="button"
                            className="mention-alert-card"
                            onClick={() => dismissMention(message)}
                          >
                            <span className="mention-alert-spark">
                              <Icon name="at" size={16} />
                            </span>
                            <span>
                              <strong>{message.sender_id || "有人"} @ 了你</strong>
                              <em>{messagePreview(message)}</em>
                            </span>
                            <Icon name="x" size={14} />
                          </button>
                        ))}
                      </div>
                    )}
                    {loadingMore && (
                      <div className="messages-backfill" aria-live="polite">
                        <span className="messages-backfill-spin" /> 加载更早消息…
                      </div>
                    )}
                    {!hasMore && (
                      <div className="messages-top">— 已经到顶部 —</div>
                    )}
                    {groupMessages(messages).map((group, index) => {
                      if (group.kind === "trace") {
                        return (
                          <ThinkingStream
                            key={`stream-${group.key}-${index}`}
                            traces={group.items}
                            runs={group.runs}
                            dark={dark}
                            onStop={stopGeneration}
                          />
                        );
                      }
                      if (group.kind === "command") {
                        return (
                          <CommandRunCard
                            key={`command-${group.key}-${index}`}
                            messages={group.items}
                            dark={dark}
                            onStop={stopGeneration}
                          />
                        );
                      }
                      if (group.kind === "event") {
                        return (
                          <EventItem
                            key={group.items[0].id || index}
                            message={group.items[0]}
                          />
                        );
                      }
                      return (
                        <div id={group.items[0].id ? `msg-${group.items[0].id}` : undefined} key={group.items[0].id || index}>
                          <MessageItem
                            message={group.items[0]}
                            viewerID={viewerID}
                            dark={dark}
                            mentioned={mentionsViewer(group.items[0], viewerID, me)}
                            onReply={startReply}
                          />
                        </div>
                      );
                    })}
                  </div>
                )}
              </section>
              <Composer
                content={content}
                ended={room.ended}
                busy={awaitingReply}
                textareaRef={textareaRef}
                mentionOpen={mention.active}
                mentionSelected={mention.selected}
                mentionOptions={mentionOptions}
                attachments={pendingAttachments}
                uploadingCount={uploadingCount}
                replyTo={replyTo}
                replyPreview={replyTo ? messagePreview(replyTo) : ""}
                onCancelReply={() => setReplyTo(null)}
                onAttachFiles={attachFiles}
                onRemoveAttachment={removeAttachment}
                onContentChange={onContentChange}
                onSend={sendMessage}
                onStop={() => {
                  const t = awaitingTargetRef.current;
                  if (t) stopGeneration(t);
                }}
                onKeyDown={onKeyDown}
                onPickMention={insertMention}
              />
            </>
          )}

          {tab === "activity" && (
            <section className="messages" ref={scrollRef}>
              <ActivityPanel messages={messages} />
            </section>
          )}
          {tab === "approvals" && (
            <section className="messages">
              <ApprovalsPanel messages={messages} />
            </section>
          )}
          {tab === "summary" && (
            <section className="messages">
              <SummaryPanel
                summary={summary}
                loading={summaryLoading}
                onRefresh={refreshSummary}
                canManage={canManage}
                onClear={() => setConfirm("clearSummary")}
              />
            </section>
          )}
          {tab === "agents" && (
            <section className="messages">
              <AgentsPanel
                participants={participants}
                onCopyMention={copyMention}
                roomID={roomID}
                currentLogin={me.authenticated ? me.user.login : null}
              />
            </section>
          )}
          {tab === "settings" && (
            <section className="messages">
              <SettingsPanel
                room={room}
                roomURL={roomURL}
                onCopyURL={copyRoomURL}
                onTitleSave={renameRoom}
                onGitHubSave={saveRoomGitHubURL}
                onToggleGated={toggleGated}
                onEnd={() => setConfirm("end")}
                onClearHistory={() => setConfirm("clearHistory")}
                onDestroy={() => setConfirm("destroy")}
              />
            </section>
          )}
        </main>
      </div>
      {confirm && (
        <ConfirmModal
          kind={confirm}
          onCancel={() => setConfirm(null)}
          onConfirm={confirmAction}
        />
      )}
      {!gatedBlocked && (
        <PermissionToasts
          pending={perms.pending}
          canApprove={canApprove}
          onReply={replyPermission}
        />
      )}
      {import.meta.env.DEV && <DevTweaksPanel />}
      {toast && <div className="toast">{toast}</div>}
    </>
  );
}

export function App() {
  return (
    <ThemeProvider>
      <AuthProvider>
        <InnerApp />
      </AuthProvider>
    </ThemeProvider>
  );
}
