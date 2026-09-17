"use client";

import { FormEvent, useState } from "react";
import { ArrowRight, Check, Eye, EyeOff, KeyRound, LockKeyhole, ShieldCheck, Sparkles } from "lucide-react";

const API = process.env.NEXT_PUBLIC_CONTROL_API_URL || "";

export default function AdminLoginPage() {
  const [username, setUsername] = useState("admin");
  const [password, setPassword] = useState("");
  const [showPassword, setShowPassword] = useState(false);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  async function submit(event: FormEvent) {
    event.preventDefault(); setBusy(true); setError("");
    try {
      const response = await fetch(`${API}/api/v1/auth/admin/login`, { method: "POST", credentials: "include", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ username, password }) });
      if (!response.ok) { setError("Only active product owners and managers can access this console."); return; }
      window.location.href = "/admin";
    } catch { setError("The product control plane is unreachable."); } finally { setBusy(false); }
  }

  return <main className="login-layout admin-login-layout"><section className="login-story"><div className="brand-lockup login-brand"><span className="brand-mark"><Sparkles size={18} /></span><span><strong>Phonarch</strong><small>Product administration</small></span></div><div className="login-story-copy"><div className="eyebrow">Owner control plane</div><h1>Govern every workspace with <em>clarity.</em></h1><p>Allocate workspaces, room capacity, telephony numbers, and customer access from the isolated product administration console.</p><div className="login-feature-list"><LoginFeature icon={ShieldCheck} title="Cross-workspace governance" detail="Select one workspace at a time while retaining owner visibility" /><LoginFeature icon={KeyRound} title="Customer access lifecycle" detail="Provision and manage workspace-scoped users and roles" /><LoginFeature icon={LockKeyhole} title="Separate identity boundary" detail="Product admin sessions are isolated from customer logins" /></div></div><div className="login-story-footer"><span><span className="live-pulse" /> Admin control plane ready</span><a href="/login">Back to workspace login</a></div></section><section className="login-panel-wrap"><div className="login-panel"><div className="login-panel-top"><span className="login-panel-icon"><KeyRound size={17} /></span><span className="secure-label"><span className="status-dot" /> Product owner access</span></div><div className="login-heading"><div className="eyebrow">Restricted console</div><h2>Admin Console</h2><p>Sign in with a product owner or product manager account.</p></div><form onSubmit={(event) => void submit(event)}><label className="field-label">Username<input className="field-control" value={username} onChange={(event) => setUsername(event.target.value)} autoComplete="username" autoFocus required /></label><label className="field-label">Password<div className="password-field"><input className="field-control" type={showPassword ? "text" : "password"} value={password} onChange={(event) => setPassword(event.target.value)} autoComplete="current-password" required /><button type="button" onClick={() => setShowPassword((current) => !current)} aria-label={showPassword ? "Hide password" : "Show password"}>{showPassword ? <EyeOff size={16} /> : <Eye size={16} />}</button></div></label>{error && <div className="login-error"><span><ShieldCheck size={14} /></span>{error}</div>}<button className="button primary login-submit" disabled={busy}>{busy ? "Authenticating…" : <>Open Admin Console <ArrowRight size={15} /></>}</button></form><div className="login-panel-foot"><span><Check size={13} /> HttpOnly admin session</span><span>No customer workspace access is granted here.</span></div></div><div className="login-orbit orbit-login-a" /><div className="login-orbit orbit-login-b" /></section></main>;
}

function LoginFeature({ icon: Icon, title, detail }: { icon: typeof ShieldCheck; title: string; detail: string }) {
  return <div className="login-feature"><span className="login-feature-icon"><Icon size={15} /></span><span><strong>{title}</strong><small>{detail}</small></span></div>;
}
