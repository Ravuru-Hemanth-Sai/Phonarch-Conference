"use client";

import { ChangeEvent, FormEvent, useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { LucideIcon } from "lucide-react";
import * as XLSX from "xlsx";
import {
  Activity,
  AlertCircle,
  ArrowLeft,
  ArrowRight,
  ArrowUpRight,
  Ban,
  Building2,
  Check,
  ChevronDown,
  ChevronRight,
  Clock3,
  FileSpreadsheet,
  Gauge,
  Hand,
  LifeBuoy,
  LogOut,
  Mic,
  MicOff,
  Network,
  PanelLeftClose,
  PanelLeftOpen,
  Pencil,
  Phone,
  Plus,
  RadioTower,
  RefreshCw,
  Search,
  ServerCog,
  Settings,
  ShieldCheck,
  Sparkles,
  Trash2,
  Upload,
  Users,
  Volume2,
  X,
} from "lucide-react";

const API = process.env.NEXT_PUBLIC_CONTROL_API_URL || "";

type View = "rooms" | "room" | "dialer" | "bulk" | "activity" | "settings";
type RoomTab = "live" | "dialer" | "bulk" | "activity" | "settings";
type DialRegion = { code: string; label: string };
type StartCheck = { key: string; label: string; ok: boolean; detail: string };
type RoomTFN = { id: string; number: string; label: string };
type Bridge = { id: string; name: string; status: string; room_state?: string; host_name?: string; host_phone?: string; default_region?: string; participant_limit?: number; tfn_id?: string; tfn_number?: string; tfn_label?: string; created_at?: string };
type Participant = {
  participant_id: string;
  name: string;
  phone_number: string;
  desired_state: string;
  call_id: string;
  state: string;
  node_id: string;
  role?: string;
};
type SpeakerRequest = {
  id: string;
  participant_id: string;
  status: string;
  digit: string;
  requested_at: string;
  granted_by?: string;
};
type SessionParticipant = { participant_id?: string; name: string; phone_number: string; role: string; state: string; joined_at?: string; left_at?: string };
type SessionSummary = { session_id: string; status: string; started_at?: string; ended_at?: string; duration_seconds?: number; participants: SessionParticipant[] };
type RoomState = { workspace_id?: string; bridge_id: string; room_state?: string; default_region?: string; participant_limit?: number; tfn?: RoomTFN; start_ready?: boolean; start_checks?: StartCheck[]; host?: { name: string; phone_number: string }; participants: Participant[]; speaker_requests?: SpeakerRequest[]; sessions?: SessionSummary[] };
type AuthMe = { username: string; display_name: string; workspace_role: string; can_manage_users: boolean; workspace?: { id: string; slug: string; name: string; status: string } };
type Contact = { name: string; phone_number: string; role: string; valid: boolean; reason?: string };
type Notice = { tone: "success" | "error"; message: string } | null;

const liveStates = new Set(["DISPATCHED", "DIALING", "CALLING", "RINGING", "ANSWERED", "IN_BRIDGE"]);
const connectedStates = new Set(["ANSWERED", "IN_BRIDGE"]);
const terminalStates = new Set(["DROPPED", "ENDED", "FAILED"]);
const dialRegions: DialRegion[] = [
  { code: "+91", label: "India (+91)" },
  { code: "+1", label: "United States / Canada (+1)" },
  { code: "+44", label: "United Kingdom (+44)" },
  { code: "+61", label: "Australia (+61)" },
  { code: "+65", label: "Singapore (+65)" },
  { code: "+971", label: "United Arab Emirates (+971)" },
];

function normalizePhone(phone: string, region: string): string {
  const trimmed = phone.trim();
  const digits = trimmed.replace(/\D/g, "");
  if (!digits) return "";
  if (trimmed.startsWith("+")) return `+${digits}`;
  return `${region}${digits.replace(/^0+/, "")}`;
}

function localPhonePart(phone: string, region: string): string {
  const trimmed = phone.trim();
  if (!trimmed) return "";
  const digits = trimmed.replace(/\D/g, "");
  const regionDigits = region.replace(/\D/g, "");
  if (trimmed.startsWith("+") && digits.startsWith(regionDigits)) return digits.slice(regionDigits.length);
  return trimmed.startsWith("+") ? digits : trimmed;
}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers);
  if (init.body) headers.set("Content-Type", "application/json");
  const response = await fetch(`${API}${path}`, { ...init, headers, credentials: "include", cache: "no-store" });
  const raw = await response.text();
  let payload: unknown = {};
  try { payload = raw ? JSON.parse(raw) : {}; } catch { payload = {}; }
  if (response.status === 401) {
    window.location.href = "/login";
    throw new Error("Authentication required");
  }
  if (!response.ok) {
    const message = payload && typeof payload === "object" && "error" in payload && typeof payload.error === "string" ? payload.error : `Request failed (${response.status})`;
    throw new Error(message);
  }
  return payload as T;
}

function initials(value: string): string {
  return value.split(/\s+/).filter(Boolean).slice(0, 2).map((part) => part[0]?.toUpperCase()).join("") || "P";
}

function shortTime(value?: string): string {
  if (!value) return "—";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "—";
  return new Intl.DateTimeFormat(undefined, { month: "short", day: "numeric", hour: "numeric", minute: "2-digit" }).format(date);
}

function isLive(participant: Participant): boolean {
  return Boolean(participant.call_id) && liveStates.has(participant.state) && !terminalStates.has(participant.state);
}

function isConnected(participant: Participant): boolean {
  return isLive(participant) && connectedStates.has(participant.state);
}

function stateLabel(value: string): string {
  return value ? value.replaceAll("_", " ").toLowerCase().replace(/\b\w/g, (letter) => letter.toUpperCase()) : "No call";
}

function stateTone(value: string): "live" | "warning" | "danger" | "neutral" {
  if (connectedStates.has(value)) return "live";
  if (terminalStates.has(value)) return "danger";
  if (liveStates.has(value)) return "warning";
  return "neutral";
}

function IconButton({ title, onClick, children, danger = false, disabled = false }: { title: string; onClick?: () => void; children: React.ReactNode; danger?: boolean; disabled?: boolean }) {
  return <button type="button" className={`icon-button ${danger ? "danger" : ""}`} title={title} aria-label={title} onClick={onClick} disabled={disabled}>{children}</button>;
}

function StatusPill({ state, label }: { state?: string; label?: string }) {
  const tone = state ? stateTone(state) : "neutral";
  return <span className={`status-pill ${tone}`}><span className="status-dot" />{label || (state ? stateLabel(state) : "Ready")}</span>;
}

function EmptyState({ icon: Icon, title, description, action }: { icon: LucideIcon; title: string; description: string; action?: React.ReactNode }) {
  return <div className="empty-state"><div className="empty-state-icon"><Icon size={21} /></div><h3>{title}</h3><p>{description}</p>{action}</div>;
}

function PageHeader({ eyebrow, title, description, actions }: { eyebrow: string; title: string; description: string; actions?: React.ReactNode }) {
  return <div className="page-header"><div><div className="eyebrow">{eyebrow}</div><h1>{title}</h1><p className="subtle page-description">{description}</p></div>{actions && <div className="page-actions">{actions}</div>}</div>;
}

function WorkspaceSwitcher({ onSettings }: { onSettings: () => void }) {
  const [open, setOpen] = useState(false);
  return <div className="workspace-switcher"><button type="button" className={`workspace-trigger ${open ? "open" : ""}`} onClick={() => setOpen((current) => !current)} aria-haspopup="menu" aria-expanded={open}><span className="workspace-trigger-mark">OP</span><span className="workspace-trigger-copy"><small>Workspace</small><strong>Operations</strong></span><ChevronDown size={14} className="workspace-trigger-chevron" /></button>{open && <div className="workspace-popover" role="menu"><div className="workspace-popover-heading"><strong>Current workspace</strong><kbd>Esc</kbd></div><div className="workspace-current"><span className="workspace-option-mark">OP</span><span className="workspace-option-copy"><strong>Operations</strong><small>Conference workspace</small></span><Check size={15} /></div><button type="button" className="workspace-create" onClick={() => { setOpen(false); onSettings(); }}><Settings size={14} /><span>Workspace settings</span><ChevronRight size={13} /></button></div>}</div>;
}

function TopBar({ view, room, userName, workspaceRole, onNavigate, onLogout, onRefresh }: { view: View; room?: Bridge; userName: string; workspaceRole: string; onNavigate: (view: View) => void; onLogout: () => void; onRefresh: () => void }) {
  const pageName = view === "rooms" ? "Conference rooms" : view === "room" ? room?.name || "Room operations" : view === "dialer" ? "Direct dialer" : view === "bulk" ? "Bulk outreach" : view === "activity" ? "Call activity" : "Workspace settings";
  return <header className="topbar"><div className="topbar-left"><button type="button" className="topbar-brand" onClick={() => onNavigate("rooms")}><span className="brand-mark"><Sparkles size={16} /></span><span><strong>Phonarch</strong></span></button>{view !== "rooms" && <><div className="breadcrumbs"><button type="button" onClick={() => onNavigate("rooms")}>Workspace</button><ChevronRight size={13} /><span>{pageName}</span></div><span className="topbar-caption">Conference command center</span></>}</div><div className="topbar-actions"><WorkspaceSwitcher onSettings={() => onNavigate("settings")} /><button type="button" className={`topbar-settings ${view === "settings" ? "active" : ""}`} onClick={() => onNavigate("settings")}><Settings size={15} /><span>Settings</span></button><IconButton title="Refresh workspace" onClick={onRefresh}><RefreshCw size={15} /></IconButton><div className="user-chip"><span className="user-avatar">{initials(userName)}</span><span><strong>{userName}</strong><small>{workspaceRole || "Workspace user"}</small></span></div><IconButton title="Sign out" onClick={onLogout}><LogOut size={15} /></IconButton></div></header>;
}

export default function Dashboard() {
  const [view, setView] = useState<View>("rooms");
  const [roomTab, setRoomTab] = useState<RoomTab>("live");
  const [rooms, setRooms] = useState<Bridge[]>([]);
  const [roomStates, setRoomStates] = useState<Record<string, RoomState>>({});
  const [selectedRoomId, setSelectedRoomId] = useState("");
  const [loading, setLoading] = useState(true);
  const [notice, setNotice] = useState<Notice>(null);
  const [createOpen, setCreateOpen] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<Bridge | null>(null);
  const [deleteBusy, setDeleteBusy] = useState(false);
  const [deleteError, setDeleteError] = useState("");
  const [roomSidebarCollapsed, setRoomSidebarCollapsed] = useState(false);
  const [identity, setIdentity] = useState<AuthMe | null>(null);

  const selectedRoom = rooms.find((room) => room.id === selectedRoomId);
  const selectedState = selectedRoomId ? roomStates[selectedRoomId] : undefined;

  const notify = useCallback((message: string, tone: "success" | "error" = "success") => {
    setNotice({ message, tone });
    window.setTimeout(() => setNotice((current) => current?.message === message ? null : current), 4500);
  }, []);

  const loadRooms = useCallback(async () => {
    try {
      const data = await request<Bridge[]>("/api/v1/bridges");
      setRooms(data || []);
      setSelectedRoomId((current) => current && data.some((room) => room.id === current) ? current : data[0]?.id || "");
    } catch (error) {
      notify(error instanceof Error ? error.message : "Conference rooms could not be loaded", "error");
    } finally {
      setLoading(false);
    }
  }, [notify]);

  const loadIdentity = useCallback(async () => {
    try { setIdentity(await request<AuthMe>("/api/v1/auth/me")); } catch (error) { notify(error instanceof Error ? error.message : "Session could not be loaded", "error"); }
  }, [notify]);

  const loadRoom = useCallback(async (roomId: string) => {
    if (!roomId) return;
    try {
      const data = await request<RoomState>(`/api/v1/bridges/${roomId}`);
      setRoomStates((current) => ({ ...current, [roomId]: data }));
    } catch (error) {
      notify(error instanceof Error ? error.message : "Room state could not be loaded", "error");
    }
  }, [notify]);

  const loadAllRooms = useCallback(async () => {
    if (!rooms.length) return;
    const results = await Promise.all(rooms.map(async (room) => {
      try { return await request<RoomState>(`/api/v1/bridges/${room.id}`); } catch { return null; }
    }));
    setRoomStates((current) => {
      const next = { ...current };
      results.forEach((result) => { if (result) next[result.bridge_id] = result; });
      return next;
    });
  }, [rooms]);

  useEffect(() => { void loadIdentity(); void loadRooms(); }, [loadIdentity, loadRooms]);
  useEffect(() => {
    if (!rooms.length) return;
    void loadAllRooms();
    const timer = window.setInterval(() => void loadAllRooms(), 4000);
    return () => window.clearInterval(timer);
  }, [loadAllRooms, rooms.length]);
  useEffect(() => {
    if (view === "room" && selectedRoomId) void loadRoom(selectedRoomId);
  }, [view, selectedRoomId, loadRoom]);

  async function logout() {
    try { await request("/api/v1/auth/logout", { method: "POST" }); } finally { window.location.href = "/login"; }
  }

  function navigate(next: View) {
    setView(next);
    if (next === "rooms") setRoomTab("live");
  }

  function openRoom(roomId: string, tab: RoomTab = "live") {
    setSelectedRoomId(roomId);
    setRoomTab(tab);
    setView("room");
    setRoomSidebarCollapsed(false);
    void loadRoom(roomId);
  }

  async function createRoom(name: string) {
    try {
      const created = await request<Bridge>("/api/v1/bridges", { method: "POST", body: JSON.stringify({ name }) });
      setRooms((current) => [created, ...current]);
      setSelectedRoomId(created.id);
      setCreateOpen(false);
      notify(`${name} is ready for participants`);
      setView("room");
    } catch (error) {
      notify(error instanceof Error ? error.message : "Room could not be created", "error");
    }
  }

  async function deleteRoom(roomId: string, roomName: string) {
    if (deleteBusy) return;
    setDeleteBusy(true);
    setDeleteError("");
    try {
      await request(`/api/v1/bridges/${roomId}`, { method: "DELETE" });
      setRooms((current) => current.filter((room) => room.id !== roomId));
      setRoomStates((current) => {
        const next = { ...current };
        delete next[roomId];
        return next;
      });
      if (selectedRoomId === roomId) {
        setSelectedRoomId("");
        setView("rooms");
      }
      notify(`${roomName} was deleted`);
      setDeleteTarget(null);
    } catch (error) {
      setDeleteError(error instanceof Error ? error.message : "Room could not be deleted");
    } finally {
      setDeleteBusy(false);
    }
  }

  function refresh() {
    void loadRooms();
    void loadAllRooms();
    if (selectedRoomId) void loadRoom(selectedRoomId);
    notify("Workspace refreshed");
  }

  const roomSummaries = rooms.map((room) => {
    const participants = roomStates[room.id]?.participants || [];
    return { room, participants, connected: participants.filter(isConnected), live: participants.filter(isLive) };
  });
  const allParticipants = roomSummaries.flatMap(({ room, participants }) => participants.map((participant) => ({ room, participant })));
  if (loading) return <div className="loading-screen"><div className="loading-mark"><Sparkles size={20} /></div><p>Loading workspace</p></div>;

  return <div className={`app-shell ${view === "room" ? "room-mode" : ""}`}>
    {view === "room" && selectedRoom && <RoomSidebar room={selectedRoom} tab={roomTab} connectedCount={selectedState?.participants.filter(isConnected).length || 0} liveCount={selectedState?.participants.filter(isLive).length || 0} collapsed={roomSidebarCollapsed} onToggle={() => setRoomSidebarCollapsed((value) => !value)} onTab={setRoomTab} onBack={() => navigate("rooms")} />}
    <main className="main-content">
      <TopBar view={view} room={selectedRoom} userName={identity?.display_name || identity?.username || "Operator"} workspaceRole={identity?.workspace_role || "Workspace user"} onNavigate={navigate} onLogout={() => void logout()} onRefresh={refresh} />
      {notice && <div className={`notice ${notice.tone}`}><span className="notice-icon">{notice.tone === "success" ? <Check size={14} /> : <AlertCircle size={14} />}</span><span>{notice.message}</span><button type="button" onClick={() => setNotice(null)}><X size={14} /></button></div>}
      {view === "rooms" && <RoomsPage summaries={roomSummaries} onOpenRoom={openRoom} onCreateRoom={() => setCreateOpen(true)} onDeleteRoom={(room) => { setDeleteError(""); setDeleteTarget(room); }} />}
      {view === "room" && selectedRoom && <RoomPage room={selectedRoom} state={selectedState || { bridge_id: selectedRoom.id, participants: [] }} tab={roomTab} onTab={setRoomTab} onRefresh={() => void loadRoom(selectedRoom.id)} notify={notify} />}
      {view === "dialer" && <DialerPage rooms={rooms} selectedRoomId={selectedRoomId} onRoomChange={setSelectedRoomId} onOpenRoom={(id) => openRoom(id, "dialer")} notify={notify} />}
      {view === "bulk" && <BulkPage rooms={rooms} selectedRoomId={selectedRoomId} onRoomChange={setSelectedRoomId} onOpenRoom={(id) => openRoom(id, "bulk")} notify={notify} />}
      {view === "activity" && <ActivityPage data={allParticipants} onOpenRoom={openRoom} />}
      {view === "settings" && <SettingsPage identity={identity} />}
    </main>
    {createOpen && <CreateRoomModal onClose={() => setCreateOpen(false)} onCreate={(name) => void createRoom(name)} />}
    {deleteTarget && <DeleteRoomModal room={deleteTarget} error={deleteError} busy={deleteBusy} onClose={() => { if (!deleteBusy) { setDeleteTarget(null); setDeleteError(""); } }} onConfirm={() => void deleteRoom(deleteTarget.id, deleteTarget.name)} />}
  </div>;
}

function RoomsPage({ summaries, onOpenRoom, onCreateRoom, onDeleteRoom }: { summaries: Array<{ room: Bridge; participants: Participant[]; connected: Participant[]; live: Participant[] }>; onOpenRoom: (id: string) => void; onCreateRoom: () => void; onDeleteRoom: (room: Bridge) => void }) {
  const [query, setQuery] = useState("");
  const filtered = summaries.filter(({ room }) => room.name.toLowerCase().includes(query.toLowerCase()));
  return <div className="page-enter">
    <section className="workspace-health-hub"><div className="workspace-health-copy"><div className="eyebrow">Workspace hub</div><h1>Conference room orchestration</h1><p>Create calm rooms for live conferences. Every room keeps people, calls, and operator controls together.</p><div className="workspace-health-actions"><button type="button" className="button primary" onClick={onCreateRoom}><Plus size={15} /> New conference room</button><span className="hero-note"><ShieldCheck size={14} /> Workspace-scoped</span></div></div><div className="workspace-philosophy-card"><span className="workspace-philosophy-quote">“</span><span className="eyebrow">Our philosophy</span><blockquote>Conversations work best when the room feels calm, observable, and ready.</blockquote></div></section>
    <div className="section-heading"><div><div className="eyebrow">Room directory</div><h2>Conference rooms</h2><p className="subtle">Open a room to dial participants, run bulk outreach, and control active callers.</p></div><label className="search-box"><Search size={15} /><input value={query} onChange={(event) => setQuery(event.target.value)} placeholder="Search rooms" aria-label="Search rooms" /></label></div>
    {filtered.length ? <div className="room-grid">{filtered.map(({ room, live }) => <RoomCard key={room.id} room={room} live={live} onOpen={() => onOpenRoom(room.id)} onDelete={() => onDeleteRoom(room)} />)}</div> : <div className="panel empty-state"><div className="empty-state-icon"><Building2 size={21} /></div><h3>{summaries.length ? "No rooms match this search" : "No conference rooms yet"}</h3><p>{summaries.length ? "Try another room name." : "Create a room when you are ready to organize participants. Nothing is created automatically."}</p>{!summaries.length && <button type="button" className="button primary" onClick={onCreateRoom}><Plus size={15} /> Create a conference room</button>}</div>}
  </div>;
}

function RoomCard({ room, live, onOpen, onDelete }: { room: Bridge; live: Participant[]; onOpen: () => void; onDelete: () => void }) {
  const hasLive = live.length > 0;
  return <article className="room-card panel" onClick={onOpen} onKeyDown={(event) => { if (event.key === "Enter" || event.key === " ") onOpen(); }} role="button" tabIndex={0}><div className="room-card-top"><span className="room-mark"><Building2 size={18} /></span><StatusPill label={hasLive ? "Live" : "Ready"} state={hasLive ? "IN_BRIDGE" : undefined} /><button type="button" className="room-delete" title={`Delete ${room.name}`} aria-label={`Delete ${room.name}`} onClick={(event) => { event.stopPropagation(); onDelete(); }}><Trash2 size={15} /></button></div><div className="room-card-body"><div className="room-card-title"><h3>{room.name}</h3><ArrowUpRight size={16} /></div><p>{hasLive ? "Live conference in progress" : "Ready when you are"}</p></div><div className="room-card-foot"><span><span className={`room-live-indicator ${hasLive ? "active" : ""}`} />{hasLive ? `${live.length} participant${live.length === 1 ? "" : "s"} live` : "No active conference"}</span><span className="room-open">Open <ArrowRight size={13} /></span></div></article>;
}

function RoomSidebar({ room, tab, connectedCount, liveCount, collapsed, onToggle, onTab, onBack }: { room: Bridge; tab: RoomTab; connectedCount: number; liveCount: number; collapsed: boolean; onToggle: () => void; onTab: (tab: RoomTab) => void; onBack: () => void }) {
  return <aside className={`room-sidebar ${collapsed ? "collapsed" : ""}`} aria-label="Conference room navigation">
    <div className="room-sidebar-topline"><button type="button" className="room-sidebar-back" onClick={onBack}><ArrowLeft size={14} /> <span>Back to workspace</span></button><button type="button" className="room-sidebar-collapse" onClick={onToggle} title={collapsed ? "Expand room navigation" : "Collapse room navigation"} aria-label={collapsed ? "Expand room navigation" : "Collapse room navigation"}>{collapsed ? <PanelLeftOpen size={15} /> : <PanelLeftClose size={15} />}</button></div>
    <div className="room-sidebar-heading"><span>Conference room</span><strong title={room.name}>{room.name}</strong></div>
    <nav className="room-rail-nav" aria-label="Room sections">
      <RoomSidebarSection title="Live controls">
        <RoomTabButton active={tab === "live"} onClick={() => onTab("live")} icon={Users} label="Live control" detail={liveCount ? `${connectedCount} connected` : "No active calls"} />
        <RoomTabButton active={tab === "activity"} onClick={() => onTab("activity")} icon={Activity} label="Call activity" detail="Room history" />
      </RoomSidebarSection>
      <RoomSidebarSection title="Outreach & media">
        <RoomTabButton active={tab === "bulk"} onClick={() => onTab("bulk")} icon={FileSpreadsheet} label="Bulk outreach" detail="Contact list" />
      </RoomSidebarSection>
      <RoomSidebarSection title="Configuration">
        <RoomTabButton active={tab === "settings"} onClick={() => onTab("settings")} icon={Settings} label="Room settings" detail="Room preferences" />
      </RoomSidebarSection>
    </nav>
    <div className="room-sidebar-footer"><StatusPill label={liveCount ? `${connectedCount} connected` : "Ready"} state={liveCount ? "IN_BRIDGE" : undefined} /></div>
  </aside>;
}

function RoomSidebarSection({ title, children }: { title: string; children: React.ReactNode }) {
  return <section className="room-sidebar-section"><h2>{title}</h2><div>{children}</div></section>;
}

function RoomPage({ room, state, tab, onTab, onRefresh, notify }: { room: Bridge; state: RoomState; tab: RoomTab; onTab: (tab: RoomTab) => void; onRefresh: () => void; notify: (message: string, tone?: "success" | "error") => void }) {
  const live = state.participants.filter(isLive);
  const connected = state.participants.filter(isConnected);
  const roomState = state.room_state || room.room_state || "READY";
  const starting = roomState === "STARTING";
  const running = roomState === "RUNNING";
  async function performAction(participantId: string, action: "mute" | "unmute" | "drop" | "add") {
    try {
      await request(`/api/v1/participants/${participantId}/${action}`, { method: "POST", body: "{}" });
      notify(action === "add" ? "Participant call re-dispatched" : `Participant ${action} action accepted`);
      onRefresh();
    } catch (error) {
      notify(error instanceof Error ? error.message : "Participant action failed", "error");
    }
  }
  async function readVolume(participantId: string) {
    try {
      const result = await request<{ volume_db: number; state: string }>(`/api/v1/participants/${participantId}/volume`);
      notify(`Live volume ${Number(result.volume_db ?? 0).toFixed(1)} dB · ${stateLabel(result.state)}`);
    } catch (error) {
      notify(error instanceof Error ? error.message : "Volume could not be read", "error");
    }
  }
  async function speakerAction(requestID: string, action: "grant" | "withdraw") {
    try {
      await request(`/api/v1/speaker-requests/${requestID}/${action}`, { method: "POST", body: "{}" });
      notify(action === "grant" ? "Participant is now an approved speaker" : "Speaker request withdrawn");
      onRefresh();
    } catch (error) {
      notify(error instanceof Error ? error.message : "Speaker request action failed", "error");
    }
  }
  async function lifecycle(action: "start" | "stop") {
    try {
      await request(`/api/v1/bridges/${room.id}/${action}`, { method: "POST", body: "{}" });
      notify(action === "start" ? "Host call started; listeners will be called after the host answers" : "Room stopped and active calls were released");
      onRefresh();
    } catch (error) {
      notify(error instanceof Error ? error.message : `Room could not ${action}`, "error");
    }
  }
  async function dispatchFloatingDial(name: string, phone: string) {
    await request(`/api/v1/bridges/${room.id}/dial`, { method: "POST", body: JSON.stringify({ name, phone_number: phone }) });
    notify("Ad-hoc participant call dispatched");
    onRefresh();
  }
  const failedChecks = (state.start_checks || []).filter((check) => !check.ok);
  return <div className="page-enter room-page"><div className="room-workspace"><section className="room-content"><div className="room-command-bar"><div className="room-command-identity"><span className="eyebrow">Room operations</span><h1>{room.name}</h1></div><div className="room-command-meta"><span className={`room-state-chip ${roomState.toLowerCase()}`}>{starting ? "Calling host" : running ? "Live" : "Ready"}</span><span>{connected.length} connected</span><span>{state.participants.filter((participant) => participant.role !== "HOST").length} rostered</span></div><div className="room-command-actions"><button type="button" className={`button compact-button ${running || starting ? "danger" : "primary"}`} onClick={() => void lifecycle(running || starting ? "stop" : "start")} disabled={starting}>{running || starting ? <><Ban size={14} /> Stop</> : <><Phone size={14} /> Start</>}</button><button type="button" className="button ghost compact-button" onClick={onRefresh}><RefreshCw size={14} /> Refresh</button></div></div>{!running && failedChecks.length > 0 && <div className="room-start-warning"><AlertCircle size={16} /><span><strong>Room is not ready to start</strong><small>{failedChecks.map((check) => `${check.label}: ${check.detail}`).join(" · ")}</small></span><button type="button" className="text-button" onClick={() => onTab("settings")}>Open settings</button></div>}{tab === "live" && <LiveControl room={room} live={live} requests={state.speaker_requests || []} onAction={performAction} onVolume={readVolume} onSpeakerAction={speakerAction} onDial={dispatchFloatingDial} defaultRegion={state.default_region || room.default_region || "+91"} />}{tab === "dialer" && <DialerPage rooms={[room]} selectedRoomId={room.id} onRoomChange={() => undefined} onOpenRoom={() => { onTab("live"); onRefresh(); }} notify={notify} embedded />}{tab === "bulk" && <BulkPage rooms={[room]} selectedRoomId={room.id} onRoomChange={() => undefined} onOpenRoom={() => { onTab("live"); onRefresh(); }} notify={notify} embedded />}{tab === "activity" && <RoomActivity participants={state.participants} sessions={state.sessions || []} onAction={performAction} />}{tab === "settings" && <RoomSettingsPanel room={room} state={state} onRefresh={onRefresh} notify={notify} />}</section></div></div>;
}

function RoomTabButton({ active, onClick, icon: Icon, label, detail }: { active: boolean; onClick: () => void; icon: LucideIcon; label: string; detail: string }) {
  return <button type="button" role="tab" aria-selected={active} className={`room-tab ${active ? "active" : ""}`} onClick={onClick}><span className="room-tab-icon"><Icon size={16} /></span><span><strong>{label}</strong><small>{detail}</small></span><ChevronRight size={15} className="room-tab-chevron" /></button>;
}

function RoomActivity({ participants, sessions, onAction }: { participants: Participant[]; sessions: SessionSummary[]; onAction: (participantId: string, action: "mute" | "unmute" | "drop" | "add") => Promise<void> }) {
  const [query, setQuery] = useState("");
  const filtered = participants.filter((participant) => `${participant.name} ${participant.phone_number} ${participant.node_id} ${participant.state}`.toLowerCase().includes(query.toLowerCase()));
  const formatDuration = (seconds?: number) => seconds ? `${Math.floor(seconds / 60)}m ${seconds % 60}s` : "—";
  return <section className="room-activity-stack"><div className="panel session-history"><div className="panel-heading"><div><div className="eyebrow">Historical sessions</div><h2>Room call activity</h2><p className="subtle">Every start and stop is recorded with the people included in that room session.</p></div><Clock3 size={18} /></div>{sessions.length ? <div className="session-list">{sessions.map((session) => <article className="session-card" key={session.session_id}><div className="session-card-top"><div><strong>{session.status === "ENDED" ? "Completed room session" : stateLabel(session.status)}</strong><small>{shortTime(session.started_at)}{session.ended_at ? ` → ${shortTime(session.ended_at)}` : ""}</small></div><span className={`role-pill ${session.status.toLowerCase()}`}>{formatDuration(session.duration_seconds)}</span></div><div className="session-members">{session.participants?.length ? session.participants.map((member) => <span key={`${session.session_id}-${member.participant_id || member.phone_number}`}><span className="participant-avatar small">{initials(member.name || member.phone_number)}</span>{member.name || member.phone_number}<b>{member.role === "HOST" ? "Host" : "Participant"}</b></span>) : <span className="subtle">No call legs recorded.</span>}</div></article>)}</div> : <div className="session-empty"><Clock3 size={17} /><span><strong>No room sessions yet</strong><small>Start the room to create its first historical session.</small></span></div>}</div><section className="panel room-activity-panel"><div className="panel-heading"><div><div className="eyebrow">Roster history</div><h2>Participant activity</h2><p className="subtle">Review participants who have joined this room and add back a caller when needed.</p></div><label className="search-box"><Search size={15} /><input value={query} onChange={(event) => setQuery(event.target.value)} placeholder="Search participants" aria-label="Search room participant history" /></label></div>{filtered.length ? <div className="room-activity-table"><div className="room-activity-head"><span>Participant</span><span>Phone number</span><span>State</span><span>Action</span></div>{filtered.map((participant) => <div className="room-activity-row" key={`${participant.participant_id}-${participant.call_id}`}><span className="participant-identity"><span className="participant-avatar">{initials(participant.name || participant.phone_number)}</span><span><strong>{participant.name || "Unnamed participant"}</strong><small>{participant.role === "HOST" ? "Host · " : ""}{participant.phone_number}</small></span></span><span className="mono">{participant.phone_number}</span><StatusPill state={participant.state} /><span>{terminalStates.has(participant.state) ? <button type="button" className="text-button" onClick={() => void onAction(participant.participant_id, "add")}>Add back</button> : isLive(participant) ? <button type="button" className="text-button danger-text" onClick={() => void onAction(participant.participant_id, "drop")}>Drop</button> : <span className="muted-action">No action</span>}</span></div>)}</div> : <EmptyState icon={Activity} title="No participant activity" description={participants.length ? "No participants match the current search." : "This room has no participant history yet."} />}</section></section>;
}

type RoomSettingsProps = { room: Bridge; state: RoomState; onRefresh: () => void; notify: (message: string, tone?: "success" | "error") => void };

function RoomSettingsPanel(props: RoomSettingsProps) {
  const tfn = props.state.tfn?.number || props.room.tfn_number || "";
  return <><section className="room-governance-card panel"><div><div className="eyebrow">Product allocation</div><h2>Room launch requirements</h2><p className="subtle">Capacity and phone identity are controlled by the PhonArch product administrator.</p></div><div className="room-governance-items"><div><span>Participant limit</span><strong>{props.state.participant_limit || props.room.participant_limit || "—"}</strong><small>callers in this room</small></div><div className={!tfn ? "missing" : ""}><span>Assigned TFN</span><strong>{tfn || "Not assigned"}</strong><small>{tfn ? props.state.tfn?.label || props.room.tfn_label || "Room caller identity" : "Admin allocation required before start"}</small></div></div></section><RoomSettingsForm {...props} /></>;
}

function RoomSettingsForm({ room, state, onRefresh, notify }: RoomSettingsProps) {
  const currentHost = state.host || { name: room.host_name || "", phone_number: room.host_phone || "" };
  const [defaultRegion, setDefaultRegion] = useState(state.default_region || room.default_region || "+91");
  const [hostName, setHostName] = useState(currentHost.name);
  const [hostPhone, setHostPhone] = useState(localPhonePart(currentHost.phone_number, state.default_region || room.default_region || "+91"));
  const [participantName, setParticipantName] = useState("");
  const [participantPhone, setParticipantPhone] = useState("");
  const [editingParticipant, setEditingParticipant] = useState<Participant | null>(null);
  const [editingName, setEditingName] = useState("");
  const [editingPhone, setEditingPhone] = useState("");
  const [busy, setBusy] = useState(false);
  async function saveDialingRegion(event: FormEvent) {
    event.preventDefault(); setBusy(true);
    try { await request(`/api/v1/bridges/${room.id}/settings`, { method: "PUT", body: JSON.stringify({ default_region: defaultRegion }) }); notify(`Default dialing region set to ${defaultRegion}`); onRefresh(); } catch (error) { notify(error instanceof Error ? error.message : "Dialing region could not be saved", "error"); } finally { setBusy(false); }
  }
  async function saveHost(event: FormEvent) {
    event.preventDefault(); setBusy(true);
    try { await request(`/api/v1/bridges/${room.id}/host`, { method: "PUT", body: JSON.stringify({ name: hostName.trim(), phone_number: normalizePhone(hostPhone, defaultRegion) }) }); await request(`/api/v1/bridges/${room.id}/settings`, { method: "PUT", body: JSON.stringify({ default_region: defaultRegion }) }); notify("Room host and dialing defaults saved"); onRefresh(); } catch (error) { notify(error instanceof Error ? error.message : "Host could not be saved", "error"); } finally { setBusy(false); }
  }
  async function clearHost() { setBusy(true); try { await request(`/api/v1/bridges/${room.id}/host`, { method: "DELETE" }); setHostName(""); setHostPhone(""); notify("Room host cleared"); onRefresh(); } catch (error) { notify(error instanceof Error ? error.message : "Host could not be cleared", "error"); } finally { setBusy(false); } }
  async function addParticipant(event: FormEvent) {
    event.preventDefault(); setBusy(true);
    try { await request(`/api/v1/bridges/${room.id}/participants`, { method: "POST", body: JSON.stringify({ name: participantName.trim(), phone_number: participantPhone.trim(), role: "LISTENER" }) }); setParticipantName(""); setParticipantPhone(""); notify("Participant added to the fixed roster"); onRefresh(); } catch (error) { notify(error instanceof Error ? error.message : "Participant could not be added", "error"); } finally { setBusy(false); }
  }
  async function removeParticipant(id: string) { try { await request(`/api/v1/participants/${id}/remove`, { method: "POST", body: "{}" }); notify("Participant removed from the fixed roster"); onRefresh(); } catch (error) { notify(error instanceof Error ? error.message : "Participant could not be removed", "error"); } }
  function beginEdit(participant: Participant) { setEditingParticipant(participant); setEditingName(participant.name); setEditingPhone(participant.phone_number); }
  async function saveParticipant(event: FormEvent) {
    event.preventDefault();
    if (!editingParticipant) return;
    setBusy(true);
    try { await request(`/api/v1/participants/${editingParticipant.participant_id}`, { method: "PUT", body: JSON.stringify({ name: editingName.trim(), phone_number: editingPhone.trim() }) }); setEditingParticipant(null); notify("Participant roster details saved"); onRefresh(); } catch (error) { notify(error instanceof Error ? error.message : "Participant could not be saved", "error"); } finally { setBusy(false); }
  }
  const roster = state.participants.filter((participant) => participant.role !== "HOST");
  return <section className="room-settings-grid"><div className="panel settings-main host-panel"><div className="settings-section-title"><div><div className="eyebrow">Room host</div><h2>Fixed host details</h2><p className="subtle">The host is always called first when this room starts. Edit these details while the room is stopped.</p></div><ShieldCheck size={18} /></div><form className="host-form" onSubmit={(event) => void saveHost(event)}><label className="field-label">Host name<input className="field-control" value={hostName} onChange={(event) => setHostName(event.target.value)} placeholder="e.g. Maya Chen" required /></label><label className="field-label">Host phone number<span className="phone-composite"><select className="field-control phone-region" value={defaultRegion} onChange={(event) => setDefaultRegion(event.target.value)} aria-label="Host phone country or region">{dialRegions.map((region) => <option key={region.code} value={region.code}>{region.code}</option>)}</select><input className="field-control" value={hostPhone} onChange={(event) => setHostPhone(event.target.value)} placeholder="98765 43210" inputMode="tel" required /></span>{normalizePhone(hostPhone, defaultRegion) && <small className="phone-preview">Will dial {normalizePhone(hostPhone, defaultRegion)}</small>}</label><div className="settings-action-row"><button className="button primary" disabled={busy}>{busy ? "Saving…" : "Save host"}</button><button type="button" className="button ghost danger-button" onClick={() => void clearHost()} disabled={busy || !hostName}>Clear host</button></div></form><form className="dialing-defaults" onSubmit={(event) => void saveDialingRegion(event)}><div><div className="eyebrow">Dialing defaults</div><h3>{dialRegions.find((region) => region.code === defaultRegion)?.label || defaultRegion}</h3><p className="subtle">The region beside the host number is also the default for local numbers entered by the live dialer and fixed roster.</p></div><button className="button ghost compact-button" disabled={busy}>Save dialing region</button></form></div><div className="panel settings-main roster-panel"><div className="settings-section-title"><div><div className="eyebrow">Fixed participants</div><h2>Roster before the call</h2><p className="subtle">Listeners join muted by default. They stay assigned to this room across sessions.</p></div><Users size={18} /></div><form className="roster-add-form" onSubmit={(event) => void addParticipant(event)}><input className="field-control" value={participantName} onChange={(event) => setParticipantName(event.target.value)} placeholder="Participant name" required /><input className="field-control" value={participantPhone} onChange={(event) => setParticipantPhone(event.target.value)} placeholder="Phone number" inputMode="tel" required /><button className="button primary" disabled={busy}><Plus size={14} /> Add</button></form>{roster.length ? <div className="roster-list">{roster.map((participant) => editingParticipant?.participant_id === participant.participant_id ? <form className="roster-edit-row" key={participant.participant_id} onSubmit={(event) => void saveParticipant(event)}><input className="field-control" value={editingName} onChange={(event) => setEditingName(event.target.value)} aria-label="Edit participant name" required /><input className="field-control" value={editingPhone} onChange={(event) => setEditingPhone(event.target.value)} aria-label="Edit participant phone number" inputMode="tel" required /><button className="button primary compact-button" disabled={busy}>Save</button><button type="button" className="button ghost compact-button" onClick={() => setEditingParticipant(null)}>Cancel</button></form> : <div className="roster-row" key={participant.participant_id}><span className="participant-avatar">{initials(participant.name || participant.phone_number)}</span><span><strong>{participant.name || "Unnamed participant"}</strong><small>{participant.phone_number}</small></span><span className="role-pill">Listener</span><span className="roster-row-actions"><button type="button" className="icon-button" title="Edit participant" onClick={() => beginEdit(participant)}><Pencil size={14} /></button><button type="button" className="icon-button danger" title="Remove participant from roster" onClick={() => void removeParticipant(participant.participant_id)}><Trash2 size={14} /></button></span></div>)}</div> : <div className="session-empty"><Users size={17} /><span><strong>No fixed participants</strong><small>Add them manually or import a Name, Phone Number, Role file.</small></span></div>}</div></section>;
}

function LiveControl({ room, live, requests, onAction, onVolume, onSpeakerAction, onDial, defaultRegion }: { room: Bridge; live: Participant[]; requests: SpeakerRequest[]; onAction: (participantId: string, action: "mute" | "unmute" | "drop" | "add") => Promise<void>; onVolume: (participantId: string) => Promise<void>; onSpeakerAction: (requestID: string, action: "grant" | "withdraw") => Promise<void>; onDial: (name: string, phone: string) => Promise<void>; defaultRegion: string }) {
  const [filter, setFilter] = useState<"all" | "connected" | "dialing">("all");
  const shown = live.filter((participant) => filter === "all" || (filter === "connected" ? isConnected(participant) : !isConnected(participant)));
  const participantByID = new Map(live.map((participant) => [participant.participant_id, participant]));
  return <section className="control-layout"><div className="panel control-panel"><div className="panel-heading"><div><div className="eyebrow">Operator surface</div><h2>Live participants</h2><p className="subtle">Mute, inspect volume, or remove a caller while they are in this room.</p></div><div className="filter-tabs">{(["all", "connected", "dialing"] as const).map((item) => <button type="button" key={item} className={filter === item ? "active" : ""} onClick={() => setFilter(item)}>{item === "all" ? `All ${live.length}` : item === "connected" ? `Connected ${live.filter(isConnected).length}` : `Dialing ${live.filter((participant) => !isConnected(participant)).length}`}</button>)}</div></div>{shown.length ? <div className="participant-table"><div className="participant-table-head"><span>Participant</span><span>Call state</span><span>Controls</span></div>{shown.map((participant) => <ParticipantRow key={participant.participant_id} participant={participant} onAction={onAction} onVolume={onVolume} />)}</div> : <EmptyState icon={Users} title={live.length ? "No participants match this filter" : "No live calls in this room"} description={live.length ? "Choose another live-state filter." : "Start the room to call the fixed host and roster."} />}</div><aside className="control-side"><div className="panel side-card speaker-queue-card"><div className="side-card-heading"><span className="mini-icon"><Hand size={15} /></span><div><div className="eyebrow">Moderated room</div><h3>Speaker requests</h3></div><span className="queue-count">{requests.length}</span></div>{requests.length ? <div className="speaker-request-list">{requests.map((request) => { const participant = participantByID.get(request.participant_id); return <div className="speaker-request" key={request.id}><div className="speaker-request-copy"><strong>{participant?.name || "Participant"}</strong><small>{participant?.phone_number || request.participant_id.slice(0, 8)} · {request.status.toLowerCase()}</small></div><div className="speaker-request-actions">{request.status === "QUEUED" && <button type="button" className="text-button" onClick={() => void onSpeakerAction(request.id, "grant")}>Allow</button>}<button type="button" className="text-button danger-text" onClick={() => void onSpeakerAction(request.id, "withdraw")}>Dismiss</button></div></div>; })}</div> : <p className="subtle speaker-empty">No raised hands. Participants who press 0 will appear here.</p>}</div><div className="panel side-card"><div className="side-card-heading"><span className="mini-icon"><Gauge size={15} /></span><div><div className="eyebrow">Room status</div><h3>Conversation controls</h3></div></div><div className="health-list"><HealthRow label="Participants online" value={String(live.length)} tone={live.length ? "good" : "neutral"} /><HealthRow label="Connected" value={String(live.filter(isConnected).length)} tone={live.some(isConnected) ? "good" : "neutral"} /><HealthRow label="Operator actions" value="Ready" tone="good" /></div></div><FloatingDialer onDial={onDial} defaultRegion={defaultRegion} /></aside></section>;
}

function FloatingDialer({ onDial, defaultRegion }: { onDial: (name: string, phone: string) => Promise<void>; defaultRegion: string }) {
  const dialerRef = useRef<HTMLDivElement>(null);
  const [open, setOpen] = useState(false);
  const [name, setName] = useState("");
  const [phone, setPhone] = useState("");
  const [region, setRegion] = useState(defaultRegion);
  const [busy, setBusy] = useState(false);
  const normalizedPhone = normalizePhone(phone, region);
  useEffect(() => {
    if (!open) return;
    function closeOnOutsidePointer(event: PointerEvent) {
      if (dialerRef.current && !dialerRef.current.contains(event.target as Node)) setOpen(false);
    }
    document.addEventListener("pointerdown", closeOnOutsidePointer);
    return () => document.removeEventListener("pointerdown", closeOnOutsidePointer);
  }, [open]);
  async function submit(event: FormEvent) {
    event.preventDefault();
    if (!normalizedPhone) return;
    setBusy(true);
    try { await onDial(name.trim() || normalizedPhone, normalizedPhone); setName(""); setPhone(""); setOpen(false); } catch { /* parent toast owns the error */ } finally { setBusy(false); }
  }
  return <div ref={dialerRef} className="live-dialer"><button type="button" className="live-dialer-fab" title="Dial an ad-hoc participant" aria-label="Dial an ad-hoc participant" onClick={() => setOpen((value) => !value)}><Phone size={19} /><span>Dial</span></button>{open && <form className="live-dialer-popover panel" onSubmit={(event) => void submit(event)}><div className="eyebrow">Live room dialer</div><h3>Add a caller</h3><p className="subtle">This call is attached to the current room only.</p><input className="field-control" value={name} onChange={(event) => setName(event.target.value)} placeholder="Name" /><label className="field-label">Dialing region<select className="field-control" value={region} onChange={(event) => setRegion(event.target.value)}>{dialRegions.map((item) => <option key={item.code} value={item.code}>{item.label}</option>)}</select></label><input className="field-control" value={phone} onChange={(event) => setPhone(event.target.value)} placeholder="Phone number" inputMode="tel" required />{normalizedPhone && <span className="dial-region-preview">Will dial {normalizedPhone}</span>}<button className="button primary" disabled={busy || !normalizedPhone}><Phone size={14} /> {busy ? "Calling…" : "Dial participant"}</button></form>}</div>;
}

function ParticipantRow({ participant, onAction, onVolume }: { participant: Participant; onAction: (participantId: string, action: "mute" | "unmute" | "drop" | "add") => Promise<void>; onVolume: (participantId: string) => Promise<void> }) {
  const muted = participant.desired_state === "MUTED";
  return <div className="participant-row"><div className="participant-identity"><span className="participant-avatar">{initials(participant.name || participant.phone_number)}</span><span><strong>{participant.name || "Unnamed participant"}</strong><small>{participant.phone_number}</small></span></div><div><StatusPill state={participant.state} /></div><div className="participant-actions"><IconButton title={muted ? "Unmute participant" : "Mute participant"} onClick={() => void onAction(participant.participant_id, muted ? "unmute" : "mute")}><span className={muted ? "icon-active" : ""}>{muted ? <MicOff size={15} /> : <Mic size={15} />}</span></IconButton><IconButton title="Read live volume" onClick={() => void onVolume(participant.participant_id)}><Volume2 size={15} /></IconButton><IconButton title="Drop participant" danger onClick={() => void onAction(participant.participant_id, "drop")}><Ban size={15} /></IconButton></div></div>;
}

function HealthRow({ label, value, tone }: { label: string; value: string; tone: "good" | "neutral" }) {
  return <div className="health-row"><span>{label}</span><strong className={tone}>{value}<i /></strong></div>;
}

function DialerPage({ rooms, selectedRoomId, onRoomChange, onOpenRoom, notify, embedded = false }: { rooms: Bridge[]; selectedRoomId: string; onRoomChange: (value: string) => void; onOpenRoom: (id: string) => void; notify: (message: string, tone?: "success" | "error") => void; embedded?: boolean }) {
  const [name, setName] = useState("");
  const [phone, setPhone] = useState("");
  const selectedRoom = rooms.find((room) => room.id === selectedRoomId);
  const [region, setRegion] = useState(selectedRoom?.default_region || "+91");
  const [busy, setBusy] = useState(false);
  const keypad = ["1", "2", "3", "4", "5", "6", "7", "8", "9", "*", "0", "#"];
  useEffect(() => { setRegion(selectedRoom?.default_region || "+91"); }, [selectedRoomId, selectedRoom?.default_region]);
  const normalizedPhone = normalizePhone(phone, region);
  async function submit(event?: FormEvent) {
    event?.preventDefault();
    if (!selectedRoomId || !normalizedPhone) { notify("Choose a room and enter a phone number", "error"); return; }
    setBusy(true);
    try {
      await request(`/api/v1/bridges/${selectedRoomId}/dial`, { method: "POST", body: JSON.stringify({ name: name.trim() || normalizedPhone, phone_number: normalizedPhone, region }) });
      notify("Call dispatched into the selected room");
      setName(""); setPhone("");
      onOpenRoom(selectedRoomId);
    } catch (error) { notify(error instanceof Error ? error.message : "Call could not be dispatched", "error"); } finally { setBusy(false); }
  }
  return <div className={`page-enter task-page ${embedded ? "embedded-task" : ""}`}><PageHeader eyebrow={embedded ? "Room operation · Single call" : "Tools · Direct dialer"} title="Start a participant call" description="Send one outbound call into this conference room and keep the participant isolated to its room workspace." actions={!embedded && <button type="button" className="button ghost" onClick={() => onOpenRoom(selectedRoomId)} disabled={!selectedRoomId}><Building2 size={15} /> Open room</button>} /><div className="task-grid"><form className="panel dialer-card" onSubmit={(event) => void submit(event)}><div className="task-card-heading"><span className="task-icon"><Phone size={17} /></span><div><div className="eyebrow">Participant call</div><h2>Dial into a room</h2></div><span className="transport-badge"><RadioTower size={12} /> Phone audio</span></div><label className="field-label">Conference room<select className="field-control" value={selectedRoomId} onChange={(event) => onRoomChange(event.target.value)} required><option value="">Choose a room</option>{rooms.map((room) => <option key={room.id} value={room.id}>{room.name}</option>)}</select></label><label className="field-label">Dialing region<select className="field-control" value={region} onChange={(event) => setRegion(event.target.value)}>{dialRegions.map((item) => <option key={item.code} value={item.code}>{item.label}</option>)}</select></label><label className="field-label">Participant name <span>Optional</span><input className="field-control" value={name} onChange={(event) => setName(event.target.value)} placeholder="e.g. Maya Chen" /></label><label className="field-label">Phone number<input className="field-control phone-input" value={phone} onChange={(event) => setPhone(event.target.value)} placeholder="98765 43210" inputMode="tel" required /></label>{normalizedPhone && <span className="dial-region-preview">Will dial {normalizedPhone}</span>}<div className="keypad">{keypad.map((key) => <button type="button" key={key} onClick={() => setPhone((current) => current + key)}>{key}</button>)}</div><div className="task-footer"><span><ShieldCheck size={14} /> This participant will appear only in the selected room.</span><button className="button primary" disabled={busy || !selectedRoomId || !normalizedPhone}>{busy ? "Dispatching…" : <><Phone size={15} /> Start call</>}</button></div></form><div className="panel guidance-card"><div className="eyebrow">Room experience</div><h2>Every call has a clear home.</h2><p className="subtle">The participant is attached to this room before dialing. Once connected, operators can mute, inspect volume, or remove the caller from the Live control page.</p><div className="routing-steps"><RoutingStep index="01" icon={Building2} title="Choose the room" description="The room is the boundary for the participant and its controls." /><RoutingStep index="02" icon={Phone} title="Start the call" description="The call is placed and its status appears in this room." /><RoutingStep index="03" icon={Users} title="Operate together" description="Keep the live conversation focused in one workspace." /></div></div></div></div>;
}

function RoutingStep({ index, icon: Icon, title, description }: { index: string; icon: LucideIcon; title: string; description: string }) {
  return <div className="routing-step"><span className="step-index">{index}</span><span className="step-icon"><Icon size={15} /></span><span><strong>{title}</strong><small>{description}</small></span></div>;
}

function BulkPage({ rooms, selectedRoomId, onRoomChange, onOpenRoom, notify, embedded = false }: { rooms: Bridge[]; selectedRoomId: string; onRoomChange: (value: string) => void; onOpenRoom: (id: string) => void; notify: (message: string, tone?: "success" | "error") => void; embedded?: boolean }) {
  const [contacts, setContacts] = useState<Contact[]>([]);
  const [busy, setBusy] = useState(false);
  const validContacts = useMemo(() => contacts.filter((contact) => contact.valid), [contacts]);
  async function parseFile(event: ChangeEvent<HTMLInputElement>) {
    const file = event.target.files?.[0];
    if (!file) return;
    try {
      const workbook = XLSX.read(await file.arrayBuffer(), { type: "array" });
      const sheet = workbook.Sheets[workbook.SheetNames[0]];
      const rows = XLSX.utils.sheet_to_json<Record<string, unknown>>(sheet, { defval: "" });
      const parsed = rows.map((row) => {
        const entries = Object.entries(row);
        const name = String(entries.find(([key]) => /^(name|full.?name)$/i.test(key))?.[1] ?? "").trim();
        const phone = String(entries.find(([key]) => /^(phone|phone.?number|number|mobile)$/i.test(key))?.[1] ?? "").trim();
        const rawRole = String(entries.find(([key]) => /^(role|type|participant.?role)$/i.test(key))?.[1] ?? "LISTENER").trim().toUpperCase();
        const role = rawRole === "HOST" ? "HOST" : "LISTENER";
        const valid = /^\+?[0-9][0-9 ()-]{5,}$/.test(phone);
        return { name, phone_number: phone, role, valid, reason: !name ? "Missing name" : !phone ? "Missing phone number" : !valid ? "Invalid phone number" : undefined };
      });
      setContacts(parsed);
      notify(`${parsed.length} rows parsed · ${parsed.filter((contact) => contact.valid).length} ready`);
    } catch { notify("The spreadsheet could not be read", "error"); }
  }
  async function launch() {
    if (!selectedRoomId || !validContacts.length) { notify("Choose a room and add at least one valid contact", "error"); return; }
    setBusy(true);
    try {
      const result = await request<{ batch_id: string }>(`/api/v1/bridges/${selectedRoomId}/batches`, { method: "POST", body: JSON.stringify({ contacts: validContacts.map(({ name, phone_number, role }) => ({ name, phone_number, role })) }) });
      notify(`Fixed roster imported · ${result.batch_id.slice(0, 8)}`);
      onOpenRoom(selectedRoomId);
    } catch (error) { notify(error instanceof Error ? error.message : "Batch could not be queued", "error"); } finally { setBusy(false); }
  }
  return <div className={`page-enter task-page ${embedded ? "embedded-task" : ""}`}><PageHeader eyebrow={embedded ? "Room operation · Fixed roster" : "Tools · Bulk roster"} title="Import fixed participants" description="Preview XLSX or CSV contacts locally. Name, Phone Number, and Role define the room roster before any calls begin." actions={!embedded && <button type="button" className="button ghost" onClick={() => onOpenRoom(selectedRoomId)} disabled={!selectedRoomId}><Building2 size={15} /> Open room</button>} /><div className="panel bulk-card"><div className="bulk-toolbar"><div className="task-card-heading"><span className="task-icon"><FileSpreadsheet size={17} /></span><div><div className="eyebrow">Fixed roster import</div><h2>Prepare room members</h2></div></div><label className="button primary upload-button"><Upload size={15} /> Choose XLSX / CSV<input type="file" accept=".xlsx,.xls,.csv" onChange={(event) => void parseFile(event)} /></label></div><div className="bulk-options"><label className="field-label">Target room<select className="field-control" value={selectedRoomId} onChange={(event) => onRoomChange(event.target.value)} required><option value="">Choose a room</option>{rooms.map((room) => <option key={room.id} value={room.id}>{room.name}</option>)}</select></label><div className="bulk-protection"><ShieldCheck size={16} /><span><strong>Calls do not start on import</strong><small>Configure one HOST row or Room settings, then start the room to call host first.</small></span></div></div>{contacts.length ? <><div className="preview-header"><div><div className="eyebrow">Import preview</div><h3>{validContacts.length} ready of {contacts.length} rows</h3></div><span className="preview-summary"><Check size={13} /> Name · Phone · Role</span></div><div className="contact-table"><div className="contact-table-head"><span>Name</span><span>Phone number</span><span>Role</span><span>Validation</span></div>{contacts.slice(0, 100).map((contact, index) => <div className="contact-row" key={`${contact.phone_number}-${index}`}><span>{contact.name || "Unnamed contact"}</span><span className="mono">{contact.phone_number || "—"}</span><span className="role-pill">{contact.role === "HOST" ? "Host" : "Listener"}</span><span className={contact.valid ? "valid" : "invalid"}>{contact.valid ? <><Check size={13} /> Ready</> : <><AlertCircle size={13} /> {contact.reason}</>}</span></div>)}</div><div className="bulk-footer"><span>{contacts.length > 100 ? "Showing first 100 rows · " : ""}{validContacts.length} valid roster members will be imported.</span><button type="button" className="button primary" onClick={() => void launch()} disabled={busy || !selectedRoomId || !validContacts.length}><Users size={15} /> {busy ? "Importing…" : "Import roster"}</button></div></> : <EmptyState icon={FileSpreadsheet} title="No roster loaded" description="Choose an XLSX or CSV file with Name, Phone Number, and Role columns. Role may be HOST or LISTENER." action={<div className="file-format-note"><span>Name</span><span>Phone Number</span><span>Role</span><span>CSV / XLSX</span></div>} />}</div></div>;
}

function ActivityPage({ data, onOpenRoom }: { data: Array<{ room: Bridge; participant: Participant }>; onOpenRoom: (id: string) => void }) {
  const [query, setQuery] = useState("");
  const [filter, setFilter] = useState("all");
  const filtered = data.filter(({ room, participant }) => {
    const matchesQuery = `${room.name} ${participant.name} ${participant.phone_number}`.toLowerCase().includes(query.toLowerCase());
    return matchesQuery && (filter === "all" || participant.state === filter);
  });
  return <div className="page-enter"><PageHeader eyebrow="Workspace · Observability" title="Call activity" description="Review every participant leg across the workspace. Live operations remain inside their conference room." actions={<button type="button" className="button ghost" onClick={() => setFilter("all")}><Activity size={15} /> Clear filters</button>} /><div className="panel activity-panel"><div className="activity-toolbar"><label className="search-box"><Search size={15} /><input value={query} onChange={(event) => setQuery(event.target.value)} placeholder="Search room, participant, or phone" /></label><select className="field-control compact-select" value={filter} onChange={(event) => setFilter(event.target.value)}><option value="all">All call states</option><option value="IN_BRIDGE">In bridge</option><option value="RINGING">Ringing</option><option value="DISPATCHED">Dispatched</option><option value="DROPPED">Dropped</option><option value="FAILED">Failed</option><option value="ENDED">Ended</option></select><span className="activity-count">{filtered.length} leg{filtered.length === 1 ? "" : "s"}</span></div>{filtered.length ? <div className="activity-table"><div className="activity-table-head"><span>Participant</span><span>Room</span><span>State</span><span>Node ownership</span><span>Last action</span></div>{filtered.map(({ room, participant }) => <button type="button" className="activity-row" key={`${room.id}-${participant.participant_id}-${participant.call_id}`} onClick={() => onOpenRoom(room.id)}><span className="participant-identity"><span className="participant-avatar">{initials(participant.name || participant.phone_number)}</span><span><strong>{participant.name || "Unnamed participant"}</strong><small>{participant.phone_number}</small></span></span><span className="activity-room"><Building2 size={13} /> {room.name}</span><StatusPill state={participant.state} /><span className="ownership"><span><span className="ownership-dot" />{participant.node_id || "No node"}</span><small>{participant.call_id ? `Call ${participant.call_id.slice(0, 8)}` : "No call ID"}</small></span><span className="activity-open">Open room <ArrowUpRight size={13} /></span></button>)}</div> : <EmptyState icon={Activity} title="No call activity" description={data.length ? "No call legs match the current filters." : "Once a call is dispatched, its lifecycle will appear here."} />}</div></div>;
}

function LegacySettingsPage() {
  return <div className="page-enter"><PageHeader eyebrow="Workspace · Administration" title="Workspace settings" description="Manage who can access this workspace and the defaults shared by its conference rooms." actions={<span className="scope-badge"><ShieldCheck size={14} /> Workspace-scoped</span>} /><div className="settings-grid"><section className="panel settings-main"><div className="settings-intro"><span className="settings-icon"><Users size={17} /></span><div><div className="eyebrow">Access management</div><h2>User management</h2><p className="subtle">Workspace access is controlled by administrators. Invite and role management can be connected to your identity provider here.</p></div><span className="scope-badge"><ShieldCheck size={14} /> Admin access</span></div><div className="settings-fields"><div><span>Signed-in operator</span><strong>Admin</strong></div><div><span>Role</span><strong>Administrator</strong></div><div><span>Workspace</span><strong>Operations</strong></div><div><span>Room access</span><strong>All conference rooms</strong></div></div></section><section className="panel settings-main"><div className="settings-section-title"><div><div className="eyebrow">Workspace defaults</div><h2>Conference experience</h2><p className="subtle">Customer-facing settings that apply to rooms created in this workspace.</p></div><Building2 size={18} /></div><div className="policy-list"><PolicyRow icon={Users} title="Room membership" value="Workspace members" detail="Only authorized users can operate rooms" /><PolicyRow icon={Mic} title="Participant controls" value="Enabled" detail="Mute, unmute, volume, and remove callers" /><PolicyRow icon={ShieldCheck} title="Access boundary" value="Private" detail="Rooms and participants stay within this workspace" /></div></section><section className="panel settings-main settings-note-card"><div className="settings-note-icon"><LifeBuoy size={17} /></div><div><div className="eyebrow">Operator guidance</div><h3>Room health belongs in the room.</h3><p className="subtle">Open a conference room to dial participants, run outreach, and control the live conversation from its left-hand workspace navigation.</p></div></section></div></div>;
  return <div className="page-enter"><PageHeader eyebrow="Workspace · Administration" title="Workspace settings" description="The workspace is the boundary for conference rooms, routing policy, and operator visibility." actions={<span className="scope-badge"><ShieldCheck size={14} /> Workspace-scoped</span>} /><div className="settings-grid"><section className="panel settings-main"><div className="settings-intro"><span className="settings-icon"><Building2 size={17} /></span><div><div className="eyebrow">Workspace identity</div><h2>Operations</h2><p className="subtle">The default workspace for conference room operations.</p></div><span className="scope-badge"><ShieldCheck size={14} /> Protected</span></div><div className="settings-fields"><div><span>Workspace slug</span><strong>operations</strong></div><div><span>Access role</span><strong>Administrator</strong></div><div><span>Call data boundary</span><strong>PostgreSQL + Redis</strong></div><div><span>Media boundary</span><strong>RTP relay only</strong></div></div></section><section className="panel settings-main"><div className="settings-section-title"><div><div className="eyebrow">Transport policy</div><h2>Telephony surface</h2><p className="subtle">Explicitly SIP and RTP. The browser never becomes a media endpoint.</p></div><Network size={18} /></div><div className="policy-list"><PolicyRow icon={RadioTower} title="SIP signaling" value="UDP + TCP · port 5060" detail="Edge proxy receives provider traffic" /><PolicyRow icon={Activity} title="RTP media" value="Relay through PBX nodes" detail="No WebRTC or WSS transport" /><PolicyRow icon={ServerCog} title="Node discovery" value="Redis TTL heartbeat" detail="Capacity and active calls guide routing" /></div></section><section className="panel settings-main"><div className="settings-section-title"><div><div className="eyebrow">Routing policy</div><h2>Fail-safe operations</h2><p className="subtle">Each call stays attached to the PBX that owns its SIP dialog.</p></div><ShieldCheck size={18} /></div><div className="policy-list"><PolicyRow icon={Gauge} title="Distribution" value="Least-load round robin" detail="Active call count and load score" /><PolicyRow icon={Clock3} title="Heartbeat expiry" value="Aggressive TTL" detail="Stale nodes leave the routing pool" /><PolicyRow icon={Users} title="Operator controls" value="Owner-resolved" detail="Mute, volume, drop, and add-back" /></div></section><section className="panel settings-main settings-note-card"><div className="settings-note-icon"><LifeBuoy size={17} /></div><div><div className="eyebrow">Operational note</div><h3>Room health belongs in the room.</h3><p className="subtle">Use Call activity for workspace-wide history. Open a conference room to operate active participants and inspect its recent call legs.</p></div></section></div></div>;
}

type WorkspaceMember = { operator_id: string; username: string; display_name: string; status: string; role: string };

function SettingsPage({ identity }: { identity: AuthMe | null }) {
  const [members, setMembers] = useState<WorkspaceMember[]>([]);
  const [displayName, setDisplayName] = useState("");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [role, setRole] = useState("OPERATOR");
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState("");
  const canManageUsers = Boolean(identity?.can_manage_users);

  async function loadMembers() {
    try { setMembers(await request<WorkspaceMember[]>("/api/v1/workspace/members")); } catch (error) { setMessage(error instanceof Error ? error.message : "Workspace members could not be loaded"); }
  }

  useEffect(() => { void loadMembers(); }, []);

  async function addUser(event: FormEvent) {
    event.preventDefault(); setBusy(true); setMessage("");
    try {
      await request("/api/v1/workspace/members", { method: "POST", body: JSON.stringify({ display_name: displayName, username, password, role }) });
      setDisplayName(""); setUsername(""); setPassword(""); setRole("OPERATOR"); setMessage("Workspace user added"); await loadMembers();
    } catch (error) { setMessage(error instanceof Error ? error.message : "User could not be added"); } finally { setBusy(false); }
  }

  async function updateRole(member: WorkspaceMember, nextRole: string) {
    setBusy(true); setMessage("");
    try { await request(`/api/v1/workspace/members/${member.operator_id}`, { method: "PUT", body: JSON.stringify({ role: nextRole }) }); setMessage("Workspace role updated"); await loadMembers(); } catch (error) { setMessage(error instanceof Error ? error.message : "Role could not be updated"); } finally { setBusy(false); }
  }

  async function removeUser(member: WorkspaceMember) {
    if (!window.confirm(`Remove ${member.display_name || member.username} from this workspace?`)) return;
    setBusy(true); setMessage("");
    try { await request(`/api/v1/workspace/members/${member.operator_id}`, { method: "DELETE" }); setMessage("Workspace access removed"); await loadMembers(); } catch (error) { setMessage(error instanceof Error ? error.message : "User could not be removed"); } finally { setBusy(false); }
  }

  return <div className="page-enter"><PageHeader eyebrow="Workspace · Administration" title="Workspace settings" description="Manage users and defaults inside this workspace. Product allocation, TFNs, and cross-workspace policy remain isolated in the product admin console." actions={<span className="scope-badge"><ShieldCheck size={14} /> {identity?.workspace_role || "Workspace-scoped"}</span>} /><div className="settings-grid"><section className="panel settings-main"><div className="settings-intro"><span className="settings-icon"><Users size={17} /></span><div><div className="eyebrow">Workspace access</div><h2>User management</h2><p className="subtle">Workspace owners and admins can provision customer users. Product administrators manage cross-workspace access separately.</p></div><span className="scope-badge"><ShieldCheck size={14} /> {canManageUsers ? "Manage access" : "Read only"}</span></div>{canManageUsers && <form className="workspace-user-form" onSubmit={(event) => void addUser(event)}><input className="field-control" value={displayName} onChange={(event) => setDisplayName(event.target.value)} placeholder="Display name" required /><input className="field-control" value={username} onChange={(event) => setUsername(event.target.value)} placeholder="Username" required /><input className="field-control" type="password" minLength={8} value={password} onChange={(event) => setPassword(event.target.value)} placeholder="Temporary password" required /><select className="field-control" value={role} onChange={(event) => setRole(event.target.value)}><option value="OPERATOR">Operator</option><option value="ADMIN">Workspace admin</option><option value="VIEWER">Viewer</option></select><button className="button primary" disabled={busy}><Plus size={14} /> Add user</button></form>}{message && <div className="settings-inline-message">{message}</div>}<div className="workspace-member-list">{members.map((member) => <div className="workspace-member-row" key={member.operator_id}><span className="user-avatar">{initials(member.display_name || member.username)}</span><span><strong>{member.display_name || member.username}</strong><small>{member.username} · {member.status}</small></span>{member.role === "OWNER" ? <span className="role-pill">OWNER</span> : canManageUsers ? <><select className="field-control compact-select" value={member.role} onChange={(event) => void updateRole(member, event.target.value)} disabled={busy}><option value="ADMIN">ADMIN</option><option value="OPERATOR">OPERATOR</option><option value="VIEWER">VIEWER</option></select><button type="button" className="button ghost compact-button" onClick={() => void removeUser(member)} disabled={busy}>Remove</button></> : <span className="role-pill">{member.role}</span>}</div>)}</div></section><section className="panel settings-main"><div className="settings-section-title"><div><div className="eyebrow">Workspace defaults</div><h2>Conference experience</h2><p className="subtle">Customer-facing settings shared by this workspace. Room-specific host, roster, and operations remain inside each room.</p></div><Building2 size={18} /></div><div className="policy-list"><PolicyRow icon={Users} title="Room membership" value="Workspace members" detail="Only authorized users can operate rooms" /><PolicyRow icon={Mic} title="Participant controls" value="Enabled" detail="Mute, unmute, volume, and remove callers" /><PolicyRow icon={ShieldCheck} title="Access boundary" value="Private" detail="Rooms and participants stay within this workspace" /></div></section><section className="panel settings-main settings-note-card"><div className="settings-note-icon"><LifeBuoy size={17} /></div><div><div className="eyebrow">Access boundary</div><h3>Product administration is isolated.</h3><p className="subtle">Product owners use the separate Admin Console to select any workspace, allocate TFNs, and set room policy. Workspace admins only manage users in this workspace.</p></div></section></div></div>;
}

function PolicyRow({ icon: Icon, title, value, detail }: { icon: LucideIcon; title: string; value: string; detail: string }) {
  return <div className="policy-row"><span className="policy-icon"><Icon size={15} /></span><span><strong>{title}</strong><small>{detail}</small></span><b>{value}</b><Check size={14} className="policy-check" /></div>;
}

function CreateRoomModal({ onClose, onCreate }: { onClose: () => void; onCreate: (name: string) => void }) {
  const [name, setName] = useState("");
  return <div className="modal-backdrop" role="presentation" onMouseDown={(event) => { if (event.target === event.currentTarget) onClose(); }}><form className="modal-card" onSubmit={(event) => { event.preventDefault(); if (name.trim()) onCreate(name.trim()); }}><div className="modal-heading"><div><div className="eyebrow">New workspace resource</div><h2>Create conference room</h2><p className="subtle">A room starts empty and becomes live only when a call is dispatched.</p></div><IconButton title="Close" onClick={onClose}><X size={16} /></IconButton></div><label className="field-label">Room name<input className="field-control" autoFocus value={name} onChange={(event) => setName(event.target.value)} placeholder="e.g. Customer success briefing" required /></label><div className="modal-footer"><span><ShieldCheck size={14} /> Protected by workspace access</span><div><button type="button" className="button ghost" onClick={onClose}>Cancel</button><button className="button primary" disabled={!name.trim()}><Plus size={15} /> Create room</button></div></div></form></div>;
}

function DeleteRoomModal({ room, error, busy, onClose, onConfirm }: { room: Bridge; error: string; busy: boolean; onClose: () => void; onConfirm: () => void }) {
  return <div className="modal-backdrop delete-modal-backdrop" role="presentation" onMouseDown={(event) => { if (event.target === event.currentTarget && !busy) onClose(); }}><section className="modal-card delete-modal" role="dialog" aria-modal="true" aria-labelledby="delete-room-title"><div className="delete-modal-top"><span className="delete-modal-icon"><Trash2 size={19} /></span><IconButton title="Close delete dialog" onClick={onClose} disabled={busy}><X size={16} /></IconButton></div><div className="delete-modal-copy"><div className="eyebrow">Remove conference room</div><h2 id="delete-room-title">Delete {room.name}?</h2><p className="subtle">This will permanently remove the room from your workspace and delete its participant history.</p></div><div className="delete-modal-warning"><AlertCircle size={16} /><span><strong>Permanent action</strong><small>Active participants must be removed before this room can be deleted.</small></span></div>{error && <div className="delete-modal-error"><AlertCircle size={14} /><span>{error}</span></div>}<div className="delete-modal-actions"><button type="button" className="button ghost" onClick={onClose} disabled={busy}>Keep room</button><button type="button" className="button danger" onClick={onConfirm} disabled={busy}>{busy ? "Deleting…" : <><Trash2 size={15} /> Delete room</>}</button></div></section></div>;
}
