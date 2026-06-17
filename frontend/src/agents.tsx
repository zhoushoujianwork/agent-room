import { useCallback, useEffect, useState } from "react";
import { Icon } from "./icons";
import { StatusDot, TopBrand } from "./ui";
import { PageNav, UserMenu } from "./home";
import { useAuth } from "./auth";
import { copyText, relativeAge, shortRoomID } from "./lib";
import {
  createAgentToken,
  deleteAgent,
  getAgentConfig,
  listAgents,
  listAgentTokens,
  putAgentConfig,
  revokeAgentToken,
} from "./api";
import type { Agent, AgentConfig, AgentToken, CreatedAgentToken } from "./types";

interface AgentsPageProps {
  isAdmin: boolean;
  onOpenHome: () => void;
  onOpenRooms: () => void;
  onOpenAdmin: () => void;
  onEnterRoom: (room?: string) => void;
  onShowToast: (message: string) => void;
}

type AgentsTab = "agents" | "tokens";

// startCommand 渲染「拿着这个 token 启动 bridge」的示例命令。token 明文只在
// 生成弹层里出现一次，因此该命令也只在那一刻可复制。
function currentRelayOrigin(): string {
  if (typeof window === "undefined") return "";
  return window.location.origin || "";
}

function startCommand(token: string, mode: "agent" | "executor"): string {
  const relay = currentRelayOrigin();
  const relayArg = relay ? ` -relay ${relay}` : "";
  const modeArg = mode === "executor" ? " -mode executor" : "";
  return `AGENT_ROOM_AGENT_TOKEN=${token} agent-room bridge${relayArg}${modeArg}`;
}

export function AgentsPage({
  isAdmin,
  onOpenHome,
  onOpenRooms,
  onOpenAdmin,
  onEnterRoom,
  onShowToast,
}: AgentsPageProps) {
  const { me, signOut } = useAuth();
  const [tab, setTab] = useState<AgentsTab>("agents");

  const right =
    me.auth_enabled && me.authenticated ? (
      <div className="home-nav-actions">
        <PageNav
          current="agents"
          isAdmin={isAdmin}
          onOpenHome={onOpenHome}
          onOpenRooms={onOpenRooms}
          onOpenAdmin={onOpenAdmin}
          onOpenAgents={() => {}}
        />
        <UserMenu user={me.user} isAdmin={isAdmin} onSignOut={signOut} />
      </div>
    ) : null;

  return (
    <div className="home">
      <TopBrand right={right} />
      <main className="home-main">
        <div className="home-hero">
          <span className="kicker">
            <Icon name="bot" size={14} /> 我的 Agents
          </span>
          <h1>管理我的 Agent</h1>
          <p>
            这里集中管理你名下的 Agent：查看在线状态与所在房间、复制 ID、解绑，
            以及生成用于把本机 bridge 绑定到账号的接入 Token。
          </p>
        </div>

        <div className="admin-tabs" role="tablist" aria-label="Agent 管理分区">
          <button
            type="button"
            className={tab === "agents" ? "on" : ""}
            onClick={() => setTab("agents")}
            role="tab"
            aria-selected={tab === "agents"}
          >
            <Icon name="bot" size={15} /> 我的 Agents
          </button>
          <button
            type="button"
            className={tab === "tokens" ? "on" : ""}
            onClick={() => setTab("tokens")}
            role="tab"
            aria-selected={tab === "tokens"}
          >
            <Icon name="lock" size={15} /> 接入 Token
          </button>
        </div>

        {tab === "agents" ? (
          <AgentsList onEnterRoom={onEnterRoom} onShowToast={onShowToast} onGoTokens={() => setTab("tokens")} />
        ) : (
          <TokensPanel onShowToast={onShowToast} />
        )}
      </main>
    </div>
  );
}

/* ── 我的 Agents 列表 ──────────────────────────────────────────────── */

function AgentsList({
  onEnterRoom,
  onShowToast,
  onGoTokens,
}: {
  onEnterRoom: (room?: string) => void;
  onShowToast: (message: string) => void;
  onGoTokens: () => void;
}) {
  const [agents, setAgents] = useState<Agent[] | null>(null);
  const [error, setError] = useState(false);
  const [expanded, setExpanded] = useState<string | null>(null);
  const [confirming, setConfirming] = useState<string | null>(null);

  const refresh = useCallback(() => {
    setError(false);
    listAgents()
      .then((list) => setAgents(list))
      .catch(() => {
        setAgents([]);
        setError(true);
      });
  }, []);

  useEffect(() => {
    refresh();
    const timer = window.setInterval(refresh, 3000);
    return () => window.clearInterval(timer);
  }, [refresh]);

  const unbind = useCallback(
    (agentID: string) => {
      deleteAgent(agentID)
        .then(() => {
          onShowToast("已解绑该 Agent");
          setConfirming(null);
          setExpanded(null);
          refresh();
        })
        .catch(() => onShowToast("解绑失败，请稍后重试"));
    },
    [onShowToast, refresh],
  );

  function copyID(agentID: string) {
    copyText(agentID).then(
      () => onShowToast("Agent ID 已复制"),
      () => onShowToast("复制失败"),
    );
  }

  return (
    <section className="home-rooms" aria-label="我的 Agents">
      <div className="home-rooms-head">
        <span>名下 Agent</span>
        <span className="count">{agents ? agents.length : "…"}</span>
        <button
          type="button"
          className="icon-btn icon-btn-ghost"
          onClick={refresh}
          title="刷新"
          aria-label="刷新"
          style={{ marginLeft: "auto" }}
        >
          <Icon name="refresh" size={15} />
        </button>
      </div>

      {agents === null ? (
        <p className="home-fine">正在加载 Agent 列表…</p>
      ) : error ? (
        <p className="home-fine">加载失败：可能未登录或服务暂不可用。</p>
      ) : agents.length === 0 ? (
        <AgentsEmpty onGoTokens={onGoTokens} />
      ) : (
        <ul className="agent-mgr-list">
          {agents.map((agent) => (
            <li key={agent.agent_id} className="agent-mgr-row">
              <div className="agent-mgr-main">
                <span className={agent.online ? "agent-mgr-status on" : "agent-mgr-status"}>
                  <StatusDot tone={agent.online ? "live" : "off"} pulse={agent.online} size={8} />
                </span>
                <div className="agent-mgr-text">
                  <div className="agent-mgr-title">
                    <strong>{agent.label || agent.agent_id}</strong>
                    {agent.provider && <span className="agent-mgr-provider">{agent.provider}</span>}
                  </div>
                  <div className="agent-mgr-meta">
                    <button
                      type="button"
                      className="agent-mgr-id"
                      onClick={() => copyID(agent.agent_id)}
                      title="点击复制完整 Agent ID"
                    >
                      <Icon name="copy" size={12} /> {shortAgentID(agent.agent_id)}
                    </button>
                    <span>
                      {agent.online ? "在线" : "离线"}
                      {!agent.online && relativeAge(agent.last_seen_at)
                        ? ` · 最后在线 ${relativeAge(agent.last_seen_at)}`
                        : ""}
                    </span>
                  </div>
                  {agent.rooms && agent.rooms.length > 0 ? (
                    <div className="agent-mgr-rooms">
                      {agent.rooms.map((roomID) => (
                        <button
                          key={roomID}
                          type="button"
                          className="agent-mgr-room"
                          onClick={() => onEnterRoom(roomID)}
                          title={`进入房间 ${roomID}`}
                        >
                          <Icon name="hash" size={12} /> {shortRoomID(roomID)}
                        </button>
                      ))}
                    </div>
                  ) : (
                    <span className="agent-mgr-no-rooms">未加入任何房间</span>
                  )}
                </div>
              </div>

              <div className="agent-mgr-actions">
                <button
                  type="button"
                  className="btn btn-sm"
                  onClick={() => setExpanded((cur) => (cur === agent.agent_id ? null : agent.agent_id))}
                  aria-expanded={expanded === agent.agent_id}
                >
                  <Icon name="settings" size={14} /> 配置
                </button>
                {confirming === agent.agent_id ? (
                  <span className="agent-mgr-confirm">
                    <button type="button" className="btn btn-sm btn-danger" onClick={() => unbind(agent.agent_id)}>
                      确认解绑
                    </button>
                    <button type="button" className="btn btn-sm" onClick={() => setConfirming(null)}>
                      取消
                    </button>
                  </span>
                ) : (
                  <button
                    type="button"
                    className="btn btn-sm btn-danger"
                    onClick={() => setConfirming(agent.agent_id)}
                  >
                    <Icon name="trash" size={14} /> 解绑
                  </button>
                )}
              </div>

              {expanded === agent.agent_id && (
                <div className="agent-mgr-config">
                  <AgentConfigForm agentId={agent.agent_id} />
                </div>
              )}
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

// MODEL_SUGGESTIONS 仅作为 <datalist> 建议项，用户可自由输入任意自定义模型名。
const MODEL_SUGGESTIONS = ["claude-sonnet-4-6", "claude-opus-4-8", "claude-haiku-4-5"];

// AgentConfigForm 对接 GET/PUT /v1/agents/{id}/config：挂载即加载已存配置，
// 编辑后部分更新（字段缺席=保持，api_key 空串=清除）。安全红线：api_key 输入值
// 绝不写入 console / localStorage，也不在 DOM 上明文回显——仅展示后端脱敏串。
function AgentConfigForm({ agentId }: { agentId: string }) {
  const [config, setConfig] = useState<AgentConfig | null>(null);
  const [loadError, setLoadError] = useState(false);
  const [loading, setLoading] = useState(true);

  const [model, setModel] = useState("");
  const [apiBaseURL, setApiBaseURL] = useState("");
  const [apiKey, setApiKey] = useState("");
  const [clearKey, setClearKey] = useState(false);

  const [saving, setSaving] = useState(false);
  const [formError, setFormError] = useState<string | null>(null);
  const [success, setSuccess] = useState(false);

  const load = useCallback(() => {
    setLoading(true);
    setLoadError(false);
    getAgentConfig(agentId)
      .then((cfg) => {
        setConfig(cfg);
        setModel(cfg.model || "");
        setApiBaseURL(cfg.api_base_url || "");
        setApiKey("");
        setClearKey(false);
        setFormError(null);
        setSuccess(false);
      })
      .catch(() => setLoadError(true))
      .finally(() => setLoading(false));
  }, [agentId]);

  useEffect(() => {
    load();
  }, [load]);

  const save = useCallback(() => {
    const trimmedBase = apiBaseURL.trim();
    if (trimmedBase && !/^https?:\/\//i.test(trimmedBase)) {
      setSuccess(false);
      setFormError("API 地址需以 http:// 或 https:// 开头。");
      return;
    }

    // model / api_base_url 总是携带（存在即覆盖，空串=清空）；api_key 三态：
    // 待清除 → 空串；输入了新值 → 携带其值；否则完全不携带（保持现有 key）。
    const patch: { model?: string; api_base_url?: string; api_key?: string } = {
      model: model.trim(),
      api_base_url: trimmedBase,
    };
    if (clearKey) {
      patch.api_key = "";
    } else if (apiKey !== "") {
      patch.api_key = apiKey;
    }

    setSaving(true);
    setFormError(null);
    setSuccess(false);
    putAgentConfig(agentId, patch)
      .then((cfg) => {
        setConfig(cfg);
        setModel(cfg.model || "");
        setApiBaseURL(cfg.api_base_url || "");
        setApiKey("");
        setClearKey(false);
        setSuccess(true);
      })
      .catch((err: unknown) => {
        setFormError(err instanceof Error ? err.message : "保存失败，请稍后重试。");
      })
      .finally(() => setSaving(false));
  }, [agentId, model, apiBaseURL, apiKey, clearKey]);

  if (loading) {
    return <p className="home-fine">加载配置…</p>;
  }

  if (loadError || !config) {
    return (
      <div className="agent-config-form">
        <div className="agent-config-error" role="alert">
          <Icon name="alert" size={14} /> 加载配置失败，可能未登录或服务暂不可用。
        </div>
        <button type="button" className="btn btn-sm" onClick={load}>
          <Icon name="refresh" size={14} /> 重试
        </button>
      </div>
    );
  }

  const masked = config.api_key_masked;
  const keyPlaceholder = clearKey
    ? "保存后将清除"
    : masked
      ? `当前已设置：${masked}（输入新值覆盖）`
      : "未设置";
  const hasRuntimeReport = Boolean(config.runtime_updated_at);

  return (
    <div className="agent-config-form">
      <div className="agent-config-form-head">
        <Icon name="settings" size={14} />
        <strong>Agent 启动配置</strong>
      </div>
      <div className="agent-runtime-config">
        <span className="agent-runtime-title">
          <Icon name="activity" size={13} /> 当前运行
        </span>
        {hasRuntimeReport ? (
          <div className="agent-runtime-grid">
            <span>provider: {config.runtime_provider || "unknown"}</span>
            <span>model: {config.runtime_model || "bridge 默认"}</span>
            <span>api: {config.runtime_api_base_url || "默认官方入口"}</span>
            <span>key: {config.runtime_api_key_set ? "已注入" : "未注入"}</span>
          </div>
        ) : (
          <span className="agent-runtime-empty">等待在线 bridge 上报当前配置…</span>
        )}
      </div>

      <label className="agent-config-field">
        <span>模型 model</span>
        <input
          type="text"
          value={model}
          onChange={(e) => setModel(e.target.value)}
          list={`model-suggestions-${agentId}`}
          placeholder="留空使用 bridge 本地默认"
          spellCheck={false}
          autoComplete="off"
        />
        <datalist id={`model-suggestions-${agentId}`}>
          {MODEL_SUGGESTIONS.map((m) => (
            <option key={m} value={m} />
          ))}
        </datalist>
      </label>

      <label className="agent-config-field">
        <span>API 地址 api_base_url</span>
        <input
          type="text"
          value={apiBaseURL}
          onChange={(e) => setApiBaseURL(e.target.value)}
          placeholder="https://…（Anthropic 兼容网关，留空走官方）"
          spellCheck={false}
          autoComplete="off"
        />
      </label>

      <label className="agent-config-field">
        <span>API Key</span>
        <div className="agent-config-key-row">
          <input
            type="password"
            value={apiKey}
            onChange={(e) => setApiKey(e.target.value)}
            placeholder={keyPlaceholder}
            disabled={clearKey}
            autoComplete="off"
          />
          {clearKey ? (
            <button
              type="button"
              className="btn btn-sm"
              onClick={() => setClearKey(false)}
            >
              取消清除
            </button>
          ) : (
            <button
              type="button"
              className="btn btn-sm btn-danger"
              onClick={() => {
                setApiKey("");
                setClearKey(true);
              }}
              disabled={!masked}
              title={masked ? "保存后清除已存 Key" : "当前未设置 Key"}
            >
              清除已存 Key
            </button>
          )}
        </div>
      </label>

      {formError && (
        <div className="agent-config-error" role="alert">
          <Icon name="alert" size={14} /> {formError}
        </div>
      )}
      {success && (
        <div className="agent-config-success" role="status">
          <Icon name="check" size={14} /> 已保存。agent 在线时已即时下发，下一次生成开始生效。
        </div>
      )}

      <div className="agent-config-form-foot">
        <button type="button" className="btn btn-primary btn-sm" onClick={save} disabled={saving}>
          {saving ? "保存中…" : "保存配置"}
        </button>
      </div>
    </div>
  );
}

function AgentsEmpty({ onGoTokens }: { onGoTokens: () => void }) {
  return (
    <div className="rooms-empty">
      <Icon name="bot" size={18} />
      <strong>还没有绑定的 Agent</strong>
      <span>
        生成一个接入 Token，用它启动本机 bridge，Agent 连接后会出现在这里（含离线）。
      </span>
      <button type="button" className="btn" onClick={onGoTokens} style={{ marginTop: 10 }}>
        <Icon name="lock" size={14} /> 去生成接入 Token
      </button>
    </div>
  );
}

function shortAgentID(value: string): string {
  if (value.length <= 16) return value;
  return `${value.slice(0, 8)}…${value.slice(-4)}`;
}

/* ── 接入 Token ────────────────────────────────────────────────────── */

function TokensPanel({ onShowToast }: { onShowToast: (message: string) => void }) {
  const [tokens, setTokens] = useState<AgentToken[] | null>(null);
  const [error, setError] = useState(false);
  const [note, setNote] = useState("");
  const [creating, setCreating] = useState(false);
  const [created, setCreated] = useState<CreatedAgentToken | null>(null);
  const [confirming, setConfirming] = useState<string | null>(null);

  const refresh = useCallback(() => {
    setError(false);
    listAgentTokens()
      .then((list) => setTokens(list))
      .catch(() => {
        setTokens([]);
        setError(true);
      });
  }, []);

  useEffect(() => {
    refresh();
  }, [refresh]);

  const generate = useCallback(() => {
    setCreating(true);
    createAgentToken(note.trim() || undefined)
      .then((res) => {
        setCreated(res);
        setNote("");
        refresh();
      })
      .catch(() => onShowToast("生成失败，请稍后重试"))
      .finally(() => setCreating(false));
  }, [note, refresh, onShowToast]);

  const revoke = useCallback(
    (prefix: string) => {
      revokeAgentToken(prefix)
        .then(() => {
          onShowToast("Token 已吊销");
          setConfirming(null);
          refresh();
        })
        .catch(() => onShowToast("吊销失败，请稍后重试"));
    },
    [onShowToast, refresh],
  );

  return (
    <section className="home-rooms" aria-label="接入 Token">
      <div className="token-create-card">
        <div className="home-card-head">
          <div>
            <h2>生成接入 Token</h2>
            <p>用它把本机 bridge 绑定到你的账号。明文只展示一次，关闭后无法再次查看。</p>
          </div>
          <Icon name="lock" size={21} />
        </div>
        <div className="token-create-form">
          <input
            value={note}
            onChange={(e) => setNote(e.target.value)}
            placeholder="备注（可选，如「办公室 Mac」）"
            aria-label="Token 备注"
            maxLength={120}
            spellCheck={false}
          />
          <button type="button" className="btn btn-primary" onClick={generate} disabled={creating}>
            {creating ? "正在生成…" : (
              <>
                <Icon name="plus" size={15} /> 生成 Token
              </>
            )}
          </button>
        </div>
      </div>

      <div className="home-rooms-head" style={{ marginTop: 20 }}>
        <span>已有 Token</span>
        <span className="count">{tokens ? tokens.length : "…"}</span>
        <button
          type="button"
          className="icon-btn icon-btn-ghost"
          onClick={refresh}
          title="刷新"
          aria-label="刷新"
          style={{ marginLeft: "auto" }}
        >
          <Icon name="refresh" size={15} />
        </button>
      </div>

      {tokens === null ? (
        <p className="home-fine">正在加载 Token 列表…</p>
      ) : error ? (
        <p className="home-fine">加载失败：可能未登录或服务暂不可用。</p>
      ) : tokens.length === 0 ? (
        <p className="home-fine">还没有接入 Token。生成一个即可绑定本机 bridge。</p>
      ) : (
        <ul className="token-list">
          {tokens.map((token) => (
            <li key={token.hash_prefix} className="token-row">
              <div className="token-row-main">
                <code className="token-prefix">{token.hash_prefix}…</code>
                {token.note && <span className="token-note">{token.note}</span>}
                <div className="token-row-meta">
                  <span>
                    <Icon name="clock" size={12} /> 创建 {relativeAge(token.created_at) || "刚刚"}
                  </span>
                  <span>
                    <Icon name="activity" size={12} />{" "}
                    {token.last_used_at ? `最近使用 ${relativeAge(token.last_used_at)}` : "从未使用"}
                  </span>
                </div>
              </div>
              {confirming === token.hash_prefix ? (
                <span className="agent-mgr-confirm">
                  <button type="button" className="btn btn-sm btn-danger" onClick={() => revoke(token.hash_prefix)}>
                    确认吊销
                  </button>
                  <button type="button" className="btn btn-sm" onClick={() => setConfirming(null)}>
                    取消
                  </button>
                </span>
              ) : (
                <button
                  type="button"
                  className="btn btn-sm btn-danger"
                  onClick={() => setConfirming(token.hash_prefix)}
                >
                  <Icon name="trash" size={14} /> 吊销
                </button>
              )}
            </li>
          ))}
        </ul>
      )}

      {created && (
        <NewTokenDialog token={created} onClose={() => setCreated(null)} onShowToast={onShowToast} />
      )}
    </section>
  );
}

// NewTokenDialog 一次性展示明文 token + 启动示例命令。关闭后无法再次查看。
function NewTokenDialog({
  token,
  onClose,
  onShowToast,
}: {
  token: CreatedAgentToken;
  onClose: () => void;
  onShowToast: (message: string) => void;
}) {
  const agentCommand = startCommand(token.token, "agent");
  const executorCommand = startCommand(token.token, "executor");

  function copyToken() {
    copyText(token.token).then(
      () => onShowToast("Token 已复制"),
      () => onShowToast("复制失败"),
    );
  }

  function copyCommand(command: string, label: string) {
    copyText(command).then(
      () => onShowToast(`${label} 启动命令已复制`),
      () => onShowToast("复制失败"),
    );
  }

  return (
    <div className="token-dialog-backdrop" role="dialog" aria-modal="true" aria-label="新生成的接入 Token">
      <div className="token-dialog">
        <div className="token-dialog-head">
          <h3>
            <Icon name="check" size={16} /> Token 已生成
          </h3>
          <button type="button" className="icon-btn icon-btn-ghost" onClick={onClose} aria-label="关闭">
            <Icon name="x" size={16} />
          </button>
        </div>
        <p className="token-dialog-warn">
          <Icon name="alert" size={14} /> 明文只展示这一次，关闭后无法再次查看，请立即复制保存。
        </p>

        <label className="token-dialog-field">
          <span>Token 明文</span>
          <div className="token-dialog-copy">
            <code>{token.token}</code>
            <button type="button" className="btn btn-sm" onClick={copyToken}>
              <Icon name="copy" size={14} /> 复制
            </button>
          </div>
        </label>

        <label className="token-dialog-field">
          <span>启动 AI Agent</span>
          <div className="token-dialog-copy">
            <code>{agentCommand}</code>
            <button type="button" className="btn btn-sm" onClick={() => copyCommand(agentCommand, "Agent")}>
              <Icon name="copy" size={14} /> 复制
            </button>
          </div>
        </label>

        <label className="token-dialog-field">
          <span>启动 Executor（不跑 AI，被 @ 时执行命令）</span>
          <div className="token-dialog-copy">
            <code>{executorCommand}</code>
            <button type="button" className="btn btn-sm" onClick={() => copyCommand(executorCommand, "Executor")}>
              <Icon name="copy" size={14} /> 复制
            </button>
          </div>
        </label>
        <p className="home-fine">
          Agent 可不指定房间，后续从页面加入；Executor 需要房间，交互启动会询问，也可追加 <code>-room &lt;room_id&gt;</code>。
        </p>

        <div className="token-dialog-foot">
          <button type="button" className="btn btn-primary" onClick={onClose}>
            我已保存，关闭
          </button>
        </div>
      </div>
    </div>
  );
}
