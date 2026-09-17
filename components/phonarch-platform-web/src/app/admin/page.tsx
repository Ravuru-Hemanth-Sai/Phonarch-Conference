"use client";

import { FormEvent, useEffect, useMemo, useState } from "react";
import type { LucideIcon } from "lucide-react";
import { ArrowLeft, ArrowRight, ArrowUpRight, Building2, Check, ChevronDown, LockKeyhole, LogOut, Phone, Plus, RefreshCw, Save, ShieldCheck, Trash2, UserPlus, Users, X } from "lucide-react";

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
    if (response.status === 401 && typeof window !== "undefined") window.location.href = "/admin/login";
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

  async function loadDirectory() {
    setBusy(true);
    try {
      const nextWorkspaces = await request<Workspace[]>("/api/v1/admin/workspaces");
      setWorkspaces(nextWorkspaces);
      setSelectedWorkspaceId((current) => current && nextWorkspaces.some((workspace) => workspace.id === current) ? current : nextWorkspaces[0]?.id || "");
      setMessage(null);
    } catch (error) {
      setMessage({ text: error instanceof Error ? error.message : "Product admin data could not be loaded", error: true });
    } finally {
      setBusy(false);
    }
  }

  async function loadWorkspaceData(workspaceID: string) {
    if (!workspaceID) { setOverview(null); setRooms([]); setTfns([]); setUsers([]); return; }
    try {
      const scope = `?workspace_id=${encodeURIComponent(workspaceID)}`;
      const [nextOverview, nextRooms, nextTFNs, nextUsers] = await Promise.all([
        request<Overview>(`/api/v1/admin/overview${scope}`),
        request<Room[]>(`/api/v1/admin/rooms${scope}`),
        request<TFN[]>(`/api/v1/admin/tfns${scope}`),
        request<User[]>(`/api/v1/admin/users${scope}`),
      ]);
      setOverview(nextOverview);
      setRooms(nextRooms);
      setRoomLimitDrafts(Object.fromEntries(nextRooms.map((room) => [room.id, String(room.participant_limit)])));
      setTfns(nextTFNs);
      setUsers(nextUsers);
    } catch (error) {
      setMessage({ text: error instanceof Error ? error.message : "Workspace admin data could not be loaded", error: true });
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

  useEffect(() => { void loadDirectory(); }, []);
  useEffect(() => { void loadWorkspaceData(selectedWorkspaceId); }, [selectedWorkspaceId]);
  useEffect(() => { void loadMembers(selectedWorkspaceId); }, [selectedWorkspaceId]);

  async function createWorkspace(event: FormEvent) {
    event.preventDefault(); setBusy(true);
    try {
      await request("/api/v1/admin/workspaces", { method: "POST", body: JSON.stringify({ name: workspaceName, slug: workspaceSlug, max_participants: Number(workspaceLimit) }) });
      setWorkspaceName(""); setWorkspaceSlug(""); setWorkspaceLimit("500"); setMessage({ text: "Workspace created" }); await loadDirectory();
    } catch (error) { setMessage({ text: error instanceof Error ? error.message : "Workspace could not be created", error: true }); } finally { setBusy(false); }
  }

  async function createRoom(event: FormEvent) {
    event.preventDefault(); if (!selectedWorkspaceId) return; setBusy(true);
    try {
      await request("/api/v1/admin/rooms", { method: "POST", body: JSON.stringify({ workspace_id: selectedWorkspaceId, name: roomName, participant_limit: Number(roomLimit) }) });
      setRoomName(""); setRoomLimit(selectedWorkspace?.max_participants ? String(selectedWorkspace.max_participants) : "500"); setMessage({ text: "Room created" }); await loadWorkspaceData(selectedWorkspaceId);
    } catch (error) { setMessage({ text: error instanceof Error ? error.message : "Room could not be created", error: true }); } finally { setBusy(false); }
  }

  async function createUser(event: FormEvent) {
    event.preventDefault(); setBusy(true);
    try {
      await request("/api/v1/admin/users", { method: "POST", body: JSON.stringify({ username: userName, display_name: userDisplayName, password: userPassword, platform_role: userPlatformRole, workspace_id: selectedWorkspaceId, workspace_role: userRole }) });
      setUserName(""); setUserDisplayName(""); setUserPassword(""); setMessage({ text: "User created and workspace access assigned" }); await loadWorkspaceData(selectedWorkspaceId); await loadMembers(selectedWorkspaceId);
    } catch (error) { setMessage({ text: error instanceof Error ? error.message : "User could not be created", error: true }); } finally { setBusy(false); }
  }

  async function createTFN(event: FormEvent) {
    event.preventDefault(); setBusy(true);
    try {
      await request("/api/v1/admin/tfns", { method: "POST", body: JSON.stringify({ workspace_id: selectedWorkspaceId, number: tfnNumber, label: tfnLabel }) });
      setTfnNumber(""); setTfnLabel(""); setMessage({ text: "TFN added to the workspace pool" }); await loadWorkspaceData(selectedWorkspaceId);
    } catch (error) { setMessage({ text: error instanceof Error ? error.message : "TFN could not be added", error: true }); } finally { setBusy(false); }
  }

  async function saveRoom(room: Room, tfnID = room.tfn_id) {
    const participantLimit = Number(roomLimitDrafts[room.id] || room.participant_limit);
    setBusy(true);
    try {
      await request(`/api/v1/admin/rooms/${room.id}?workspace_id=${encodeURIComponent(selectedWorkspaceId)}`, { method: "PUT", body: JSON.stringify({ name: room.name, participant_limit: participantLimit, tfn_id: tfnID || null }) });
      setMessage({ text: `${room.name} updated` }); await loadWorkspaceData(selectedWorkspaceId);
    } catch (error) { setMessage({ text: error instanceof Error ? error.message : "Room could not be updated", error: true }); } finally { setBusy(false); }
  }

  async function saveWorkspace(event: FormEvent) {
    event.preventDefault(); if (!selectedWorkspace) return; setBusy(true);
    try {
      await request(`/api/v1/admin/workspaces/${selectedWorkspace.id}`, { method: "PUT", body: JSON.stringify({ name: selectedWorkspace.name, status: selectedWorkspace.status, max_participants: selectedWorkspace.max_participants }) });
      setMessage({ text: "Workspace policy saved" }); await loadDirectory(); await loadWorkspaceData(selectedWorkspaceId);
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
      await request(`/api/v1/admin/users/${user.id}?workspace_id=${encodeURIComponent(selectedWorkspaceId)}`, { method: "PUT", body: JSON.stringify({ display_name: user.display_name, status, platform_role: user.platform_role }) });
      setMessage({ text: `${user.display_name || user.username} is now ${status.toLowerCase()}` }); await loadWorkspaceData(selectedWorkspaceId);
    } catch (error) { setMessage({ text: error instanceof Error ? error.message : "User status could not be changed", error: true }); } finally { setBusy(false); }
  }

  async function deleteTFN(tfn: TFN) {
    if (tfn.room_id || !window.confirm(`Remove ${tfn.number} from ${tfn.workspace_name}?`)) return; setBusy(true);
    try {
      await request(`/api/v1/admin/tfns/${tfn.id}?workspace_id=${encodeURIComponent(selectedWorkspaceId)}`, { method: "DELETE" }); setMessage({ text: `${tfn.number} removed` }); await loadWorkspaceData(selectedWorkspaceId);
    } catch (error) { setMessage({ text: error instanceof Error ? error.message : "TFN could not be removed", error: true }); } finally { setBusy(false); }
  }

  async function logout() {
    try { await request("/api/v1/auth/admin/logout", { method: "POST" }); } finally { window.location.href = "/admin/login"; }
  }

  if (message?.error && (message.text.includes("administrator access") || message.text.includes("authentication required"))) {
    return <div className="admin-denied"><ShieldCheck size={28} /><h1>Product admin access required</h1><p>This console is reserved for PhonArch owners and product administrators. Customer workspace operators do not have access here.</p><button className="button primary" onClick={() => { window.location.href = "/"; }}>Back to workspace</button></div>;
  }

  return (
    <main className="admin-shell">
      <header className="admin-topbar">
        <div className="admin-brand"><span className="brand-mark"><ShieldCheck size={16} /></span><span><strong>Phonarch</strong><small>Product administration</small></span></div>
        <div className="admin-top-actions"><span className="admin-security"><LockKeyhole size={13} /> Owner-only console</span><button className="button ghost compact-button" onClick={() => { window.location.href = "/"; }}><ArrowLeft size={14} /> Workspace</button><button className="icon-button" title="Refresh admin data" aria-label="Refresh admin data" onClick={() => { void loadDirectory(); void loadWorkspaceData(selectedWorkspaceId); void loadMembers(selectedWorkspaceId); }}><RefreshCw size={15} /></button><button className="button ghost compact-button" onClick={() => void logout()}><LogOut size={14} /> Sign out</button></div>
      </header>
      {message && <div className={`admin-notice ${message.error ? "error" : "success"}`}>{message.error ? <X size={14} /> : <Check size={14} />}{message.text}</div>}
      <section className="admin-heading admin-heading-compact"><div><div className="eyebrow">Product owner workspace view</div><h1>{selectedWorkspace?.name || "Workspace control center"}</h1><p>Switch between isolated customer workspaces and manage the room policy they see in their own workspace UI.</p></div><span className="admin-badge"><ShieldCheck size={14} /> No self-signup</span></section>
      <section className="panel admin-workspace-context"><div className="admin-workspace-context-copy"><span className="admin-workspace-mark">{initials(selectedWorkspace?.name || "Workspace")}</span><div><div className="eyebrow">Selected workspace</div><strong>{selectedWorkspace?.name || "Choose a workspace"}</strong><small>{selectedWorkspace ? `${selectedWorkspace.slug} · isolated room, TFN, and access boundary` : "No workspace selected"}</small></div></div><label className="admin-workspace-select"><span>Switch workspace</span><span className="admin-select-wrap"><select className="field-control" value={selectedWorkspaceId} onChange={(event) => setSelectedWorkspaceId(event.target.value)} aria-label="Switch workspace">{workspaces.map((workspace) => <option key={workspace.id} value={workspace.id}>{workspace.name}</option>)}</select><ChevronDown size={14} /></span></label></section>
      {overview && <div className="admin-metrics"><AdminMetric label="Conference rooms" value={overview.rooms} icon={Building2} /><AdminMetric label="Workspace users" value={overview.users} icon={Users} /><AdminMetric label="Active TFNs" value={overview.tfns} icon={Phone} /><AdminMetric label="Max per room" value={selectedWorkspace?.max_participants || 0} icon={ShieldCheck} /></div>}
      {selectedWorkspace && <section className="panel admin-room-directory"><div className="admin-panel-heading"><div><div className="eyebrow">Room directory</div><h2>{selectedWorkspace.name} conference rooms</h2><p className="subtle">Every room is an isolated customer resource. TFN and participant limit are product-managed controls and are read-only to workspace users.</p></div><span className={`status-pill ${selectedWorkspace.status === "ACTIVE" ? "live" : "warning"}`}><span className="status-dot" />{selectedWorkspace.status}</span></div><form className="admin-create-room-bar" onSubmit={(event) => void createRoom(event)}><input className="field-control" value={roomName} onChange={(event) => setRoomName(event.target.value)} placeholder="New conference room name" required /><input className="field-control admin-number" type="number" min="1" max={selectedWorkspace.max_participants} value={roomLimit} onChange={(event) => setRoomLimit(event.target.value)} aria-label="New room participant limit" /><button className="button primary" disabled={busy}><Plus size={14} /> Create room</button></form>{workspaceRooms.length ? <div className="admin-room-grid">{workspaceRooms.map((room) => { const locked = room.room_state === "RUNNING" || room.room_state === "STARTING"; return <article className="admin-room-card" key={room.id}><div className="admin-room-card-top"><span className="room-mark"><Building2 size={17} /></span><span className={`status-pill ${room.room_state === "RUNNING" ? "live" : room.room_state === "STARTING" ? "warning" : "neutral"}`}><span className="status-dot" />{room.room_state || "READY"}</span><ArrowUpRight size={15} className="admin-room-arrow" /></div><div className="admin-room-card-title"><h3>{room.name}</h3><small>{locked ? "Policy locked while room is live" : "Ready for room configuration"}</small></div><div className="admin-room-policy"><label className="field-label">Assigned TFN<select className="field-control" value={room.tfn_id} onChange={(event) => void saveRoom(room, event.target.value)} disabled={busy || locked}><option value="">No TFN assigned</option>{workspaceTFNs.map((tfn) => <option key={tfn.id} value={tfn.id}>{tfn.number}{tfn.room_id && tfn.room_id !== room.id ? " · assigned" : ""}</option>)}</select></label><label className="field-label">Participant limit<input className="field-control" type="number" min="1" max={selectedWorkspace.max_participants} value={roomLimitDrafts[room.id] ?? room.participant_limit} onChange={(event) => setRoomLimitDrafts((drafts) => ({ ...drafts, [room.id]: event.target.value }))} aria-label={`${room.name} participant limit`} disabled={busy || locked} /></label></div><div className="admin-room-card-foot"><span>{room.tfn_number || "No TFN allocated"}</span><span>{room.participant_limit} max callers</span><button type="button" className="button ghost compact-button" disabled={busy || locked} onClick={() => void saveRoom(room)}>{locked ? "Locked" : <><Save size={13} /> Save policy</>}</button></div></article>; })}</div> : <div className="admin-empty admin-room-empty"><Building2 size={18} />No conference rooms in this workspace yet.</div>}</section>}
      {!selectedWorkspace && <section className="panel admin-empty admin-room-empty"><Building2 size={18} />Create or select a workspace to manage its conference rooms.</section>}
      <div className="admin-layout admin-secondary-layout">
        <section className="admin-main-column">
          {selectedWorkspace && <section className="panel admin-panel"><div className="admin-panel-heading"><div><div className="eyebrow">Workspace policy</div><h2>{selectedWorkspace.name}</h2><p className="subtle">This default capacity applies to new rooms. Existing rooms can be configured independently above.</p></div><Building2 size={18} /></div><form className="admin-policy-form" onSubmit={(event) => void saveWorkspace(event)}><label className="field-label">Workspace name<input className="field-control" value={selectedWorkspace.name} onChange={(event) => setWorkspaces((items) => items.map((item) => item.id === selectedWorkspace.id ? { ...item, name: event.target.value } : item))} /></label><label className="field-label">Default room capacity<input className="field-control" type="number" min="1" value={selectedWorkspace.max_participants} onChange={(event) => setWorkspaces((items) => items.map((item) => item.id === selectedWorkspace.id ? { ...item, max_participants: Number(event.target.value) } : item))} /></label><label className="field-label">Workspace state<select className="field-control" value={selectedWorkspace.status} onChange={(event) => setWorkspaces((items) => items.map((item) => item.id === selectedWorkspace.id ? { ...item, status: event.target.value } : item))}><option value="ACTIVE">Active</option><option value="SUSPENDED">Suspended</option></select></label><button className="button primary" disabled={busy}><Save size={14} /> Save policy</button></form></section>}
          <section className="panel admin-panel"><div className="admin-panel-heading"><div><div className="eyebrow">Customer access</div><h2>Users and workspace membership</h2><p className="subtle">Only product administrators create accounts. Assigning a user to a workspace grants the selected role.</p></div><Users size={18} /></div><form className="admin-user-form" onSubmit={(event) => void createUser(event)}><input className="field-control" value={userDisplayName} onChange={(event) => setUserDisplayName(event.target.value)} placeholder="Display name" required /><input className="field-control" value={userName} onChange={(event) => setUserName(event.target.value)} placeholder="Username" required /><input className="field-control" type="password" minLength={8} value={userPassword} onChange={(event) => setUserPassword(event.target.value)} placeholder="Temporary password" required /><select className="field-control" value={userRole} onChange={(event) => setUserRole(event.target.value)}><option value="OPERATOR">Workspace operator</option><option value="ADMIN">Workspace admin</option><option value="VIEWER">Read-only viewer</option></select><select className="field-control" value={userPlatformRole} onChange={(event) => setUserPlatformRole(event.target.value)}><option value="WORKSPACE_OPERATOR">Customer user</option><option value="PLATFORM_ADMIN">Product admin</option></select><button className="button primary" disabled={busy || !selectedWorkspaceId}><UserPlus size={14} /> Create user</button></form><div className="admin-table">{users.map((user) => <div className="admin-user-row" key={user.id}><span className="user-avatar">{initials(user.display_name || user.username)}</span><span><strong>{user.display_name || user.username}</strong><small>{user.username} · {user.platform_role === "PLATFORM_OWNER" ? "Owner" : user.platform_role === "PLATFORM_ADMIN" ? "Product admin" : "Customer user"}</small></span><span className={`role-pill ${user.status === "ACTIVE" ? "running" : "ended"}`}>{user.status}</span><button type="button" className="button ghost compact-button" disabled={busy || user.platform_role === "PLATFORM_OWNER"} onClick={() => void toggleUser(user)}>{user.status === "ACTIVE" ? "Disable" : "Activate"}</button></div>)}</div>{selectedWorkspaceId && <div className="admin-members"><div className="admin-subheading"><strong>{selectedWorkspace?.name} members</strong><small>Workspace-scoped access</small></div><form className="admin-member-form" onSubmit={(event) => void addMember(event)}><select className="field-control" value={memberUserId} onChange={(event) => setMemberUserId(event.target.value)}><option value="">Add existing user</option>{users.filter((user) => !members.some((member) => member.operator_id === user.id)).map((user) => <option key={user.id} value={user.id}>{user.display_name || user.username}</option>)}</select><select className="field-control" value={memberRole} onChange={(event) => setMemberRole(event.target.value)}><option value="OPERATOR">Operator</option><option value="ADMIN">Admin</option><option value="VIEWER">Viewer</option></select><button className="button ghost" disabled={busy || !memberUserId}><Plus size={14} /> Add access</button></form>{members.map((member) => <div className="admin-member-row" key={member.operator_id}><span>{member.display_name || member.username}</span><span className="role-pill">{member.role}</span>{member.role !== "OWNER" && <button type="button" className="button ghost compact-button" disabled={busy} onClick={() => void removeMember(member)}>Remove</button>}</div>)}</div>}</section>
        </section>
        <aside className="admin-side-column">
          <section className="panel admin-panel"><div className="admin-panel-heading"><div><div className="eyebrow">Telephony inventory</div><h2>Workspace TFNs</h2><p className="subtle">Numbers are isolated to this workspace and can be assigned to one room.</p></div><Phone size={18} /></div>{selectedWorkspace && <form className="admin-tfn-form" onSubmit={(event) => void createTFN(event)}><input className="field-control" value={tfnNumber} onChange={(event) => setTfnNumber(event.target.value)} placeholder="+919876543210" required /><input className="field-control" value={tfnLabel} onChange={(event) => setTfnLabel(event.target.value)} placeholder="TFN label" /><button className="button primary" disabled={busy}><Plus size={14} /> Add TFN</button></form>}{workspaceTFNs.length ? <div className="admin-tfn-list">{workspaceTFNs.map((tfn) => <div className="admin-tfn-row" key={tfn.id}><span className="mini-icon"><Phone size={14} /></span><span><strong>{tfn.number}</strong><small>{tfn.label || "Unlabelled"} · {tfn.room_name || "Unassigned"}</small></span><span className={`status-dot ${tfn.status === "ACTIVE" ? "active" : ""}`} /><button type="button" className="icon-button admin-delete-button" title="Remove unassigned TFN" aria-label="Remove unassigned TFN" disabled={busy || Boolean(tfn.room_id)} onClick={() => void deleteTFN(tfn)}><Trash2 size={13} /></button></div>)}</div> : <div className="admin-empty">No TFNs allocated to this workspace yet.</div>}</section>
          <section className="panel admin-panel"><div className="admin-panel-heading"><div><div className="eyebrow">Tenant directory</div><h2>Add workspace</h2><p className="subtle">Create another isolated customer boundary, then switch into it above.</p></div><Building2 size={18} /></div><form className="admin-inline-form" onSubmit={(event) => void createWorkspace(event)}><input className="field-control" value={workspaceName} onChange={(event) => setWorkspaceName(event.target.value)} placeholder="Workspace name" required /><input className="field-control" value={workspaceSlug} onChange={(event) => setWorkspaceSlug(event.target.value)} placeholder="slug (optional)" /><input className="field-control admin-number" type="number" min="1" value={workspaceLimit} onChange={(event) => setWorkspaceLimit(event.target.value)} aria-label="Workspace participant limit" /><button className="button primary" disabled={busy}><Plus size={14} /> Create</button></form><div className="admin-workspace-list admin-workspace-list-secondary">{workspaces.map((workspace) => <button type="button" key={workspace.id} className={`admin-workspace-card ${selectedWorkspaceId === workspace.id ? "active" : ""}`} onClick={() => setSelectedWorkspaceId(workspace.id)}><span className="admin-workspace-mark">{initials(workspace.name)}</span><span><strong>{workspace.name}</strong><small>{workspace.room_count} rooms · {workspace.tfn_count} TFNs</small></span><ArrowRight size={13} className="admin-workspace-chevron" /></button>)}</div></section>
        </aside>
      </div>
    </main>
  );
}

function AdminMetric({ label, value, icon: Icon }: { label: string; value: number; icon: LucideIcon }) {
  return <div className="panel admin-metric"><span className="mini-icon"><Icon size={15} /></span><span><strong>{value}</strong><small>{label}</small></span></div>;
}
