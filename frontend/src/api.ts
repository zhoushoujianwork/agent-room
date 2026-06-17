import type {
  AccessPersistence,
  AccessRequest,
  AdminUsersReport,
  Agent,
  AgentConfig,
  AgentToken,
  AttachmentUpload,
  ChatMessage,
  CreatedAgentToken,
  Me,
  Participant,
  Room,
  RoomSummary,
  UserRoom,
} from "./types";
import { cleanRoom } from "./lib";

async function getJSON<T>(path: string): Promise<T> {
  const res = await fetch(path, { credentials: "same-origin" });
  if (!res.ok) throw new Error(`${path} → ${res.status}`);
  return res.json() as Promise<T>;
}

async function sendJSON<T>(path: string, method: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method,
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  if (!res.ok) {
    // 透传服务端 {"error": "..."} 文案（如 400 的 AGENT_ROOM_SECRET_KEY 未配置提示），
    // 拿不到时退回纯状态码。既有调用方仍只读 message，行为向后兼容。
    let detail = "";
    try {
      const data = (await res.json()) as { error?: string };
      if (data && typeof data.error === "string") detail = data.error;
    } catch {
      // 非 JSON 响应体，保持状态码兜底
    }
    throw new Error(
      detail ? `${path} ${method} → ${res.status}: ${detail}` : `${path} ${method} → ${res.status}`,
    );
  }
  return res.json() as Promise<T>;
}

export async function loadMe(): Promise<Me> {
  try {
    return await getJSON<Me>("/v1/me");
  } catch {
    // Server doesn't ship the endpoint yet → treat auth as disabled, keep app working.
    return { authenticated: false, auth_enabled: false };
  }
}

export async function logout(): Promise<void> {
  try {
    await fetch("/auth/logout", { method: "POST", credentials: "same-origin" });
  } catch {
    // best effort
  }
}

export async function createRoom(title?: string): Promise<Room> {
  return sendJSON<Room>("/v1/rooms", "POST", title ? { title } : {});
}

export async function listMyRooms(limit = 50): Promise<UserRoom[]> {
  const data = await getJSON<UserRoom[]>(`/v1/me/rooms?limit=${limit}`);
  return data ?? [];
}

export async function rememberRoom(roomID: string): Promise<void> {
  await sendJSON<{ ok: boolean }>(
    `/v1/me/rooms/${encodeURIComponent(cleanRoom(roomID))}`,
    "PUT",
  );
}

export async function forgetMyRoom(roomID: string): Promise<void> {
  const res = await fetch(`/v1/me/rooms/${encodeURIComponent(cleanRoom(roomID))}`, {
    method: "DELETE",
    credentials: "same-origin",
  });
  if (!res.ok) throw new Error(`forget room → ${res.status}`);
}

// listAllRooms returns one page of rooms newest-first (admin-only on the
// server). Resolves to an empty list when the caller isn't an admin or the
// endpoint isn't available, so the UI can call it unconditionally.
export async function listAllRooms(offset = 0, limit = 50): Promise<Room[]> {
  try {
    return await getJSON<Room[]>(`/v1/admin/rooms?limit=${limit}&offset=${offset}`);
  } catch {
    return [];
  }
}

export async function loadAdminUsers(): Promise<AdminUsersReport | null> {
  try {
    return await getJSON<AdminUsersReport>("/v1/admin/users");
  } catch {
    return null;
  }
}

export async function getRoom(roomID: string): Promise<Room | null> {
  try {
    return await getJSON<Room>(`/v1/rooms/${encodeURIComponent(cleanRoom(roomID))}`);
  } catch {
    return null;
  }
}

export async function patchRoom(
  roomID: string,
  patch: Partial<Pick<Room, "title" | "gated" | "ended">>,
): Promise<Room> {
  return sendJSON<Room>(`/v1/rooms/${encodeURIComponent(cleanRoom(roomID))}`, "PATCH", patch);
}

export async function deleteRoom(roomID: string): Promise<void> {
  const res = await fetch(`/v1/rooms/${encodeURIComponent(cleanRoom(roomID))}`, {
    method: "DELETE",
    credentials: "same-origin",
  });
  if (!res.ok) throw new Error(`delete room → ${res.status}`);
}

export async function loadHistory(roomID: string): Promise<ChatMessage[]> {
  const res = await fetch(`/v1/rooms/${encodeURIComponent(roomID)}/messages?limit=240`);
  if (!res.ok) throw new Error("history request failed");
  return res.json();
}

export async function clearRoomHistory(roomID: string): Promise<void> {
  const res = await fetch(`/v1/rooms/${encodeURIComponent(cleanRoom(roomID))}/messages`, {
    method: "DELETE",
    credentials: "same-origin",
  });
  if (!res.ok) throw new Error(`clear history → ${res.status}`);
}

// loadHistoryBefore pages backwards into older history: it returns up to
// `limit` messages stored before `beforeID` (oldest-first), the cursor being
// the id of the oldest message currently held. An empty array means there is
// nothing older — the top of the room.
export async function loadHistoryBefore(
  roomID: string,
  beforeID: string,
  limit = 120,
): Promise<ChatMessage[]> {
  const params = new URLSearchParams({ before: beforeID, limit: String(limit) });
  const res = await fetch(
    `/v1/rooms/${encodeURIComponent(roomID)}/messages?${params.toString()}`,
  );
  if (!res.ok) throw new Error("history page request failed");
  return res.json();
}

export async function loadParticipants(roomID: string): Promise<Participant[]> {
  const res = await fetch(`/v1/rooms/${encodeURIComponent(roomID)}/participants`);
  if (!res.ok) throw new Error("participants request failed");
  return res.json();
}

export async function postMessage(roomID: string, message: ChatMessage): Promise<void> {
  const res = await fetch(`/v1/rooms/${encodeURIComponent(roomID)}/messages`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(message),
  });
  if (!res.ok) throw new Error("send failed");
}

// uploadAttachment posts one image to the room's attachment store. The server
// sniffs the real mime (raster images only) and enforces size/quota caps, so
// a non-2xx here surfaces as a thrown error with the server's reason.
export async function uploadAttachment(roomID: string, file: File): Promise<AttachmentUpload> {
  const form = new FormData();
  form.append("file", file, file.name);
  const res = await fetch(`/v1/rooms/${encodeURIComponent(cleanRoom(roomID))}/attachments`, {
    method: "POST",
    body: form,
  });
  if (!res.ok) {
    let reason = `upload → ${res.status}`;
    try {
      const body = (await res.json()) as { error?: string };
      if (body.error) reason = body.error;
    } catch {
      // keep status fallback
    }
    throw new Error(reason);
  }
  return res.json() as Promise<AttachmentUpload>;
}

// attachmentURL builds the download path for an attachment reference.
export function attachmentURL(roomID: string, attachmentID: string): string {
  return `/v1/rooms/${encodeURIComponent(cleanRoom(roomID))}/attachments/${encodeURIComponent(attachmentID)}`;
}

// loadSummary fetches the room's rolling LLM summary. The endpoint always
// returns 200 (an empty RoomSummary when the feature is off or no summary has
// been generated yet), so a null here means the request itself failed.
export async function loadSummary(roomID: string): Promise<RoomSummary | null> {
  try {
    return await getJSON<RoomSummary>(
      `/v1/rooms/${encodeURIComponent(cleanRoom(roomID))}/summary`,
    );
  } catch {
    return null;
  }
}

export async function clearRoomSummary(roomID: string): Promise<void> {
  const res = await fetch(`/v1/rooms/${encodeURIComponent(cleanRoom(roomID))}/summary`, {
    method: "DELETE",
    credentials: "same-origin",
  });
  if (!res.ok) throw new Error(`clear summary → ${res.status}`);
}

/* ── agents (用户维度自管理) ──────────────────────────────────────── */

// listAgents 返回登录用户名下全部 agent（含离线，在线状态由服务端合并 hub
// presence）。失败（未登录 / 端点缺失）时抛出，由调用方决定降级。
export async function listAgents(): Promise<Agent[]> {
  const data = await getJSON<{ agents: Agent[] | null }>("/v1/agents");
  return data.agents ?? [];
}

// deleteAgent 解绑（吊销）一个 agent。成功后该 agent 重连降级为匿名。
export async function deleteAgent(agentID: string): Promise<void> {
  const res = await fetch(`/v1/agents/${encodeURIComponent(agentID)}`, {
    method: "DELETE",
    credentials: "same-origin",
  });
  if (!res.ok) throw new Error(`delete agent → ${res.status}`);
}

// listAgentTokens 返回登录用户的接入 token 列表（脱敏：仅哈希前缀与元信息）。
export async function listAgentTokens(): Promise<AgentToken[]> {
  const data = await getJSON<{ tokens: AgentToken[] | null }>("/v1/agents/tokens");
  return data.tokens ?? [];
}

// createAgentToken 生成一个新的接入 token，明文只在返回体里出现一次。
export async function createAgentToken(note?: string): Promise<CreatedAgentToken> {
  return sendJSON<CreatedAgentToken>(
    "/v1/agents/tokens",
    "POST",
    note ? { note } : {},
  );
}

// revokeAgentToken 按哈希前缀吊销一个 token，吊销后用它启动的 bridge 被拒绝。
export async function revokeAgentToken(hashPrefix: string): Promise<void> {
  const res = await fetch(`/v1/agents/tokens/${encodeURIComponent(hashPrefix)}`, {
    method: "DELETE",
    credentials: "same-origin",
  });
  if (!res.ok) throw new Error(`revoke token → ${res.status}`);
}

// joinAgentToRoom 把自己的 agent 加入指定房间；delivered=false 表示 agent
// 控制连接不在线，上线后由 relay 对账补发加入指令。
export async function joinAgentToRoom(
  roomID: string,
  agentID: string,
): Promise<{ ok: boolean; delivered: boolean }> {
  return sendJSON<{ ok: boolean; delivered: boolean }>(
    `/v1/rooms/${encodeURIComponent(roomID)}/agents`,
    "POST",
    { agent_id: agentID },
  );
}

// removeAgentFromRoom 把 agent 移出指定房间（agent 主人 / 房间主人 / admin）。
export async function removeAgentFromRoom(
  roomID: string,
  agentID: string,
): Promise<void> {
  const res = await fetch(
    `/v1/rooms/${encodeURIComponent(roomID)}/agents/${encodeURIComponent(agentID)}`,
    { method: "DELETE", credentials: "same-origin" },
  );
  if (!res.ok) throw new Error(`remove agent → ${res.status}`);
}

// getAgentConfig 读取 agent 启动配置；未保存过时后端返回同形空对象。
export async function getAgentConfig(agentID: string): Promise<AgentConfig> {
  return getJSON<AgentConfig>(
    `/v1/agents/${encodeURIComponent(agentID)}/config`,
  );
}

// putAgentConfig 部分更新：键缺席=保持，api_key 传空串=清除已存 key。
// 返回与 GET 同形的最新脱敏视图。
export async function putAgentConfig(
  agentID: string,
  patch: { model?: string; api_base_url?: string; api_key?: string },
): Promise<AgentConfig> {
  return sendJSON<AgentConfig>(
    `/v1/agents/${encodeURIComponent(agentID)}/config`,
    "PUT",
    patch,
  );
}

/* ── access requests ─────────────────────────────────────────────── */

export async function listAccessRequests(roomID: string): Promise<AccessRequest[]> {
  try {
    return await getJSON<AccessRequest[]>(
      `/v1/rooms/${encodeURIComponent(roomID)}/access-requests`,
    );
  } catch {
    return [];
  }
}

export async function createAccessRequest(
  roomID: string,
  label?: string,
): Promise<AccessRequest> {
  return sendJSON<AccessRequest>(
    `/v1/rooms/${encodeURIComponent(roomID)}/access-requests`,
    "POST",
    label ? { label } : {},
  );
}

export async function decideAccessRequest(
  roomID: string,
  requestID: string,
  decision: "approve" | "deny",
  persistence?: AccessPersistence,
): Promise<AccessRequest> {
  return sendJSON<AccessRequest>(
    `/v1/rooms/${encodeURIComponent(roomID)}/access-requests/${encodeURIComponent(requestID)}`,
    "PATCH",
    persistence ? { decision, persistence } : { decision },
  );
}
