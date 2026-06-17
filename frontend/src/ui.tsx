import type { CSSProperties, ReactNode } from "react";
import { agentTint, hueFromID, initials, normalizeOS } from "./lib";
import { Icon, OsGlyph, osLabel } from "./icons";

const GITHUB_REPO_URL = "https://github.com/zhoushoujianwork/agent_room";

interface AvatarPerson {
  id: string;
  label?: string;
  kind?: "user" | "agent" | "system";
  avatar_url?: string;
  hue?: number;
}

interface AvatarProps {
  person: AvatarPerson;
  size?: number;
  dark: boolean;
  ring?: boolean;
}

export function Avatar({ person, size = 30, dark, ring = true }: AvatarProps) {
  const hue = person.hue ?? hueFromID(person.id);
  const tint = agentTint(hue, dark);
  const isAgent = person.kind === "agent";
  if (person.avatar_url) {
    return (
      <img
        className="avatar"
        src={person.avatar_url}
        alt={person.label || person.id}
        width={size}
        height={size}
        style={{ width: size, height: size, borderRadius: "50%", objectFit: "cover" }}
      />
    );
  }
  return (
    <span
      className="avatar"
      style={{
        width: size,
        height: size,
        borderRadius: isAgent ? Math.round(size * 0.28) : "50%",
        background: tint.soft,
        color: tint.text,
        boxShadow: ring ? `inset 0 0 0 1px ${tint.line}` : "none",
        fontSize: size * 0.4,
      }}
    >
      {initials(person.label || person.id)}
    </span>
  );
}

/**
 * OsAvatar wraps Avatar with a small corner badge showing the participant's
 * operating system (brand glyph). The badge only renders when `os` resolves to
 * a known value, so participants without OS metadata get a plain avatar.
 */
export function OsAvatar({
  person,
  size = 32,
  dark,
  os,
}: AvatarProps & { os?: string | null }) {
  const resolved = normalizeOS(os);
  return (
    <span className="os-avatar">
      <Avatar person={person} size={size} dark={dark} />
      {resolved && (
        <span className={`os-badge os-badge-${resolved}`} title={osLabel(resolved)}>
          <OsGlyph os={resolved} size={11} />
        </span>
      )}
    </span>
  );
}

export type StatusTone = "live" | "ok" | "idle" | "busy" | "off" | "sys";

interface StatusDotProps {
  tone?: StatusTone;
  pulse?: boolean;
  size?: number;
}

export function StatusDot({ tone = "live", pulse = false, size = 8 }: StatusDotProps) {
  return (
    <span
      className={`sdot sdot-${tone}${pulse ? " sdot-pulse" : ""}`}
      style={{ width: size, height: size }}
    />
  );
}

export type ChipTone = "neutral" | "ok" | "warn" | "danger" | "user" | "mono";

interface ChipProps {
  children: ReactNode;
  tone?: ChipTone;
  mono?: boolean;
  style?: CSSProperties;
}

export function Chip({ children, tone = "neutral", mono = false, style }: ChipProps) {
  const cls = tone === "mono" ? "chip chip-mono" : `chip chip-${tone}${mono ? " chip-mono" : ""}`;
  return (
    <span className={cls} style={style}>
      {children}
    </span>
  );
}

const MODE_META: Record<string, { label: string; short: string; icon: string; tone: string; desc: string }> = {
  agent: {
    label: "Agent 模式",
    short: "Agent",
    icon: "brain",
    tone: "mode-agent",
    desc: "机器上运行自主 AI agent，可自行规划并发起命令",
  },
  slave: {
    label: "Slave 模式",
    short: "Slave",
    icon: "terminal",
    tone: "mode-slave",
    desc: "只执行被授权的 shell 命令，不自行决策",
  },
};

export function ModeBadge({ mode, full = false }: { mode: string; full?: boolean }) {
  const m = MODE_META[mode] || MODE_META.slave;
  return (
    <span className={`mode-badge ${m.tone}`} title={m.desc}>
      <Icon name={m.icon} size={11} /> {full ? m.label : m.short}
    </span>
  );
}

export function BrandMark({ size = 30 }: { size?: number }) {
  return (
    <span
      className="brandmark"
      style={{ width: size, height: size, fontSize: size * 0.5 }}
    >
      <span className="brandmark-glyph">◴</span>
    </span>
  );
}

export function TopBrand({ right }: { right?: ReactNode }) {
  return (
    <div className="home-nav">
      <div className="home-brand">
        <BrandMark size={30} />
        <div className="home-brand-text">
          <strong>Agent Room</strong>
          <span>isolated agents, one shared room</span>
        </div>
      </div>
      <div className="home-nav-right">
        <a
          className="icon-btn icon-btn-line"
          href={GITHUB_REPO_URL}
          target="_blank"
          rel="noreferrer"
          title="在 GitHub 查看项目"
          aria-label="在 GitHub 查看 Agent Room 项目"
        >
          <Icon name="github" size={17} />
        </a>
        {right}
      </div>
    </div>
  );
}

// SignInPill is the provider-aware, optional login affordance. External SSO
// and GitHub share the same anonymous-first model: the pill
// is never forced — clicking it starts an external login that returns a
// session cookie. Lives here (shared primitives) so both home.tsx and
// shell.tsx use it without a circular import.
export function SignInPill({
  loginURL,
  provider,
}: {
  loginURL: string;
  provider?: "sso" | "github";
}) {
  const isSSO = provider === "sso";
  return (
    <a className="signin-pill" href={loginURL}>
      <Icon name={isSSO ? "shieldCheck" : "github"} size={14} />{" "}
      {isSSO ? "用 SSO 账号登录" : "Sign in"}
    </a>
  );
}
