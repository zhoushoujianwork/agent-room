import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { Icon } from "./icons";
import { Avatar, SignInPill, StatusDot, TopBrand } from "./ui";
import { shortRoomID } from "./lib";
import { useAuth } from "./auth";
import type { AdminUser, AdminUsersReport, Room } from "./types";

export interface RoomRecord {
  room: string;
  createdAt: string;
  lastOpenedAt: string;
  title?: string;
  githubURL?: string;
}

interface HomePageProps {
  roomDraft: string;
  creatingRoom: boolean;
  isAdmin: boolean;
  onOpenRooms: () => void;
  onOpenAgents: () => void;
  onOpenAdmin: () => void;
  onCreateRoom: () => void;
  onEnterRoom: (room?: string, audit?: boolean) => void;
  onRoomDraftChange: (value: string) => void;
  onShowToast: (message: string) => void;
}

const homeBanners = [
  {
    image: "/banners/it-windows-agent-support.png",
    alt: "IT 人员通过 AI agent 远程协调处理员工 Windows 环境问题",
    label: "IT support",
    title: "远程处理 Windows 环境问题",
    detail: "把员工电脑、IT agent 和授权记录拉进同一个房间。",
  },
  {
    image: "/banners/ops-group-debug.png",
    alt: "运维人员把基础设施 AI agent 接入群里协助开发人员定位服务日志故障",
    label: "Ops bridge",
    title: "在群里直接 debug 服务日志",
    detail: "开发、运维和基础设施 agent 零距离协作定位故障。",
  },
  {
    image: "/banners/remote-mobile-dispatch.png",
    alt: "异地办公者通过家庭设备或手机 app 调度工作电脑上的 AI agent",
    label: "Remote control",
    title: "异地调度工作电脑上的 agent",
    detail: "在家或手机上唤起工作机 agent，跨设备推进任务。",
  },
] as const;

function formatRecordTime(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "recently";
  return date.toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
}

export function HomePage({
  roomDraft,
  creatingRoom,
  isAdmin,
  onOpenRooms,
  onOpenAgents,
  onOpenAdmin,
  onCreateRoom,
  onEnterRoom,
  onRoomDraftChange,
  onShowToast,
}: HomePageProps) {
  const { me, signOut } = useAuth();
  const [activeBanner, setActiveBanner] = useState(0);
  const banner = homeBanners[activeBanner];

  useEffect(() => {
    const timer = window.setInterval(() => {
      setActiveBanner((current) => (current + 1) % homeBanners.length);
    }, 6000);
    return () => window.clearInterval(timer);
  }, []);

  const right =
    me.auth_enabled && me.authenticated ? (
      <div className="home-nav-actions">
        <PageNav
          current="home"
          isAdmin={isAdmin}
          onOpenHome={() => {}}
          onOpenRooms={onOpenRooms}
          onOpenAgents={onOpenAgents}
          onOpenAdmin={onOpenAdmin}
        />
        <UserMenu user={me.user} isAdmin={isAdmin} onSignOut={signOut} />
      </div>
    ) : me.auth_enabled ? (
      <SignInPill loginURL={me.login_url} provider={me.auth_provider} />
    ) : null;

  function submit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    onEnterRoom(roomDraft);
  }

  return (
    <div className="home">
      <TopBrand right={right} />
      <main className="home-main">
        <section className="home-landing" aria-label="Agent Room 首页">
          <div className="home-hero">
            <span className="kicker">
              <StatusDot tone="live" pulse /> live agent workspace
            </span>
            <h1>
              开个房间，<br />让异处的 AI Agent<br />自己协作～
            </h1>
            <p>
              Agent Room 把隔离运行的 Claude CLI、执行器和操作者拉进同一个实时房间。
              对话、@召唤、授权执行和审计记录都在一处完成。
            </p>
            <div className="home-proof" aria-label="核心能力">
              <span>
                <Icon name="shieldCheck" size={14} /> 授权执行
              </span>
              <span>
                <Icon name="at" size={14} /> @召唤 Agent
              </span>
              <span>
                <Icon name="activity" size={14} /> 全程审计
              </span>
            </div>
          </div>

          <aside className="home-visual-card" aria-label="Agent Room 主要使用场景">
            <div className="home-banner-stage">
              <img src={banner.image} alt={banner.alt} />
            </div>
            <div className="home-visual-caption">
              <span className="home-visual-live">
                <StatusDot tone="live" size={7} /> {banner.label}
              </span>
              <strong>{banner.title}</strong>
              <span>{banner.detail}</span>
            </div>
            <div className="home-banner-dots" aria-label="切换场景图">
              {homeBanners.map((item, index) => (
                <button
                  key={item.image}
                  type="button"
                  className={index === activeBanner ? "on" : undefined}
                  onClick={() => setActiveBanner(index)}
                  aria-label={`查看场景：${item.title}`}
                  aria-current={index === activeBanner ? "true" : undefined}
                />
              ))}
            </div>
          </aside>
        </section>

        <section className="home-action-row" aria-label="Create or join a room">
          <div className="home-card">
            <div className="home-card-head">
              <div>
                <h2>立刻开始</h2>
                <p>新建房间，或者加入别人发来的房间链接。</p>
              </div>
              <Icon name="branch" size={22} />
            </div>
            <button
              className="btn btn-primary btn-lg"
              type="button"
              onClick={onCreateRoom}
              disabled={creatingRoom}
            >
              {creatingRoom ? (
                "正在创建…"
              ) : (
                <>
                  <Icon name="plus" size={17} /> 创建房间
                </>
              )}
            </button>
            <div className="home-or">
              <span>或加入已有房间</span>
            </div>
            <form className="home-join" onSubmit={submit}>
              <input
                value={roomDraft}
                onChange={(event) => onRoomDraftChange(event.target.value)}
                placeholder="粘贴房间 ID 或完整链接"
                aria-label="加入已有房间"
                autoComplete="off"
                spellCheck={false}
              />
              <button className="btn" type="submit">
                加入
              </button>
            </form>
            <p className="home-fine">
              <Icon name="lock" size={12} /> 房间链接即访问凭证 · 96 位随机 ID · 仅向受信任的人分享
            </p>
          </div>

          <div className="home-principles" aria-label="Agent Room 安全原则">
            <div>
              <Icon name="cpu" size={18} />
              <strong>机器隔离</strong>
              <span>每个 agent 仍在本机运行，不共享 shell 或密钥。</span>
            </div>
            <div>
              <Icon name="shieldCheck" size={18} />
              <strong>房内授权</strong>
              <span>执行命令前先经过房间上下文和权限确认。</span>
            </div>
            <div>
              <Icon name="clock" size={18} />
              <strong>记录可追溯</strong>
              <span>消息、命令和结果沉淀为完整协作时间线。</span>
            </div>
          </div>
        </section>

        <QuickStart onShowToast={onShowToast} />
      </main>
    </div>
  );
}

interface MyRoomsPageProps {
  roomDraft: string;
  creatingRoom: boolean;
  roomRecords: RoomRecord[];
  isAdmin: boolean;
  onOpenAgents: () => void;
  onOpenAdmin: () => void;
  onBackHome: () => void;
  onCopySavedRoomURL: (record: RoomRecord) => void;
  onForgetRoomRecord: (roomID: string) => void;
  onCreateRoom: () => void;
  onEnterRoom: (room?: string) => void;
  onOpenRoomNewTab: (room?: string) => void;
  onRoomDraftChange: (value: string) => void;
}

export function MyRoomsPage({
  roomDraft,
  creatingRoom,
  roomRecords,
  isAdmin,
  onOpenAgents,
  onOpenAdmin,
  onBackHome,
  onCopySavedRoomURL,
  onForgetRoomRecord,
  onCreateRoom,
  onEnterRoom,
  onOpenRoomNewTab,
  onRoomDraftChange,
}: MyRoomsPageProps) {
  const { me, signOut } = useAuth();

  function submit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    onEnterRoom(roomDraft);
  }

  const right =
    me.auth_enabled && me.authenticated ? (
      <div className="home-nav-actions">
        <PageNav
          current="rooms"
          isAdmin={isAdmin}
          onOpenHome={onBackHome}
          onOpenRooms={() => {}}
          onOpenAgents={onOpenAgents}
          onOpenAdmin={onOpenAdmin}
        />
        <UserMenu user={me.user} isAdmin={isAdmin} onSignOut={signOut} />
      </div>
    ) : null;

  return (
    <div className="home rooms-page">
      <TopBrand right={right} />
      <main className="rooms-main">
        <section className="rooms-hero" aria-label="我的房间概览">
          <div>
            <span className="kicker">
              <Icon name="home" size={14} /> signed-in workspace
            </span>
            <h1>我的房间</h1>
            <p>这里放你最近创建或加入过的房间。首页保持干净，房间管理和切换集中在这个页面。</p>
          </div>
          <button
            className="btn btn-primary btn-lg"
            type="button"
            onClick={onCreateRoom}
            disabled={creatingRoom}
          >
            {creatingRoom ? "正在创建…" : (
              <>
                <Icon name="plus" size={17} /> 新建房间
              </>
            )}
          </button>
        </section>

        <section className="rooms-workbench" aria-label="房间管理">
          <div className="rooms-join-card">
            <div className="home-card-head">
              <div>
                <h2>加入房间</h2>
                <p>粘贴房间 ID 或完整链接，进入后会自动记到这里。</p>
              </div>
              <Icon name="link" size={21} />
            </div>
            <form className="home-join" onSubmit={submit}>
              <input
                value={roomDraft}
                onChange={(event) => onRoomDraftChange(event.target.value)}
                placeholder="粘贴房间 ID 或完整链接"
                aria-label="加入已有房间"
                autoComplete="off"
                spellCheck={false}
              />
              <button className="btn" type="submit">
                加入
              </button>
            </form>
          </div>

          <section className="home-rooms rooms-list-card" aria-label="My rooms">
            <div className="home-rooms-head">
              <span>最近房间</span>
              <span className="count">{roomRecords.length}</span>
            </div>
            {roomRecords.length === 0 ? (
              <div className="rooms-empty">
                <Icon name="hash" size={18} />
                <strong>还没有房间记录</strong>
                <span>创建房间或加入别人分享的房间后，会出现在这里。</span>
              </div>
            ) : (
              <ul>
                {roomRecords.map((record) => (
                  <li key={record.room}>
                    <button
                      className="room-link"
                      type="button"
                      onClick={() => onEnterRoom(record.room)}
                      title={record.room}
                    >
                      <span className="room-hash">
                        <Icon name="hash" size={14} />
                      </span>
                      <span className="room-link-text">
                        <strong>{record.title || shortRoomID(record.room)}</strong>
                        <span>{formatRecordTime(record.lastOpenedAt)}</span>
                      </span>
                      <Icon name="chevronRight" size={15} />
                    </button>
                    {record.githubURL && (
                      <a
                        className="icon-btn icon-btn-ghost room-github-link"
                        href={record.githubURL}
                        target="_blank"
                        rel="noreferrer"
                        title="打开 GitHub"
                        aria-label="打开 GitHub"
                      >
                        <Icon name="github" size={15} />
                      </a>
                    )}
                    <button
                      className="icon-btn icon-btn-ghost"
                      type="button"
                      onClick={() => onOpenRoomNewTab(record.room)}
                      title="新开页面打开"
                      aria-label="新开页面打开"
                    >
                      <Icon name="arrowUpRight" size={15} />
                    </button>
                    <button
                      className="icon-btn icon-btn-ghost"
                      type="button"
                      onClick={() => onCopySavedRoomURL(record)}
                      title="复制链接"
                      aria-label="复制链接"
                    >
                      <Icon name="copy" size={15} />
                    </button>
                    <button
                      className="icon-btn icon-btn-ghost"
                      type="button"
                      onClick={() => onForgetRoomRecord(record.room)}
                      title="移除"
                      aria-label="移除"
                    >
                      <Icon name="x" size={15} />
                    </button>
                  </li>
                ))}
              </ul>
            )}
          </section>
        </section>
      </main>
    </div>
  );
}

/* ── PageNav: top-right page switcher for signed-in users ──────────── */

export type PageNavCurrent = "home" | "rooms" | "agents" | "admin";

export function PageNav({
  current,
  isAdmin,
  onOpenHome,
  onOpenRooms,
  onOpenAgents,
  onOpenAdmin,
}: {
  current: PageNavCurrent;
  isAdmin: boolean;
  onOpenHome: () => void;
  onOpenRooms: () => void;
  onOpenAgents: () => void;
  onOpenAdmin: () => void;
}) {
  const items: { id: PageNavCurrent; label: string; icon: string; open: () => void }[] = [
    { id: "home", label: "首页", icon: "home", open: onOpenHome },
    { id: "rooms", label: "我的房间", icon: "hash", open: onOpenRooms },
    { id: "agents", label: "我的 Agents", icon: "bot", open: onOpenAgents },
  ];
  if (isAdmin) {
    items.push({ id: "admin", label: "管理后台", icon: "shieldCheck", open: onOpenAdmin });
  }
  return (
    <nav className="page-nav" aria-label="页面切换">
      {items.map((item) => (
        <button
          key={item.id}
          type="button"
          className={item.id === current ? "page-nav-link on" : "page-nav-link"}
          aria-current={item.id === current ? "page" : undefined}
          onClick={() => {
            if (item.id !== current) item.open();
          }}
        >
          <Icon name={item.icon} size={15} />
          <span>{item.label}</span>
        </button>
      ))}
    </nav>
  );
}

export function UserMenu({
  user,
  isAdmin = false,
  onSignOut,
}: {
  user: { login: string; name: string; email: string; avatar_url: string };
  isAdmin?: boolean;
  onSignOut: () => void;
}) {
  const [open, setOpen] = useState(false);
  return (
    <div className="usermenu">
      <button
        type="button"
        className="usermenu-trigger"
        onClick={() => setOpen((v) => !v)}
        aria-haspopup="menu"
        aria-expanded={open}
      >
        <Avatar
          person={{ id: user.login, label: user.login, avatar_url: user.avatar_url, kind: "user" }}
          dark={false}
          size={28}
        />
        <span className="usermenu-handle">@{user.login}</span>
        <Icon name="chevronDown" size={14} />
      </button>
      {open && (
        <div className="usermenu-pop" role="menu">
          <div className="usermenu-id">
            <Avatar
              person={{ id: user.login, label: user.login, avatar_url: user.avatar_url, kind: "user" }}
              dark={false}
              size={34}
            />
            <div className="usermenu-id-text">
              <strong>{user.name || user.login}</strong>
              <span>
                @{user.login}
                {isAdmin ? " · 管理员" : ""}
              </span>
            </div>
          </div>
          <button
            type="button"
            className="usermenu-item"
            onClick={() => {
              setOpen(false);
              onSignOut();
            }}
          >
            <Icon name="arrowUpRight" size={15} /> 退出登录
          </button>
        </div>
      )}
    </div>
  );
}

/* ── Admin: all rooms ──────────────────────────────────────────────── */

interface AdminPageProps {
  rooms: Room[] | null;
  users: AdminUsersReport | null;
  hasMore: boolean;
  loadingMore: boolean;
  onLoadMore: () => void;
  onEnterRoom: (room?: string, audit?: boolean) => void;
  onOpenRoomNewTab: (room?: string) => void;
  onRefresh: () => void;
  onRefreshUsers: () => void;
  onOpenHome: () => void;
  onOpenRooms: () => void;
  onOpenAgents: () => void;
}

export function AdminPage({
  rooms,
  users,
  hasMore,
  loadingMore,
  onLoadMore,
  onEnterRoom,
  onOpenRoomNewTab,
  onRefresh,
  onRefreshUsers,
  onOpenHome,
  onOpenRooms,
  onOpenAgents,
}: AdminPageProps) {
  const { me, signOut } = useAuth();
  const [panel, setPanel] = useState<"rooms" | "users">("users");
  // Infinite scroll: when the sentinel under the list scrolls into view,
  // fetch the next page. onLoadMore self-guards against overlapping calls.
  const sentinelRef = useRef<HTMLDivElement | null>(null);
  useEffect(() => {
    const el = sentinelRef.current;
    if (!el || !hasMore) return;
    const observer = new IntersectionObserver(
      (entries) => {
        if (entries.some((e) => e.isIntersecting)) onLoadMore();
      },
      { rootMargin: "240px" },
    );
    observer.observe(el);
    return () => observer.disconnect();
  }, [hasMore, onLoadMore]);

  const right = (
    <div className="home-nav-actions">
      <PageNav
        current="admin"
        isAdmin
        onOpenHome={onOpenHome}
        onOpenRooms={onOpenRooms}
        onOpenAgents={onOpenAgents}
        onOpenAdmin={() => {}}
      />
      {me.auth_enabled && me.authenticated && (
        <UserMenu user={me.user} isAdmin onSignOut={signOut} />
      )}
    </div>
  );
  return (
    <div className="home">
      <TopBrand right={right} />
      <main className="home-main">
        <div className="home-hero">
          <span className="kicker">
            <Icon name="shieldCheck" size={14} /> 管理后台
          </span>
          <h1>管理员看板</h1>
          <p>
            作为管理员，你能看到全站房间与用户动态：谁最近登录、谁还在线，以及用户增长趋势。
          </p>
        </div>

        <div className="admin-tabs" role="tablist" aria-label="Admin sections">
          <button
            type="button"
            className={panel === "users" ? "on" : ""}
            onClick={() => setPanel("users")}
            role="tab"
            aria-selected={panel === "users"}
          >
            <Icon name="users" size={15} /> 用户
          </button>
          <button
            type="button"
            className={panel === "rooms" ? "on" : ""}
            onClick={() => setPanel("rooms")}
            role="tab"
            aria-selected={panel === "rooms"}
          >
            <Icon name="hash" size={15} /> 房间
          </button>
        </div>

        {panel === "users" ? (
          <AdminUsersPanel report={users} onRefresh={onRefreshUsers} />
        ) : (
          <section className="home-rooms" aria-label="All rooms">
            <div className="home-rooms-head">
              <span>全部房间</span>
              <span className="count">
                {rooms ? `${rooms.length}${hasMore ? "+" : ""}` : "…"}
              </span>
              <button
                type="button"
                className="icon-btn icon-btn-ghost"
                onClick={onRefresh}
                title="刷新"
                aria-label="刷新"
                style={{ marginLeft: "auto" }}
              >
                <Icon name="refresh" size={15} />
              </button>
            </div>
            {rooms === null ? (
              <p className="home-fine">正在加载房间列表…</p>
            ) : rooms.length === 0 ? (
              <p className="home-fine">还没有任何房间。</p>
            ) : (
              <>
                <ul>
                  {rooms.map((room) => (
                    <li key={room.room_id}>
	                      <button
	                        className="room-link"
	                        type="button"
	                        onClick={() => onEnterRoom(room.room_id, true)}
	                        title={room.room_id}
                      >
                        <span className="room-hash">
                          <Icon name="hash" size={14} />
                        </span>
                        <span className="room-link-text">
                          <strong>{room.title || shortRoomID(room.room_id)}</strong>
                          <span>{adminRoomMeta(room)}</span>
                        </span>
                        <Icon name="chevronRight" size={15} />
                      </button>
                      {room.github_url && (
                        <a
                          className="icon-btn icon-btn-ghost room-github-link"
                          href={room.github_url}
                          target="_blank"
                          rel="noreferrer"
                          title="打开 GitHub"
                          aria-label="打开 GitHub"
                        >
                          <Icon name="github" size={15} />
                        </a>
                      )}
                      <button
                        className="icon-btn icon-btn-ghost"
                        type="button"
                        onClick={() => onOpenRoomNewTab(room.room_id)}
                        title="新开页面打开"
                        aria-label="新开页面打开"
                      >
                        <Icon name="arrowUpRight" size={15} />
                      </button>
                    </li>
                  ))}
                </ul>
                {hasMore ? (
                  <div ref={sentinelRef} className="home-rooms-more">
                    {loadingMore ? (
                      <span className="home-fine">正在加载更多…</span>
                    ) : (
                      <button type="button" className="btn" onClick={onLoadMore}>
                        加载更多
                      </button>
                    )}
                  </div>
                ) : (
                  <p className="home-fine home-rooms-more">已加载全部 {rooms.length} 间房间</p>
                )}
              </>
            )}
          </section>
        )}
      </main>
    </div>
  );
}

function AdminUsersPanel({
  report,
  onRefresh,
}: {
  report: AdminUsersReport | null;
  onRefresh: () => void;
}) {
  const maxTrend = Math.max(
    1,
    ...(report?.trend ?? []).map((point) => Math.max(point.logins, point.new_users)),
  );
  return (
    <section className="admin-users" aria-label="Users">
      <div className="home-rooms-head">
        <span>用户看板</span>
        <span className="count">{report ? `${report.total_users}` : "…"}</span>
        <button
          type="button"
          className="icon-btn icon-btn-ghost"
          onClick={onRefresh}
          title="刷新"
          aria-label="刷新"
          style={{ marginLeft: "auto" }}
        >
          <Icon name="refresh" size={15} />
        </button>
      </div>
      {report === null ? (
        <p className="home-fine admin-pad">正在加载用户数据…</p>
      ) : (
        <>
          <div className="admin-stat-grid">
            <AdminStat label="总用户" value={report.total_users} />
            <AdminStat label="当前在线" value={report.online_users} />
            <AdminStat label="24h 登录" value={report.logins_24h} />
            <AdminStat label="7d 登录" value={report.logins_7d} />
          </div>
          <div className="admin-trend" aria-label="7 day user trend">
            {report.trend.map((point) => (
              <div
                className="admin-trend-day"
                key={point.date}
                title={`${point.date} 登录 ${point.logins} · 新用户 ${point.new_users}`}
              >
                <span
                  className="admin-bar admin-bar-login"
                  style={{ height: `${Math.max(6, (point.logins / maxTrend) * 72)}px` }}
                />
                <span
                  className="admin-bar admin-bar-new"
                  style={{ height: `${Math.max(6, (point.new_users / maxTrend) * 72)}px` }}
                />
                <small>{formatTrendDay(point.date)}</small>
              </div>
            ))}
          </div>
          {report.users.length === 0 ? (
            <p className="home-fine admin-pad">还没有记录到登录用户。</p>
          ) : (
            <ul className="admin-user-list">
              {report.users.map((user) => (
                <AdminUserRow key={user.login} user={user} />
              ))}
            </ul>
          )}
        </>
      )}
    </section>
  );
}

function AdminStat({ label, value }: { label: string; value: number }) {
  return (
    <div className="admin-stat">
      <strong>{value}</strong>
      <span>{label}</span>
    </div>
  );
}

function AdminUserRow({ user }: { user: AdminUser }) {
  const roomText =
    user.online_room_ids && user.online_room_ids.length > 0
      ? `在线房间 ${user.online_room_ids.map(shortRoomID).join(", ")}`
      : "未连接房间";
  return (
    <li className="admin-user-row">
      <Avatar
        person={{
          id: user.login,
          label: user.name || user.login,
          kind: "user",
          avatar_url: user.avatar_url,
        }}
        size={34}
        dark
      />
      <div className="admin-user-main">
        <div className="admin-user-title">
          <strong>@{user.login}</strong>
          <span className={user.online ? "admin-online on" : "admin-online"}>
            {user.online ? `在线 ${user.connection_count}` : "离线"}
          </span>
        </div>
        <span>{user.name || user.email || "未设置显示名"}</span>
      </div>
      <div className="admin-user-meta">
        <span>
          <Icon name="clock" size={13} /> 上次登录 {formatRecordTime(user.last_login_at)}
        </span>
        <span>
          <Icon name="hash" size={13} /> 创建 {user.rooms_created} 间 · 登录 {user.login_count} 次
        </span>
        <span>
          <Icon name="activity" size={13} /> {roomText}
        </span>
      </div>
    </li>
  );
}

function formatTrendDay(value: string): string {
  const date = new Date(`${value}T00:00:00`);
  if (Number.isNaN(date.getTime())) return value.slice(5);
  return date.toLocaleDateString([], { month: "numeric", day: "numeric" });
}

function adminRoomMeta(room: Room): string {
  const parts: string[] = [];
  parts.push(room.owner ? `房主 @${room.owner}` : "匿名房间");
  if (room.gated) parts.push("已开启审批");
  if (room.ended) parts.push("已结束");
  parts.push(formatRecordTime(room.created_at));
  return parts.join(" · ");
}

/* ── QuickStart (terminal window from current production) ───────────── */

type QuickStartOS = "mac" | "linux" | "windows";
type QuickStartMode = "agent" | "executor";

const QUICKSTART_OSES: { id: QuickStartOS; label: string; shell: string }[] = [
  { id: "mac", label: "macOS", shell: "zsh" },
  { id: "linux", label: "Linux", shell: "bash" },
  { id: "windows", label: "Windows", shell: "powershell" },
];

const QUICKSTART_MODES: {
  id: QuickStartMode;
  title: string;
  subtitle: string;
  detail: string;
}[] = [
  {
    id: "agent",
    title: "AI Agent",
    subtitle: "本机起一个 Claude CLI",
    detail:
      "作为对话方加入房间，能被 @mention 召唤并主动回复。需要本机已安装并登录 Claude Code CLI。",
  },
  {
    id: "executor",
    title: "Executor (Slave)",
    subtitle: "不跑 AI，被动执行命令",
    detail:
      "即传统「Slave」节点：bridge 以 -bridge-mode executor 启动，远端 @ 它时凭 exec-token 执行授权命令，首次启动自动生成并固化 token。",
  },
];

export function detectQuickStartOS(): QuickStartOS {
  if (typeof navigator === "undefined") return "mac";
  const p = `${navigator.platform || ""} ${navigator.userAgent || ""}`;
  if (/Mac|iPhone|iPad/i.test(p)) return "mac";
  if (/Win/i.test(p)) return "windows";
  return "linux";
}

function downloadOrigin(): string {
  if (typeof window === "undefined") return "";
  return window.location.origin || "";
}

function quickStartSnippet(mode: QuickStartMode, os: QuickStartOS): string {
  const isWin = os === "windows";
  const binary = isWin ? ".\\agent-room.exe" : "agent-room";
  const cont = isWin ? "`" : "\\";
  const relayArg = downloadOrigin() ? ` -relay ${downloadOrigin()}` : "";
  const installLines = isWin
    ? [
        "# 1. 下载 Bridge CLI (PowerShell 直接下载预编译 exe，不需要 Go)",
        `iwr ${downloadOrigin()}/downloads/windows -OutFile agent-room.exe`,
      ]
    : [
        "# 1. 安装 Bridge CLI (curl 下载预编译二进制，不需要 Go)",
        `curl -fsSL ${downloadOrigin()}/downloads/install.sh | bash`,
        "export PATH=\"$HOME/.local/bin:$PATH\"",
      ];

  if (mode === "agent") {
    return [
      ...installLines,
      "",
      "# 2. 启动 AI Agent — 本机的 Claude 加入房间对话",
      "#    需要先装好 Claude Code CLI 并登录",
      `${binary} bridge${relayArg}`,
    ].join("\n");
  }

  return [
    ...installLines,
    "",
    "# 2. 以 Slave (Executor) 模式启动 — 不跑 AI，等被 @ 时执行命令",
    "#    首次会自动生成 exec-token，缓存到 ~/.agent-room/exec-token",
    `${binary} bridge${relayArg} ${cont}`,
    `  -mode executor`,
  ].join("\n");
}

function QuickStart({ onShowToast }: { onShowToast: (message: string) => void }) {
  const [mode, setMode] = useState<QuickStartMode>("agent");
  const [os, setOs] = useState<QuickStartOS>(() => detectQuickStartOS());
  const code = useMemo(() => quickStartSnippet(mode, os), [mode, os]);
  const osMeta = QUICKSTART_OSES.find((entry) => entry.id === os) ?? QUICKSTART_OSES[0];
  const modeMeta = QUICKSTART_MODES.find((entry) => entry.id === mode) ?? QUICKSTART_MODES[0];

  function copy() {
    navigator.clipboard.writeText(code).then(
      () => onShowToast("命令已复制"),
      () => onShowToast("复制失败"),
    );
  }

  return (
    <section className="quickstart" aria-label="快速接入">
      <div className="quickstart-head">
        <h2>把本机接入房间</h2>
        <p>选一种角色，复制下面的命令到终端。</p>
      </div>

      <div className="quickstart-modes" role="radiogroup" aria-label="选择 Bridge 模式">
        {QUICKSTART_MODES.map((entry) => (
          <button
            key={entry.id}
            type="button"
            role="radio"
            aria-checked={mode === entry.id}
            className={mode === entry.id ? "quickstart-mode active" : "quickstart-mode"}
            onClick={() => setMode(entry.id)}
          >
            <strong>{entry.title}</strong>
            <span>{entry.subtitle}</span>
          </button>
        ))}
      </div>

      <p className="quickstart-mode-detail">{modeMeta.detail}</p>

      <div className="term">
        <div className="term-bar">
          <span className="term-dots">
            <i className="d-r" />
            <i className="d-y" />
            <i className="d-g" />
          </span>
          <div className="term-tabs" role="tablist" aria-label="选择操作系统">
            {QUICKSTART_OSES.map((entry) => (
              <button
                key={entry.id}
                type="button"
                role="tab"
                aria-selected={os === entry.id}
                className={os === entry.id ? "on" : ""}
                onClick={() => setOs(entry.id)}
              >
                {entry.label}
              </button>
            ))}
          </div>
          <button type="button" className="term-copy" onClick={copy}>
            <Icon name="copy" size={13} /> 复制
          </button>
        </div>
        <div className="term-body">
          <span className="term-shell">
            — {modeMeta.title} · {osMeta.label} · {osMeta.shell} —
          </span>
          <pre>{renderQuickStartCode(code, os)}</pre>
        </div>
      </div>
    </section>
  );
}

function renderQuickStartCode(code: string, os: QuickStartOS): ReactNode {
  const promptChar = os === "windows" ? "PS>" : "$";
  return code.split("\n").map((rawLine, index) => {
    const isComment = rawLine.trim().startsWith("#");
    const isContinuation = rawLine.startsWith("  ");
    const isEmpty = rawLine.trim() === "";
    if (isEmpty)
      return (
        <div key={index} className="ql ql-b">
          {" "}
        </div>
      );
    if (isComment)
      return (
        <div key={index} className="ql ql-c">
          {rawLine}
        </div>
      );
    if (isContinuation)
      return (
        <div key={index} className="ql ql-cmd">
          <span className="ql-p">{" "}</span>
          <span>{rawLine}</span>
        </div>
      );
    return (
      <div key={index} className="ql ql-cmd">
        <span className="ql-p">{promptChar}</span>
        <span>{rawLine}</span>
      </div>
    );
  });
}
