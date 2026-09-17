"use client";

import { FormEvent, useEffect, useMemo, useState } from "react";
import type { LucideIcon } from "lucide-react";
import { ArrowLeft, Building2, Check, LockKeyhole, Phone, Plus, RefreshCw, Save, ShieldCheck, Trash2, UserPlus, Users, X } from "lucide-react";

const API = process.env.NEXT_PUBLIC_CONTROL_API_URL || "";

type Workspace = { id: string; slug: string; name: string; status: string; max_participants: number; room_count: number; tfn_count: number };
type Room = { id: string; workspace_id: string; workspace_name: string; name: string; status: string; room_state: string; participant_limit: number; tfn_id: string; tfn_number: string; tfn_label: string };
type TFN = { id: string; workspace_id: string; workspace_name: string; number: string; label: string; provider_ref: string; status: string; room_id: string; room_name: string };
type User = { id: string; username: string; display_name: string; platform_role: string; status: string };
type Member = { operator_id: string; username: string; display_name: string; platform_role: string; status: string; role: string };
type Overview = { workspaces: number; rooms: number; users: number; tfns: number };

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers);
  if (init.body) headers.set("Content-Type", "application/json");
  const response = await fetch(`${API}${path}`, { ...init, headers, credentials: "include", cache: "no-store" });
  const raw = await response.text();
  let payload: unknown = {};
  try { payload = raw ? JSON.parse(raw) : {}; } catch { payload = {}; }
  if (!response.ok) {
    const message = payload && typeof payload === "object" && "error" in payload && typeof payload.error === "string" ? payload.error : `Request failed (${response.status})`;
    throw new Error(message);
  }
  return payload as T;
}

function initials(value: string) {
  return value.split(/\s+/).filter(Boolean).slice(0, 2).map((part) => part[0]?.toUpperCase()).join("") || "A";
}

export default function AdminPage() {
  const [overview, setOverview] = useState<Overview | null>(null);
  const [workspaces, setWorkspaces] = useState<Workspace[]>([]);
  const [rooms, setRooms] = useState<Room[]>([]);
  const [tfns, setTfns] = useState<TFN[]>([]);
  const [users, setUsers] = useState<User[]>([]);
  const [members, setMembers] = useState<Member[]>([]);
  const [selectedWorkspaceId, setSelectedWorkspaceId] = useState("");
  const [roomLimitDrafts, setRoomLimitDrafts] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState<{ text: string; error?: boolean } | null>(null);
  const [workspaceName, setWorkspaceName] = useState("");
  const [workspaceSlug, setWorkspaceSlug] = useState("");
  const [workspaceLimit, setWorkspaceLimit] = useState("500");
  const [roomName, setRoomName] = useState("");
  const [roomLimit, setRoomLimit] = useState("500");
  const [userName, setUserName] = useState("");
  const [userDisplayName, setUserDisplayName] = useState("");
  const [userPassword, setUserPassword] = useState("");
  const [userRole, setUserRole] = useState("OPERATOR");
  const [userPlatformRole, setUserPlatformRole] = useState("WORKSPACE_OPERATOR");
  const [tfnNumber, setTfnNumber] = useState("");
  const [tfnLabel, setTfnLabel] = useState("");
  const [memberUserId, setMemberUserId] = useState("");
  const [memberRole, setMemberRole] = useState("OPERATOR");

  const selectedWorkspace = workspaces.find((workspace) => workspace.id === selectedWorkspaceId);
  const workspaceRooms = useMemo(() => rooms.filter((room) => room.workspace_id === selectedWorkspaceId), [rooms, selectedWorkspaceId]);
  const workspaceTFNs = useMemo(() => tfns.filter((tfn) => tfn.workspace_id === selectedWorkspaceId), [tfns, selectedWorkspaceId]);

  async function load() {
    setBusy(true);
    try {
      const [nextOverview, nextWorkspaces, nextRooms, nextTFNs, nextUsers] = await Promise.all([
        request<Overview>("/api/v1/admin/overview"),
        request<Workspace[]>("/api/v1/admin/workspaces"),
        request<Room[]>("/api/v1/admin/rooms"),
        request<TFN[]>("/api/v1/admin/tfns"),
        request<User[]>("/api/v1/admin/users"),
      ]);
      setOverview(nextOverview);
      setWorkspaces(nextWorkspaces);
      setRooms(nextRooms);
      setRoomLimitDrafts(Object.fromEntries(nextRooms.map((room) => [room.id, String(room.participant_limit)])));
      setTfns(nextTFNs);
      setUsers(nextUsers);
      setSelectedWorkspaceId((current) => current && nextWorkspaces.some((workspace) => workspace.id === current) ? current : nextWorkspaces[0]?.id || "");
      setMessage(null);
    } catch (error) {
      setMessage({ text: error instanceof Error ? error.message : "Product admin data could not be loaded", error: true });
    } finally {
      setBusy(false);
    }
  }

  async function loadMembers(workspaceID: string) {
    if (!workspaceID) { setMembers([]); return; }
    try {
      setMembers(await request<Member[]>(`/api/v1/admin/workspaces/${workspaceID}/members`));
    } catch (error) {
      setMessage({ text: error instanceof Error ? error.message : "Workspace members could not be loaded", error: true });
    }
  }

  useEffect(() => { void load(); }, []);
  useEffect(() => { void loadMembers(selectedWorkspaceId); }, [selectedWorkspaceId]);

  async function createWorkspace(event: FormEvent) {
    event.preventDefault(); setBusy(true);
    try {
      await request("/api/v1/admin/workspaces", { method: "POST", body: JSON.stringify({ name: workspaceName, slug: workspaceSlug, max_participants: Number(workspaceLimit) }) });
      setWorkspaceName(""); setWorkspaceSlug(""); setWorkspaceLimit("500"); setMessage({ text: "Workspace created" }); await load();
    } catch (error) { setMessage({ text: error instanceof Error ? error.message : "Workspace could not be created", error: true }); } finally { setBusy(false); }
  }

  async function createRoom(event: FormEvent) {
    event.preventDefault(); if (!selectedWorkspaceId) return; setBusy(true);
    try {
      await request("/api/v1/admin/rooms", { method: "POST", body: JSON.stringify({ workspace_id: selectedWorkspaceId, name: roomName, participant_limit: Number(roomLimit) }) });
      setRoomName(""); setRoomLimit(selectedWorkspace?.max_participants ? String(selectedWorkspace.max_participants) : "500"); setMessage({ text: "Room created" }); await load();
    } catch (error) { setMessage({ text: error instanceof Error ? error.message : "Room could not be created", error: true }); } finally { setBusy(false); }
  }

  async function createUser(event: FormEvent) {
    event.preventDefault(); setBusy(true);
    try {
      await request("/api/v1/admin/users", { method: "POST", body: JSON.stringify({ username: userName, display_name: userDisplayName, password: userPassword, platform_role: userPlatformRole, workspace_id: selectedWorkspaceId, workspace_role: userRole }) });
      setUserName(""); setUserDisplayName(""); setUserPassword(""); setMessage({ text: "User created and workspace access assigned" }); await load();
    } catch (error) { setMessage({ text: error instanceof Error ? error.message : "User could not be created", error: true }); } finally { setBusy(false); }
  }

  async function createTFN(event: FormEvent) {
    event.preventDefault(); setBusy(true);
    try {
      await request("/api/v1/admin/tfns", { method: "POST", body: JSON.stringify({ workspace_id: selectedWorkspaceId, number: tfnNumber, label: tfnLabel }) });
      setTfnNumber(""); setTfnLabel(""); setMessage({ text: "TFN added to the workspace pool" }); await load();
    } catch (error) { setMessage({ text: error instanceof Error ? error.message : "TFN could not be added", error: true }); } finally { setBusy(false); }
  }

  async function saveRoom(room: Room, tfnID = room.tfn_id) {
    const participantLimit = Number(roomLimitDrafts[room.id] || room.participant_limit);
    setBusy(true);
    try {
      await request(`/api/v1/admin/rooms/${room.id}`, { method: "PUT", body: JSON.stringify({ name: room.name, participant_limit: participantLimit, tfn_id: tfnID || null }) });
      setMessage({ text: `${room.name} updated` }); await load();
    } catch (error) { setMessage({ text: error instanceof Error ? error.message : "Room could not be updated", error: true }); } finally { setBusy(false); }
  }

  async function saveWorkspace(event: FormEvent) {
    event.preventDefault(); if (!selectedWorkspace) return; setBusy(true);
    try {
      await request(`/api/v1/admin/workspaces/${selectedWorkspace.id}`, { method: "PUT", body: JSON.stringify({ name: selectedWorkspace.name, status: selectedWorkspace.status, max_participants: selectedWorkspace.max_participants }) });
      setMessage({ text: "Workspace policy saved" }); await load();
    } catch (error) { setMessage({ text: error instanceof Error ? error.message : "Workspace could not be saved", error: true }); } finally { setBusy(false); }
  }

  async function addMember(event: FormEvent) {
    event.preventDefault(); if (!selectedWorkspaceId || !memberUserId) return; setBusy(true);
    try {
      await request(`/api/v1/admin/workspaces/${selectedWorkspaceId}/members`, { method: "POST", body: JSON.stringify({ operator_id: memberUserId, role: memberRole }) });
      setMessage({ text: "Workspace access saved" }); setMemberUserId(""); await loadMembers(selectedWorkspaceId);
    } catch (error) { setMessage({ text: error instanceof Error ? error.message : "Workspace access could not be saved", error: true }); } finally { setBusy(false); }
  }

  async function removeMember(member: Member) {
    if (member.role === "OWNER") return; setBusy(true);
    try {
      await request(`/api/v1/admin/workspaces/${selectedWorkspaceId}/members/${member.operator_id}`, { method: "DELETE" });
      setMessage({ text: `${member.display_name || member.username} removed from the workspace` }); await loadMembers(selectedWorkspaceId);
    } catch (error) { setMessage({ text: error instanceof Error ? error.message : "Workspace access could not be removed", error: true }); } finally { setBusy(false); }
  }

  async function toggleUser(user: User) {
    setBusy(true);
    try {
      const status = user.status === "ACTIVE" ? "DISABLED" : "ACTIVE";
      await request(`/api/v1/admin/users/${user.id}`, { method: "PUT", body: JSON.stringify({ display_name: user.display_name, status, platform_role: user.platform_role }) });
      setMessage({ text: `${user.display_name || user.username} is now ${status.toLowerCase()}` }); await load();
    } catch (error) { setMessage({ text: error instanceof Error ? error.message : "User status could not be changed", error: true }); } finally { setBusy(false); }
  }

  async function deleteTFN(tfn: TFN) {
    if (tfn.room_id || !window.confirm(`Remove ${tfn.number} from ${tfn.workspace_name}?`)) return; setBusy(true);
    try {
      await request(`/api/v1/admin/tfns/${tfn.id}`, { method: "DELETE" }); setMessage({ text: `${tfn.number} removed` }); await load();
    } catch (error) { setMessage({ text: error instanceof Error ? error.message : "TFN could not be removed", error: true }); } finally { setBusy(false); }
  }

  if (message?.error && (message.text.includes("administrator access") || message.text.includes("authentication required"))) {
    return <div className="admin-denied"><ShieldCheck size={28} /><h1>Product admin access required</h1><p>This console is reserved for PhonArch owners and product administrators. Customer workspace operators do not have access here.</p><button className="button primary" onClick={() => { window.location.href = "/"; }}>Back to workspace</button></div>;
  }

  return (
    <main className="admin-shell">
      <header className="admin-topbar">
        <div className="admin-brand"><span className="brand-mark"><ShieldCheck size={16} /></span><span><strong>Phonarch</strong><small>Product administration</small></span></div>
        <div className="admin-top-actions"><span className="admin-security"><LockKeyhole size={13} /> Owner-only console</span><button className="button ghost compact-button" onClick={() => { window.location.href = "/"; }}><ArrowLeft size={14} /> Workspace</button><button className="icon-button" title="Refresh admin data" aria-label="Refresh admin data" onClick={() => void load()}><RefreshCw size={15} /></button></div>
      </header>
      {message && <div className={`admin-notice ${message.error ? "error" : "success"}`}>{message.error ? <X size={14} /> : <Check size={14} />}{message.text}</div>}
      <section className="admin-heading"><div><div className="eyebrow">Control plane</div><h1>Platform administration</h1><p>Provision workspaces, allocate room phone numbers, set capacity policy, and control customer access from one protected surface.</p></div><span className="admin-badge"><ShieldCheck size={14} /> No self-signup</span></section>
      {overview && <div className="admin-metrics"><AdminMetric label="Active workspaces" value={overview.workspaces} icon={Building2} /><AdminMetric label="Conference rooms" value={overview.rooms} icon={Phone} /><AdminMetric label="Managed users" value={overview.users} icon={Users} /><AdminMetric label="Active TFNs" value={overview.tfns} icon={ShieldCheck} /></div>}
      <div className="admin-layout">
        <section className="admin-main-column">
          <section className="panel admin-panel"><div className="admin-panel-heading"><div><div className="eyebrow">Tenant directory</div><h2>Workspaces</h2><p className="subtle">Each workspace is an isolated customer boundary for rooms, members, and phone numbers.</p></div><Building2 size={18} /></div><div className="admin-workspace-list">{workspaces.map((workspace) => <button type="button" key={workspace.id} className={`admin-workspace-card ${selectedWorkspaceId === workspace.id ? "active" : ""}`} onClick={() => setSelectedWorkspaceId(workspace.id)}><span className="admin-workspace-mark">{initials(workspace.name)}</span><span><strong>{workspace.name}</strong><small>{workspace.slug} · {workspace.room_count} rooms · {workspace.tfn_count} TFNs</small></span><span className="admin-card-value">{workspace.max_participants}<small>per room</small></span></button>)}</div><form className="admin-inline-form" onSubmit={(event) => void createWorkspace(event)}><input className="field-control" value={workspaceName} onChange={(event) => setWorkspaceName(event.target.value)} placeholder="New workspace name" required /><input className="field-control" value={workspaceSlug} onChange={(event) => setWorkspaceSlug(event.target.value)} placeholder="slug (optional)" /><input className="field-control admin-number" type="number" min="1" value={workspaceLimit} onChange={(event) => setWorkspaceLimit(event.target.value)} aria-label="Workspace participant limit" /><button className="button primary" disabled={busy}><Plus size={14} /> Create workspace</button></form></section>
          {selectedWorkspace && <section className="panel admin-panel"><div className="admin-panel-heading"><div><div className="eyebrow">Workspace policy</div><h2>{selectedWorkspace.name}</h2><p className="subtle">Capacity is the default for new rooms. Individual room limits can be tightened below.</p></div><span className={`status-pill ${selectedWorkspace.status === "ACTIVE" ? "live" : "warning"}`}><span className="status-dot" />{selectedWorkspace.status}</span></div><form className="admin-policy-form" onSubmit={(event) => void saveWorkspace(event)}><label className="field-label">Workspace name<input className="field-control" value={selectedWorkspace.name} onChange={(event) => setWorkspaces((items) => items.map((item) => item.id === selectedWorkspace.id ? { ...item, name: event.target.value } : item))} /></label><label className="field-label">Default room capacity<input className="field-control" type="number" min="1" value={selectedWorkspace.max_participants} onChange={(event) => setWorkspaces((items) => items.map((item) => item.id === selectedWorkspace.id ? { ...item, max_participants: Number(event.target.value) } : item))} /></label><label className="field-label">Workspace state<select className="field-control" value={selectedWorkspace.status} onChange={(event) => setWorkspaces((items) => items.map((item) => item.id === selectedWorkspace.id ? { ...item, status: event.target.value } : item))}><option value="ACTIVE">Active</option><option value="SUSPENDED">Suspended</option></select></label><button className="button primary" disabled={busy}><Save size={14} /> Save policy</button></form></section>}
          <section className="panel admin-panel"><div className="admin-panel-heading"><div><div className="eyebrow">Customer access</div><h2>Users and workspace membership</h2><p className="subtle">Only product administrators create accounts. Assigning a user to a workspace grants the selected role.</p></div><Users size={18} /></div><form className="admin-user-form" onSubmit={(event) => void createUser(event)}><input className="field-control" value={userDisplayName} onChange={(event) => setUserDisplayName(event.target.value)} placeholder="Display name" required /><input className="field-control" value={userName} onChange={(event) => setUserName(event.target.value)} placeholder="Username" required /><input className="field-control" type="password" minLength={8} value={userPassword} onChange={(event) => setUserPassword(event.target.value)} placeholder="Temporary password" required /><select className="field-control" value={userRole} onChange={(event) => setUserRole(event.target.value)}><option value="OPERATOR">Workspace operator</option><option value="ADMIN">Workspace admin</option><option value="VIEWER">Read-only viewer</option></select><select className="field-control" value={userPlatformRole} onChange={(event) => setUserPlatformRole(event.target.value)}><option value="WORKSPACE_OPERATOR">Customer user</option><option value="PLATFORM_ADMIN">Product admin</option></select><button className="button primary" disabled={busy || !selectedWorkspaceId}><UserPlus size={14} /> Create user</button></form><div className="admin-table">{users.map((user) => <div className="admin-user-row" key={user.id}><span className="user-avatar">{initials(user.display_name || user.username)}</span><span><strong>{user.display_name || user.username}</strong><small>{user.username} · {user.platform_role === "PLATFORM_OWNER" ? "Owner" : user.platform_role === "PLATFORM_ADMIN" ? "Product admin" : "Customer user"}</small></span><span className={`role-pill ${user.status === "ACTIVE" ? "running" : "ended"}`}>{user.status}</span><button type="button" className="button ghost compact-button" disabled={busy || user.platform_role === "PLATFORM_OWNER"} onClick={() => void toggleUser(user)}>{user.status === "ACTIVE" ? "Disable" : "Activate"}</button></div>)}</div>{selectedWorkspaceId && <div className="admin-members"><div className="admin-subheading"><strong>{selectedWorkspace?.name} members</strong><small>Workspace-scoped access</small></div><form className="admin-member-form" onSubmit={(event) => void addMember(event)}><select className="field-control" value={memberUserId} onChange={(event) => setMemberUserId(event.target.value)}><option value="">Add existing user</option>{users.filter((user) => !members.some((member) => member.operator_id === user.id)).map((user) => <option key={user.id} value={user.id}>{user.display_name || user.username}</option>)}</select><select className="field-control" value={memberRole} onChange={(event) => setMemberRole(event.target.value)}><option value="OPERATOR">Operator</option><option value="ADMIN">Admin</option><option value="VIEWER">Viewer</option></select><button className="button ghost" disabled={busy || !memberUserId}><Plus size={14} /> Add access</button></form>{members.map((member) => <div className="admin-member-row" key={member.operator_id}><span>{member.display_name || member.username}</span><span className="role-pill">{member.role}</span>{member.role !== "OWNER" && <button type="button" className="button ghost compact-button" disabled={busy} onClick={() => void removeMember(member)}>Remove</button>}</div>)}</div>}</section>
        </section>
        <aside className="admin-side-column">
          <section className="panel admin-panel"><div className="admin-panel-heading"><div><div className="eyebrow">Telephony inventory</div><h2>Workspace TFNs</h2><p className="subtle">Phone numbers are scoped to one workspace and can be assigned to one room.</p></div><Phone size={18} /></div>{selectedWorkspace && <form className="admin-tfn-form" onSubmit={(event) => void createTFN(event)}><input className="field-control" value={tfnNumber} onChange={(event) => setTfnNumber(event.target.value)} placeholder="+919876543210" required /><input className="field-control" value={tfnLabel} onChange={(event) => setTfnLabel(event.target.value)} placeholder="TFN label" /><button className="button primary" disabled={busy}><Plus size={14} /> Add TFN</button></form>}{workspaceTFNs.length ? <div className="admin-tfn-list">{workspaceTFNs.map((tfn) => <div className="admin-tfn-row" key={tfn.id}><span className="mini-icon"><Phone size={14} /></span><span><strong>{tfn.number}</strong><small>{tfn.label || "Unlabelled"} · {tfn.room_name || "Unassigned"}</small></span><span className={`status-dot ${tfn.status === "ACTIVE" ? "active" : ""}`} /><button type="button" className="icon-button admin-delete-button" title="Remove unassigned TFN" aria-label="Remove unassigned TFN" disabled={busy || Boolean(tfn.room_id)} onClick={() => void deleteTFN(tfn)}><Trash2 size={13} /></button></div>)}</div> : <div className="admin-empty">No TFNs allocated to this workspace yet.</div>}</section>
          <section className="panel admin-panel"><div className="admin-panel-heading"><div><div className="eyebrow">Room allocation</div><h2>Room guardrails</h2><p className="subtle">A room cannot start until it has an active assigned TFN and remains inside its capacity limit.</p></div><Building2 size={18} /></div>{selectedWorkspace && <form className="admin-inline-form admin-room-create-form" onSubmit={(event) => void createRoom(event)}><input className="field-control" value={roomName} onChange={(event) => setRoomName(event.target.value)} placeholder="New room name" required /><input className="field-control admin-number" type="number" min="1" value={roomLimit} onChange={(event) => setRoomLimit(event.target.value)} aria-label="Room participant limit" /><button className="button primary" disabled={busy}><Plus size={14} /> Create room</button></form>}{workspaceRooms.length ? <div className="admin-room-list">{workspaceRooms.map((room) => <div className="admin-room-row" key={room.id}><div className="admin-room-copy"><strong>{room.name}</strong><small>{room.room_state} · {room.participant_limit} participants</small></div><input className="field-control admin-room-limit" type="number" min="1" value={roomLimitDrafts[room.id] ?? room.participant_limit} onChange={(event) => setRoomLimitDrafts((drafts) => ({ ...drafts, [room.id]: event.target.value }))} aria-label={`${room.name} participant limit`} disabled={busy || room.room_state === "RUNNING" || room.room_state === "STARTING"} /><select className="field-control" value={room.tfn_id} onChange={(event) => void saveRoom(room, event.target.value)} disabled={busy || room.room_state === "RUNNING" || room.room_state === "STARTING"}><option value="">No TFN assigned</option>{workspaceTFNs.map((tfn) => <option key={tfn.id} value={tfn.id}>{tfn.number}{tfn.room_id && tfn.room_id !== room.id ? " · assigned" : ""}</option>)}</select><button type="button" className="button ghost compact-button" disabled={busy || room.room_state === "RUNNING" || room.room_state === "STARTING"} onClick={() => void saveRoom(room)}>Save</button></div>)}</div> : <div className="admin-empty">No rooms in this workspace.</div>}</section>
        </aside>
      </div>
    </main>
  );
}

function AdminMetric({ label, value, icon: Icon }: { label: string; value: number; icon: LucideIcon }) {
  return <div className="panel admin-metric"><span className="mini-icon"><Icon size={15} /></span><span><strong>{value}</strong><small>{label}</small></span></div>;
}
