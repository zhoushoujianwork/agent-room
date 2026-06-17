import { useEffect, useRef, useState } from "react";
import type { AttachmentRef, ChatMessage } from "./types";
import { attachmentURL } from "./api";
import { Icon } from "./icons";
import { Avatar, Chip, StatusDot } from "./ui";
import { agentTint, durationMsLabel, formatElapsed, hueFromID, shortClock } from "./lib";
import { Markdown } from "./markdown";

/* ──────────────────────────────────────────────────────────────────
   Grouping (carried over from the previous implementation): chat/system/event
   messages get one-per-bubble; trace messages collapse into a thinking-stream
   group; command + command_result messages join into a single command card.
   ────────────────────────────────────────────────────────────────── */

export type MessageGroup =
  | { kind: "trace"; key: string; items: ChatMessage[]; runs?: Map<string, ChatMessage[]> }
  | { kind: "command"; key: string; items: ChatMessage[] }
  | { kind: "event"; items: [ChatMessage] }
  | { kind: "chat"; items: [ChatMessage] };

export function groupMessages(messages: ChatMessage[]): MessageGroup[] {
  const groups: MessageGroup[] = [];
  const traceIndexes = new Map<string, number>();
  const commandIndexes = new Map<string, number>();
  // agent 经 exec_remote 委派的命令(随附 phase=delegate_exec 的 trace)不再作为
  // 顶层卡片按时间序插在聊天流末尾——发起方的 thinking 流还在上面跑,命令卡片却
  // 一条条堆在最下面,长任务直接把聊天流撑爆。先扫一遍建 command_id -> 发起方
  // thinking 流 key 的映射;分组时把这些 command/command_result(以及 executor 回
  // 报的同 command_id trace)收进对应流的 runs,由 ThinkingStream 嵌套渲染成默认
  // 收起的命令卡片。
  const delegated = new Map<string, string>();
  for (const msg of messages) {
    if (msg.type !== "trace" || msg.metadata?.phase !== "delegate_exec") continue;
    const commandID = msg.metadata?.command_id;
    if (commandID) delegated.set(commandID, traceGroupKey(msg));
  }
  const topLevelCommandIDs = new Set<string>();
  for (const msg of messages) {
    if (msg.type !== "command" && msg.type !== "command_result") continue;
    const commandID = msg.metadata?.command_id || (msg.type === "command" ? msg.id : "") || "";
    if (commandID && !delegated.has(commandID)) topLevelCommandIDs.add(commandID);
  }
  const runsByStream = new Map<string, Map<string, ChatMessage[]>>();
  const stashRun = (streamKey: string, commandID: string, msg: ChatMessage) => {
    let runs = runsByStream.get(streamKey);
    if (!runs) {
      runs = new Map();
      runsByStream.set(streamKey, runs);
    }
    const list = runs.get(commandID);
    if (list) list.push(msg);
    else runs.set(commandID, [msg]);
  };
  // presence is online-state, not an event stream: a bridge that reconnects
  // re-emits "joined" every time, which otherwise stacks up as repeated rows.
  // Keep only the latest presence per sender so history shows one row each.
  const latestPresenceID = new Map<string, string>();
  for (const msg of messages) {
    if (msg.type !== "presence") continue;
    const key = msg.sender_id || "";
    const stamp = msg.id || msg.created_at || "";
    latestPresenceID.set(key, stamp);
  }
  for (const msg of messages) {
    if (msg.type === "presence") {
      const key = msg.sender_id || "";
      const stamp = msg.id || msg.created_at || "";
      if (latestPresenceID.get(key) !== stamp) continue; // drop superseded joins
      groups.push({ kind: "event", items: [msg] });
      continue;
    }
    if (msg.type === "command" || msg.type === "command_result") {
      const commandID = msg.metadata?.command_id || (msg.type === "command" ? msg.id : "") || "";
      const streamKey = commandID ? delegated.get(commandID) : undefined;
      if (streamKey) {
        stashRun(streamKey, commandID, msg);
        continue;
      }
      const key = commandGroupKey(msg);
      const existing = commandIndexes.get(key);
      if (existing !== undefined) {
        const group = groups[existing];
        if (group?.kind === "command") group.items.push(msg);
        continue;
      }
      commandIndexes.set(key, groups.length);
      groups.push({ kind: "command", key, items: [msg] });
      continue;
    }
    if (msg.type === "control") {
      // 控制消息(stop / permission_reply)是带外信号,不渲染:否则审批回灌的
      // control(content=allow_once)会冒出一条聊天气泡。
      continue;
    }
    if (msg.type !== "trace") {
      // presence is handled above; system messages also render as event rows.
      const isEvent = msg.type === "system";
      groups.push({ kind: isEvent ? "event" : "chat", items: [msg] });
      continue;
    }
    // 权限审批请求不进时间线:待审批的由 PermissionToasts 从右侧弹入,已结束的
    // 折叠成 PermissionMarkers 标记条挂在对话上方(见 permissionRequests)。
    if (msg.metadata?.phase === "permission_request") {
      continue;
    }
    // executor 对被委派命令回报的 executing/done trace(带 command_id、无 reply_to)
    // 也折进命令卡片,不再各自形成一条孤立的 mini thinking 流。
    if (!msg.metadata?.reply_to) {
      const commandID = msg.metadata?.command_id || "";
      const streamKey = commandID ? delegated.get(commandID) : undefined;
      if (streamKey) {
        stashRun(streamKey, commandID, msg);
        continue;
      }
      if (commandID && topLevelCommandIDs.has(commandID)) {
        const existing = commandIndexes.get(`command:${commandID}`);
        if (existing !== undefined) {
          const group = groups[existing];
          if (group?.kind === "command") group.items.push(msg);
        }
        continue;
      }
    }
    const key = traceGroupKey(msg);
    const existing = traceIndexes.get(key);
    if (existing !== undefined) {
      const group = groups[existing];
      if (group?.kind === "trace") group.items.push(msg);
      continue;
    }
    traceIndexes.set(key, groups.length);
    groups.push({ kind: "trace", key, items: [msg] });
  }
  // 把收集到的委派命令挂回发起方的 thinking 流。delegate_exec trace 自身经
  // reply_to 归入该流,所以正常情况下流一定存在;万一被历史分页截断,退回
  // 顶层命令卡片兜底,内容不丢。
  for (const [streamKey, runs] of runsByStream) {
    const index = traceIndexes.get(streamKey);
    const group = index !== undefined ? groups[index] : undefined;
    if (group?.kind === "trace") {
      group.runs = runs;
      continue;
    }
    for (const [commandID, items] of runs) {
      groups.push({ kind: "command", key: `command:${commandID}`, items });
    }
  }
  return groups;
}

function commandGroupKey(message: ChatMessage): string {
  if (message.metadata?.command_id) return `command:${message.metadata.command_id}`;
  if (message.type === "command" && message.id) return `command:${message.id}`;
  return `command:${message.sender_id}:${message.target_id || ""}:${message.id || message.created_at || ""}`;
}

// permissionKey 是一条审批请求 trace 的稳定标识,toast/标记条都按它去重路由。
function permissionKey(message: ChatMessage): string {
  const id = message.metadata?.permission_id;
  if (id) return id;
  return `${message.sender_id}:${message.id || message.created_at || ""}`;
}

function traceGroupKey(message: ChatMessage): string {
  if (message.metadata?.reply_to) return `reply:${message.metadata.reply_to}`;
  if (message.metadata?.command_id) return `command:${message.metadata.command_id}`;
  if (message.metadata?.tool_use_id) return `tool:${message.metadata.tool_use_id}`;
  return `trace:${message.sender_id}:${message.target_id || ""}:${message.id || message.created_at || ""}`;
}

export function latestTraceItems(messages: ChatMessage[]): ChatMessage[] {
  const latest = new Map<string, { message: ChatMessage; index: number }>();
  messages.forEach((message, index) => {
    if (message.type !== "trace") return;
    // 权限审批请求单独渲染成卡片,不进入侧栏的 thinking 摘要。
    if (message.metadata?.phase === "permission_request") return;
    latest.set(traceGroupKey(message), { message, index });
  });
  return [...latest.values()]
    .sort((left, right) => right.index - left.index)
    .map((item) => item.message);
}

/* ── Attachments ──────────────────────────────────────────────────── */

// parseAttachments 解析消息 metadata.attachments(JSON 数组)。容忍空值/坏
// JSON —— metadata 是不可信输入,解析失败当无附件。
export function parseAttachments(message: ChatMessage): AttachmentRef[] {
  const raw = message.metadata?.attachments;
  if (!raw) return [];
  try {
    const parsed = JSON.parse(raw) as AttachmentRef[];
    if (!Array.isArray(parsed)) return [];
    return parsed.filter((ref) => ref && typeof ref.id === "string" && ref.id.trim() !== "");
  } catch {
    return [];
  }
}

// MessageAttachments 把附件渲染成图片气泡;点击在新标签页打开原图。
function MessageAttachments({ message }: { message: ChatMessage }) {
  const refs = parseAttachments(message);
  if (refs.length === 0) return null;
  return (
    <div className="bubble-attachments">
      {refs.map((ref) => {
        const url = attachmentURL(message.room_id, ref.id);
        return (
          <a key={ref.id} href={url} target="_blank" rel="noreferrer" className="bubble-att">
            <img src={url} alt={ref.name || "attachment"} loading="lazy" />
          </a>
        );
      })}
    </div>
  );
}

/* ── Chat bubble ──────────────────────────────────────────────────── */

interface MessageItemProps {
  message: ChatMessage;
  viewerID: string;
  dark: boolean;
  mentioned?: boolean;
  onReply?: (message: ChatMessage) => void;
}

export function MessageItem({ message, viewerID, dark, mentioned = false, onReply }: MessageItemProps) {
  const isMine =
    message.sender_kind === "user" && (message.sender_id === viewerID || message.sender_id === "me");
  const hue = hueFromID(message.sender_id);
  const tint = agentTint(hue, dark);
  const target = message.target_id || "";
  const person = {
    id: message.sender_id,
    label: message.sender_id,
    kind: message.sender_kind,
    hue,
  };
  return (
    <div className={`bubble-row ${isMine ? "mine" : "theirs"}${mentioned ? " mentioned" : ""}`}>
      {!isMine && <Avatar person={person} dark={dark} size={32} />}
      <div className="bubble-col">
        <div className="bubble-meta">
          <strong style={{ color: tint.text }}>{message.sender_id || "unknown"}</strong>
          {message.sender_kind === "agent" && message.metadata?.role && (
            <span className="bubble-role">{message.metadata.role}</span>
          )}
          {target && (
            <span className="bubble-route">
              <Icon name="chevronRight" size={12} /> {target}
            </span>
          )}
          <time>{shortClock(message.created_at)}</time>
        </div>
        <div
          className="bubble"
          style={{
            borderLeft: `2.5px solid ${tint.solid}`,
            background: isMine ? tint.faint : "var(--surface)",
          }}
        >
          {message.metadata?.reply_to && (
            <div className="bubble-reply-ref">
              <span>{message.metadata.reply_sender || "引用"}</span>
              <p>{message.metadata.reply_preview || "原消息"}</p>
            </div>
          )}
          <Markdown text={message.content || ""} />
          <MessageAttachments message={message} />
        </div>
        {onReply && (
          <button type="button" className="bubble-reply-btn" onClick={() => onReply(message)}>
            <Icon name="reply" size={13} /> 回复
          </button>
        )}
      </div>
      {isMine && <Avatar person={person} dark={dark} size={32} />}
    </div>
  );
}

/* ── Event row (presence / system) ────────────────────────────────── */

export function EventItem({ message }: { message: ChatMessage }) {
  const target = message.target_id ? ` -> ${message.target_id}` : "";
  return (
    <div className="evt-row">
      <span className="evt-rail" />
      <div className="evt-main">
        <span className="evt-title">
          {message.sender_id || "system"}
          {target}
        </span>
        <span className="evt-detail">{compactEventText(message)}</span>
      </div>
      <time className="evt-time">{shortClock(message.created_at)}</time>
    </div>
  );
}

function compactEventText(message: ChatMessage): string {
  const text = (message.content || "").trim();
  if (!text) return message.type === "presence" ? "presence updated" : "event";
  return text.length > 140 ? `${text.slice(0, 137)}...` : text;
}

/* ── Thinking stream ──────────────────────────────────────────────── */

export function ThinkingStream({
  traces,
  runs,
  dark,
  onStop,
}: {
  traces: ChatMessage[];
  // 委派命令(command_id -> 该命令的 command/command_result/trace 消息),由
  // groupMessages 折进本流;delegate_exec 步骤据此原位渲染成默认收起的命令卡片。
  runs?: Map<string, ChatMessage[]>;
  dark: boolean;
  onStop?: (targetID: string, replyTo?: string, commandID?: string) => void;
}) {
  const first = traces[0];
  const last = traces[traces.length - 1];
  const sender = first.sender_id;
  const target = first.target_id;
  const lastPhase = (last.metadata?.phase || "thinking") as string;
  const finished = lastPhase === "done" || lastPhase === "error" || lastPhase === "stopped";
  const replyTo = first.metadata?.reply_to;
  const startMs = first.created_at ? new Date(first.created_at).getTime() : 0;
  const lastTraceMs = last.created_at ? new Date(last.created_at).getTime() : startMs;
  const endMs = finished && last.created_at ? new Date(last.created_at).getTime() : 0;
  const finalDurationMs = finished
    ? Number(last.metadata?.duration_ms || "") || (endMs && startMs ? endMs - startMs : 0)
    : 0;

  const [now, setNow] = useState(() => Date.now());
  // A stream with no terminal trace (done/error/stopped) normally ticks live.
  // But a dropped bridge connection or a lost terminal trace would otherwise let
  // the clock climb forever (we once saw "92m" on a stream whose last trace was
  // a tool_use 7min in). So if the *last* trace is older than STALL_MS, treat the
  // stream as stalled: freeze the clock at the last trace and flag it, instead of
  // pretending the agent is still thinking. STALL_MS sits just above the claude
  // default idle timeout (10min) to avoid killing genuinely long single steps.
  const STALL_MS = 12 * 60_000;
  const stalled = !finished && now - lastTraceMs > STALL_MS;
  useEffect(() => {
    if (finished || stalled) return;
    const tick = window.setInterval(() => setNow(Date.now()), 250);
    return () => window.clearInterval(tick);
  }, [finished, stalled]);
  const elapsedMs = finished
    ? finalDurationMs
    : stalled
      ? Math.max(0, lastTraceMs - startMs)
      : startMs
        ? Math.max(0, now - startMs)
        : 0;

  const hue = hueFromID(sender);
  const tint = agentTint(hue, dark);

  // Finished streams collapse to a one-line summary by default; click to expand.
  // In-progress streams are always expanded and the head is not interactive.
  const [open, setOpen] = useState(false);
  const collapsed = finished && !open;

  const events = traces.filter((t) => {
    const p = t.metadata?.phase || "";
    if (p === "thinking" || p === "done" || p === "error" || p === "stopped") return false;
    // Drop contentless draft steps so finished streams don't show empty rows.
    if (p === "text" && !(t.content || "").trim()) return false;
    return true;
  });

  // Collapse consecutive identical steps. Claude CLI can emit the same
  // step several times in one reply (e.g. a "session started · <model>"
  // init event per resumed turn), which otherwise stacks as duplicates.
  const steps = collapseAdjacent(events);

  return (
    <div
      className="stream"
      style={{ ["--tint" as string]: tint.solid, ["--tint-soft" as string]: tint.soft }}
    >
      <button
        type="button"
        className={!finished && onStop ? "stream-head stream-head-busy" : "stream-head"}
        onClick={() => finished && setOpen((v) => !v)}
        disabled={!finished}
        aria-expanded={finished ? open : undefined}
      >
        <Avatar person={{ id: sender, label: sender, kind: "agent", hue }} dark={dark} size={24} />
        <span className="stream-title">
          <strong style={{ color: tint.text }}>{sender}</strong>
          {target && <span className="stream-sub">→ {target}</span>}
        </span>
        <span className="stream-status">
          {finished ? (
            lastPhase === "error" ? (
              <span style={{ color: "var(--danger)" }}>failed</span>
            ) : lastPhase === "stopped" ? (
              <span style={{ color: "var(--warn)" }}>stopped</span>
            ) : (
              <>thought for {(elapsedMs / 1000).toFixed(1)}s · {steps.length} steps</>
            )
          ) : stalled ? (
            <span style={{ color: "var(--warn)" }}>
              ⚠ 可能已断开 · 最后活动 {(elapsedMs / 1000).toFixed(1)}s 后停滞
            </span>
          ) : (
            <>
              <span className="stream-spinner" /> thinking
            </>
          )}
        </span>
        {!finished && !stalled && <span className="stream-clock">{formatElapsed(elapsedMs)}</span>}
        {finished && <Icon name={open ? "chevronDown" : "chevronRight"} size={15} />}
      </button>
      {!finished && onStop && (
        <button
          type="button"
          className="stream-stop"
          onClick={() => onStop(sender, replyTo)}
          title="停止生成"
        >
          <Icon name="stop" size={13} /> 停止
        </button>
      )}
      {!collapsed && (
        <ol className="stream-events">
          {steps.map((step, index) => {
            const key = step.trace.id || `${step.trace.sender_id}-${index}`;
            const commandID = step.trace.metadata?.command_id || "";
            const run =
              step.trace.metadata?.phase === "delegate_exec" && commandID
                ? runs?.get(commandID)
                : undefined;
            if (run && run.length > 0) {
              // 委派的远程命令在发起步骤原位渲染成命令卡片(默认收起),
              // 而不是漂到聊天流底部。
              return (
                <li className="sev sev-cmdrun" key={key}>
                  <span className="sev-node">
                    <Icon name="terminal" size={11} />
                  </span>
                  <div className="sev-body">
                    <CommandRunCard messages={run} dark={dark} embedded onStop={onStop} />
                  </div>
                </li>
              );
            }
            return (
              <ThinkingEvent
                trace={step.trace}
                repeat={step.repeat}
                isLast={index === steps.length - 1 && !finished}
                key={key}
              />
            );
          })}
        </ol>
      )}
    </div>
  );
}

// collapseAdjacent merges runs of identical steps (same phase + tool +
// content) into a single entry carrying how many times it repeated.
function collapseAdjacent(events: ChatMessage[]): { trace: ChatMessage; repeat: number }[] {
  const out: { trace: ChatMessage; repeat: number }[] = [];
  for (const t of events) {
    const last = out[out.length - 1];
    if (last && sameStep(last.trace, t)) {
      last.repeat += 1;
      continue;
    }
    out.push({ trace: t, repeat: 1 });
  }
  return out;
}

function sameStep(a: ChatMessage, b: ChatMessage): boolean {
  return (
    (a.metadata?.phase || "") === (b.metadata?.phase || "") &&
    (a.metadata?.tool || "") === (b.metadata?.tool || "") &&
    (a.content || "") === (b.content || "") &&
    (a.metadata?.detail || "") === (b.metadata?.detail || "") &&
    // 连续委派同一目标的 delegate_exec 步骤 content 完全相同("delegating
    // command to <target>"),但各是独立的命令卡片,绝不能按重复步骤合并。
    (a.metadata?.command_id || "") === (b.metadata?.command_id || "")
  );
}

function ThinkingEvent({ trace, repeat, isLast }: { trace: ChatMessage; repeat: number; isLast: boolean }) {
  const phase = (trace.metadata?.phase || "") as string;
  const tool = trace.metadata?.tool || "";
  const detail = (trace.metadata?.detail || "").trim();
  const summary = (trace.content || "").trim();
  const [open, setOpen] = useState(false);
  const expandable = detail.length > 0 && detail !== summary;
  const kind = phase === "tool_use" ? "tool" : "think";

  const label = eventLabel(phase, tool, summary);
  return (
    <li className={`sev sev-${kind}`}>
      <span className="sev-node">
        {kind === "tool" ? (
          <Icon name="terminal" size={11} />
        ) : isLast ? (
          <span className="sev-mini-spin" />
        ) : (
          <span className="sev-tick">
            <Icon name="checkSmall" size={11} />
          </span>
        )}
      </span>
      <div className="sev-body">
        <button
          type="button"
          className={`sev-line${expandable ? " has-detail" : ""}`}
          onClick={expandable ? () => setOpen((v) => !v) : undefined}
          disabled={!expandable}
        >
          {kind === "tool" && tool && <span className="sev-tool">{tool}</span>}
          <span className="sev-text">{label}</span>
          {repeat > 1 && <span className="sev-repeat">×{repeat}</span>}
          {expandable && (
            <Icon name={open ? "chevronDown" : "chevronRight"} size={13} />
          )}
        </button>
        {expandable && open && <pre className="sev-detail">{detail}</pre>}
      </div>
    </li>
  );
}

function eventLabel(phase: string, tool: string, summary: string): string {
  if (phase === "tool_use") return summary || tool || "tool call";
  if (phase === "tool_result") return summary || "(no output)";
  if (phase === "executing") return summary || "executing";
  if (phase === "text") return summary || "drafting…";
  if (phase === "system") return summary || "session started";
  return summary || phase;
}

/* ── Command run card ─────────────────────────────────────────────── */

export function CommandRunCard({
  messages,
  dark,
  embedded = false,
  onStop,
}: {
  messages: ChatMessage[];
  dark: boolean;
  // embedded:嵌在发起方 thinking 流里的窄版(去掉 rail 对齐宽度)。
  embedded?: boolean;
  onStop?: (targetID: string, replyTo?: string, commandID?: string) => void;
}) {
  const command = messages.find((m) => m.type === "command");
  const resultMessage = [...messages].reverse().find((m) => m.type === "command_result");
  const terminalTrace = [...messages]
    .reverse()
    .find((m) => m.type === "trace" && ["done", "error", "stopped"].includes(m.metadata?.phase || ""));
  const result = resultMessage ? parseCommandResult(resultMessage) : null;
  const timedOut = resultMessage?.metadata?.timed_out === "true";
  const terminalPhase = terminalTrace?.metadata?.phase || "";
  const stopped = resultMessage?.metadata?.stopped === "true" || terminalPhase === "stopped";
  const doneWithoutResult = !resultMessage && terminalPhase === "done";
  const ok = Boolean((resultMessage && !stopped && !timedOut && result?.exitCode === "0") || doneWithoutResult);
  const failed = Boolean((resultMessage && !stopped && !ok) || terminalPhase === "error");
  const duration = durationMsLabel(resultMessage?.metadata?.duration_ms || terminalTrace?.metadata?.duration_ms);
  const commandID = command?.id || resultMessage?.metadata?.command_id || "pending";
  const source = command?.sender_id || resultMessage?.target_id || "room";
  const target = command?.target_id || resultMessage?.sender_id || "Bridge";
  const cwd = resultMessage?.metadata?.cwd || command?.metadata?.cwd || "";
  const timeout = command?.metadata?.timeout_ms ? durationMsLabel(command.metadata.timeout_ms) : "";
  const sourceTint = agentTint(hueFromID(source), dark);
  const targetTint = agentTint(hueFromID(target), dark);
  const [tab, setTab] = useState<"stdout" | "stderr">("stdout");
  const hasStderr = Boolean(result?.stderr && result.stderr.trim().length > 0);
  const running = !resultMessage && !terminalTrace;
  const canStop = Boolean(running && onStop && target && commandID !== "pending");
  // 命令卡片默认收起成一行摘要(命令 + 状态 + 耗时),点击展开完整输入输出;
  // 运行中的命令在收起态也能从状态 chip 看到 spinner。
  const [open, setOpen] = useState(false);
  const snippet = commandSnippet(command?.content || "");

  return (
    <div
      className={`cmd ${ok ? "is-ok" : stopped ? "is-stopped" : failed ? "is-failed" : "is-running"}${embedded ? " cmd-embedded" : ""}`}
    >
      <button
        type="button"
        className={`cmd-bar${canStop ? " cmd-bar-busy" : ""}`}
        onClick={() => setOpen((v) => !v)}
        aria-expanded={open}
      >
        <span className="cmd-id">
          <Icon name="terminal" size={14} />
          {open ? (
            <>
              <span className="cmd-run">Command Run</span>
              <span className="cmd-hash">{commandID}</span>
            </>
          ) : (
            <code className="cmd-snippet" title={command?.content || ""}>
              {snippet || "(command pending)"}
            </code>
          )}
        </span>
        <span className="cmd-route">
          <span style={{ color: sourceTint.text }}>{source}</span>
          <Icon name="chevronRight" size={12} />
          <span style={{ color: targetTint.text }}>{target}</span>
        </span>
        <span className="cmd-flex" />
        <span className="cmd-status">
          {running ? (
            <>
              <span className="cmd-spinner" /> running
            </>
          ) : stopped ? (
            <Chip tone="warn">
              <Icon name="stop" size={12} /> stopped
            </Chip>
          ) : failed ? (
            <Chip tone="danger">exit {result?.exitCode || "?"}</Chip>
          ) : doneWithoutResult ? (
            <Chip tone="ok">
              <Icon name="check" size={12} /> done
            </Chip>
          ) : (
            <Chip tone="ok">
              <Icon name="check" size={12} /> exit {result?.exitCode || "0"}
            </Chip>
          )}
        </span>
        {duration && <Chip tone="mono">{duration}</Chip>}
        {open && timeout && <Chip tone="mono">timeout {timeout}</Chip>}
        <Icon name={open ? "chevronDown" : "chevronRight"} size={14} />
      </button>
      {canStop && (
        <button
          type="button"
          className="cmd-stop"
          onClick={() => onStop?.(target, undefined, commandID)}
          title="停止命令"
        >
          <Icon name="stop" size={13} /> 停止
        </button>
      )}

      {open && (
      <div className="cmd-body">
        <div className="cmd-pane cmd-pane-in">
          <div className="cmd-pane-head">
            <span>COMMAND</span>
            {cwd && <span className="cmd-cwd">{cwd}</span>}
          </div>
          <pre className="cmd-pre">{command?.content || "waiting for command payload"}</pre>
        </div>
        <div className="cmd-pane cmd-pane-out">
          <div className="cmd-pane-head">
            <div className="cmd-tabs">
              <button
                type="button"
                className={tab === "stdout" ? "on" : ""}
                onClick={() => setTab("stdout")}
              >
                stdout
              </button>
              {hasStderr && (
                <button
                  type="button"
                  className={tab === "stderr" ? "on" : ""}
                  onClick={() => setTab("stderr")}
                >
                  stderr <span className="cmd-tab-dot" />
                </button>
              )}
            </div>
          </div>
          {running ? (
            <pre className="cmd-pre cmd-streaming">
              capturing output
              <span className="dots">
                <i />
                <i />
                <i />
              </span>
            </pre>
          ) : tab === "stdout" ? (
            <pre className="cmd-pre">{result?.stdout || "(no output)"}</pre>
          ) : (
            <pre className="cmd-pre cmd-stderr">{result?.stderr || ""}</pre>
          )}
        </div>
      </div>
      )}
    </div>
  );
}

// commandSnippet 取命令首行做收起态摘要,多行命令以 … 提示截断。
function commandSnippet(content: string): string {
  const trimmed = content.trim();
  if (!trimmed) return "";
  const firstLine = trimmed.split("\n", 1)[0];
  const cut = firstLine.length > 120 ? `${firstLine.slice(0, 117)}…` : firstLine;
  return trimmed.includes("\n") && cut === firstLine ? `${cut} …` : cut;
}

/* ── Permission approval card ─────────────────────────────────────── */

export type PermissionReply = "allow_once" | "allow_always" | "deny";

// parsePatternList 解析 metadata.always / metadata.patterns 的两种编码:
// JSON 数组(候选含多行精确命令时, 防止按行拆出假模式)优先, 换行分隔兜底
// (旧 bridge / 单行候选)。与 Go 侧 models.EncodePatternList 对偶。
export function parsePatternList(raw: string | undefined): string[] {
  const t = (raw || "").trim();
  if (!t) return [];
  if (t.startsWith("[")) {
    try {
      const arr: unknown = JSON.parse(t);
      if (Array.isArray(arr)) {
        return arr
          .filter((p): p is string => typeof p === "string" && p.trim() !== "")
          .map((p) => p.trim());
      }
    } catch {
      // 非法 JSON: 落回按行拆。
    }
  }
  return t
    .split("\n")
    .map((s) => s.trim())
    .filter(Boolean);
}

// encodePatternList 编码点选结果回传给 bridge: 含换行的模式用 JSON 数组,
// 否则保持换行分隔(旧 bridge 可解析)。
export function encodePatternList(patterns: string[]): string {
  return patterns.some((p) => p.includes("\n")) ? JSON.stringify(patterns) : patterns.join("\n");
}

// permissionResolutions 从消息列表里收集审批决议:每条 control(permission_reply)
// 都被 relay 持久化并广播,刷新拉历史、其他标签页实时收流都能拿到。返回
// permission_id -> 决议详情(reply/审批人/时间/点选模式)的映射(同 id 多条时
// 后者覆盖),供卡片回填"已处理"态与 Approvals 页签展示细节——没有它,审批
// 状态只活在点击者的组件 state 里,一刷新就全部回到"待审批"。
export type PermissionResolution = {
  reply: PermissionReply;
  by: string;
  at?: string;
  patterns: string[];
};

export function permissionResolutions(messages: ChatMessage[]): Map<string, PermissionResolution> {
  const out = new Map<string, PermissionResolution>();
  for (const msg of messages) {
    if (msg.type !== "control") continue;
    if (msg.metadata?.operation !== "permission_reply") continue;
    const id = msg.metadata?.permission_id;
    const reply = normalizeReply(msg.metadata?.reply);
    if (!id || !reply) continue;
    out.set(id, {
      reply,
      by: msg.sender_id || "",
      at: msg.created_at,
      patterns: parsePatternList(msg.metadata?.patterns),
    });
  }
  return out;
}

// splitPermissions 把房间消息里的审批请求 trace(phase=permission_request,按
// permission_id 去重、保持时间序)按决议状态拆成两个视图:无决议的 pending 交给
// PermissionToasts 从右侧弹入,有决议的 resolved(含审批人/时间/点选模式)交给
// Approvals 页签。决议优先级:消息自带元数据(老协议兜底) > control 消息推导的持久决议。
// 另从 allow_always 决议里聚合 rules:审批人点选的「永久放行」模式列表
// (control metadata.patterns;老消息无点选时退回请求侧的候选列表 metadata.always),
// 与 resolved 一起交给 Approvals 页签展示审批细节。
export type ResolvedPermission = {
  msg: ChatMessage;
  reply: PermissionReply;
  by?: string;
  at?: string;
  patterns?: string[];
};
export type AlwaysRule = { tool: string; pattern: string; by: string; agent: string };

export function splitPermissions(messages: ChatMessage[]): {
  pending: ChatMessage[];
  resolved: ResolvedPermission[];
  rules: AlwaysRule[];
} {
  const resolutions = permissionResolutions(messages);
  const seen = new Set<string>();
  const pending: ChatMessage[] = [];
  const resolved: ResolvedPermission[] = [];
  const requestById = new Map<string, ChatMessage>();
  for (const msg of messages) {
    if (msg.type !== "trace" || msg.metadata?.phase !== "permission_request") continue;
    const id = msg.metadata?.permission_id;
    if (id && !requestById.has(id)) requestById.set(id, msg);
    const key = permissionKey(msg);
    if (seen.has(key)) continue;
    seen.add(key);
    const inline = normalizeReply(msg.metadata?.resolved || msg.metadata?.reply);
    const resolution = resolutions.get(msg.metadata?.permission_id || "");
    if (inline) resolved.push({ msg, reply: inline });
    else if (resolution) resolved.push({ msg, ...resolution });
    else pending.push(msg);
  }

  const rules: AlwaysRule[] = [];
  const seenRule = new Set<string>();
  for (const msg of messages) {
    if (msg.type !== "control" || msg.metadata?.operation !== "permission_reply") continue;
    if (normalizeReply(msg.metadata?.reply) !== "allow_always") continue;
    const request = requestById.get(msg.metadata?.permission_id || "");
    const tool = request?.metadata?.tool || "";
    const agent = request?.sender_id || msg.target_id || "";
    let patterns = parsePatternList(msg.metadata?.patterns);
    if (patterns.length === 0) patterns = parsePatternList(request?.metadata?.always);
    for (const pattern of patterns) {
      const key = `${agent} ${tool} ${pattern}`;
      if (seenRule.has(key)) continue;
      seenRule.add(key);
      rules.push({ tool, pattern, by: msg.sender_id || "", agent });
    }
  }
  return { pending, resolved, rules };
}

// PermissionToasts 把待审批请求渲染成右侧弹入的浮层栈:新请求带滑入动画,
// 点击任一按钮(或他人/其他标签页先一步处理)后播放滑出动画再移除。已结束的
// 请求绝不进入此栈——初次加载时已决议的直接归 Approvals 页签。
type PermToastItem = { msg: ChatMessage; leaving: boolean };

export function PermissionToasts({
  pending,
  canApprove,
  onReply,
}: {
  pending: ChatMessage[];
  canApprove: boolean;
  onReply?: (message: ChatMessage, reply: PermissionReply, patterns?: string[]) => void;
}) {
  const [items, setItems] = useState<PermToastItem[]>([]);
  // 每张卡片「总是批准」候选模式的点选状态(permissionKey -> 选中集合)。默认
  // 选中程序前缀类候选(echo * / curl * / Tool(*)),精确命令默认不选——它只在
  // 没有任何前缀候选时才作为唯一默认。
  const [picked, setPicked] = useState<Map<string, Set<string>>>(new Map());
  // dismissed: 本地已点过按钮的请求 id。control 回流有延迟,在那个窗口里该请求
  // 在外部看仍是 pending,靠这个集合阻止它被重新加回栈。
  const dismissed = useRef(new Set<string>());
  const timers = useRef(new Set<string>());

  // 同步外部状态 -> 栈:新 pending 入栈(触发进入动画),决议已落地的标记 leaving
  // (触发退出动画)。以 key 串作为 effect 依赖,避免数组引用每渲染都变。
  const pendingSig = pending.map((msg) => permissionKey(msg)).join("|");
  useEffect(() => {
    const pendingKeys = new Set(pending.map((msg) => permissionKey(msg)));
    setItems((prev) => {
      const prevKeys = new Set(prev.map((it) => permissionKey(it.msg)));
      let next = prev;
      const marked = prev.map((it) =>
        !it.leaving && !pendingKeys.has(permissionKey(it.msg)) ? { ...it, leaving: true } : it,
      );
      if (marked.some((it, i) => it !== prev[i])) next = marked;
      const additions = pending.filter((msg) => {
        const key = permissionKey(msg);
        return !prevKeys.has(key) && !dismissed.current.has(key);
      });
      if (additions.length > 0) {
        next = [...next, ...additions.map((msg) => ({ msg, leaving: false }))];
      }
      return next;
    });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [pendingSig]);

  // leaving 的条目等退出动画播完后真正移除。
  useEffect(() => {
    for (const it of items) {
      if (!it.leaving) continue;
      const key = permissionKey(it.msg);
      if (timers.current.has(key)) continue;
      timers.current.add(key);
      window.setTimeout(() => {
        timers.current.delete(key);
        setItems((prev) => prev.filter((p) => permissionKey(p.msg) !== key));
      }, 300);
    }
  }, [items]);

  function reply(msg: ChatMessage, decision: PermissionReply, patterns?: string[]) {
    if (!canApprove) return;
    const key = permissionKey(msg);
    if (dismissed.current.has(key)) return;
    dismissed.current.add(key);
    onReply?.(msg, decision, patterns);
    // 立即播放退出动画,不等 control 消息回流。
    setItems((prev) =>
      prev.map((it) => (permissionKey(it.msg) === key ? { ...it, leaving: true } : it)),
    );
  }

  // alwaysCandidates 解析请求 metadata.always 成候选模式列表(JSON/换行两种编码)。
  function alwaysCandidates(msg: ChatMessage): string[] {
    return parsePatternList(msg.metadata?.always);
  }

  // selectedFor 取一张卡片当前选中的模式集合;无显式点选时按默认口径推导。
  function selectedFor(msg: ChatMessage, candidates: string[], input: string): Set<string> {
    const explicit = picked.get(permissionKey(msg));
    if (explicit) return explicit;
    const prefixes = candidates.filter((c) => c !== input);
    return new Set(prefixes.length > 0 ? prefixes : candidates);
  }

  function togglePattern(msg: ChatMessage, candidates: string[], input: string, pattern: string) {
    const key = permissionKey(msg);
    setPicked((prev) => {
      const next = new Map(prev);
      const cur = new Set(next.get(key) ?? selectedFor(msg, candidates, input));
      if (cur.has(pattern)) cur.delete(pattern);
      else cur.add(pattern);
      next.set(key, cur);
      return next;
    });
  }

  if (items.length === 0) return null;

  return (
    <div className="perm-toasts" role="region" aria-label="待审批的权限请求">
      {items.map((it) => {
        const msg = it.msg;
        const tool = msg.metadata?.tool || "";
        const rawInput = (msg.metadata?.input || "").trim();
        // content 兜底仅用于老消息;新协议命令在 metadata.input。占位文不当命令显示。
        const input =
          rawInput || (msg.content === "permission requested" ? "" : (msg.content || "").trim());
        const permissionID = msg.metadata?.permission_id || "";
        const shortID = permissionID ? permissionID.slice(-8) : "";
        const candidates = alwaysCandidates(msg);
        const selected = selectedFor(msg, candidates, input);
        return (
          <div key={permissionKey(msg)} className={`perm-toast${it.leaving ? " is-leaving" : ""}`}>
            <div className="perm-bar">
              <span className="perm-id">
                <Icon name="shield" size={14} />
                <span className="perm-run">权限审批</span>
                {shortID && <span className="perm-hash" title={permissionID}>#{shortID}</span>}
              </span>
              <span className="perm-flex" />
              <span className="perm-status">
                <Chip tone="warn">
                  <Icon name="alert" size={12} /> 待审批
                </Chip>
              </span>
            </div>

            <div className="perm-body">
              <div className="perm-meta">
                <span className="perm-agent">{msg.sender_id || "agent"}</span>
                <span className="perm-sub">请求执行{tool ? "" : "操作"}</span>
                {tool && <span className="perm-tool">{tool}</span>}
              </div>
              {input ? (
                <pre className="perm-pre">{input}</pre>
              ) : (
                <div className="perm-empty">未提供命令详情</div>
              )}
              {candidates.length > 0 && (
                <div className="perm-always-hint">
                  <span className="perm-always-cap">
                    <Icon name="shieldCheck" size={12} /> 「总是批准」将放行(点选):
                  </span>
                  <div className="perm-patterns">
                    {candidates.map((pattern) => {
                      const exact = pattern === input;
                      const on = selected.has(pattern);
                      // 多行精确命令压平展示, 超长截断; 完整内容在 title 里。
                      const flat = pattern.replace(/\s+/g, " ");
                      const label = exact && flat.length > 42 ? `${flat.slice(0, 39)}…` : flat;
                      return (
                        <button
                          key={pattern}
                          type="button"
                          className={`perm-pattern${on ? " is-on" : ""}`}
                          title={exact ? `精确匹配整条命令:\n${pattern}` : `前缀放行: ${pattern}`}
                          onClick={() => togglePattern(msg, candidates, input, pattern)}
                          disabled={!canApprove}
                        >
                          <Icon name={on ? "check" : "plus"} size={10} />
                          <code>{exact ? `= ${label}` : label}</code>
                        </button>
                      );
                    })}
                  </div>
                </div>
              )}
            </div>

            <div className="perm-actions">
              {!canApprove ? (
                <span className="perm-hint">
                  <Icon name="lock" size={13} /> 等待房主审批
                </span>
              ) : (
                <>
                  <button
                    type="button"
                    className="perm-btn perm-allow"
                    onClick={() => reply(msg, "allow_once")}
                  >
                    <Icon name="check" size={13} /> 批准一次
                  </button>
                  <button
                    type="button"
                    className="perm-btn perm-always"
                    disabled={candidates.length > 0 && selected.size === 0}
                    title={
                      candidates.length > 0 && selected.size === 0
                        ? "先点选至少一个放行模式"
                        : undefined
                    }
                    onClick={() => reply(msg, "allow_always", Array.from(selected))}
                  >
                    <Icon name="shieldCheck" size={13} /> 总是批准
                  </button>
                  <button
                    type="button"
                    className="perm-btn perm-deny"
                    onClick={() => reply(msg, "deny")}
                  >
                    <Icon name="x" size={13} /> 拒绝
                  </button>
                </>
              )}
            </div>
          </div>
        );
      })}
    </div>
  );
}

function normalizeReply(value: string | undefined): PermissionReply | null {
  if (value === "allow_once" || value === "allow_always" || value === "deny") return value;
  return null;
}

export function parseCommandResult(message: ChatMessage): { exitCode: string; stdout: string; stderr: string } {
  const content = message.content || "";
  const exitCode = message.metadata?.exit_code || content.match(/^exit_code=(\S+)/m)?.[1] || "";
  return {
    exitCode,
    stdout: normalizeTerminalOutput(extractTerminalSection(content, "stdout")),
    stderr: normalizeTerminalOutput(extractTerminalSection(content, "stderr")),
  };
}

function extractTerminalSection(content: string, label: "stdout" | "stderr"): string {
  const pattern = new RegExp(`(?:^|\\n)${label}:\\n?([\\s\\S]*?)(?=\\n(?:stdout|stderr):\\n?|$)`, "i");
  return content.match(pattern)?.[1] || "";
}

function normalizeTerminalOutput(value: string): string {
  const trimmed = value.replace(/\s+$/g, "");
  return trimmed === "(no output)" ? "" : trimmed;
}

/* ── Sidebar trace summary ───────────────────────────────────────── */

export function SidebarTrace({ trace, dark }: { trace: ChatMessage; dark: boolean }) {
  const phase = (trace.metadata?.phase || "thinking") as string;
  const tool = trace.metadata?.tool || "";
  const tone: "live" | "ok" | "idle" =
    phase === "done" ? "ok" : phase === "thinking" ? "live" : "idle";
  const text = traceText(trace);
  const tint = agentTint(hueFromID(trace.sender_id), dark);
  return (
    <div className="side-act" style={{ borderColor: tint.line }}>
      <StatusDot tone={tone} pulse={phase === "thinking"} />
      <div className="side-act-body">
        <strong style={{ color: tint.text }}>
          {trace.sender_id}
          {tool ? ` · ${tool}` : ""}
        </strong>
        <span>{text}</span>
      </div>
    </div>
  );
}

function traceText(message: ChatMessage): string {
  const phase = message.metadata?.phase || "";
  const content = (message.content || "").trim();
  if (phase === "thinking") return content ? `Thinking · ${content}` : "Agent is thinking…";
  if (phase === "done") return "Reply completed";
  if (phase === "error") return `Reply failed: ${message.metadata?.error || "unknown error"}`;
  if (phase === "system") return content || "Session started";
  if (phase === "text") return content || "Drafting reply…";
  if (phase === "tool_use") {
    const tool = message.metadata?.tool || "tool";
    return content ? `${tool}: ${content}` : `Calling ${tool}…`;
  }
  if (phase === "tool_result") {
    const ok = message.metadata?.error === "true" ? "error" : "result";
    return content ? `${ok}: ${content}` : `Tool ${ok}`;
  }
  if (phase === "executing") return content || "Executing command";
  return content || phase || "…";
}
