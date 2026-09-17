"use client";

import { FormEvent, useState } from "react";
import { ArrowRight, Check, Eye, EyeOff, LockKeyhole, Network, RadioTower, ShieldCheck, Sparkles } from "lucide-react";

const API = process.env.NEXT_PUBLIC_CONTROL_API_URL || "";

export default function LoginPage() {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [showPassword, setShowPassword] = useState(false);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  async function submit(event: FormEvent) {
    event.preventDefault();
    setBusy(true);
    setError("");
    try {
      const response = await fetch(`${API}/api/v1/auth/login`, {
        method: "POST",
        credentials: "include",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ username, password }),
      });
      if (!response.ok) {
        setError("Those credentials were not accepted. Check the account and try again.");
        return;
      }
      window.location.href = "/";
    } catch {
      setError("The control plane is unreachable. Check that the service is running.");
    } finally {
      setBusy(false);
    }
  }

  return <main className="login-layout"><section className="login-story"><div className="brand-lockup login-brand"><span className="brand-mark"><Sparkles size={18} /></span><span><strong>Phonarch</strong><small>Conference operations</small></span></div><div className="login-story-copy"><div className="eyebrow">SIP operations workspace</div><h1>Make every conference feel <em>intentional.</em></h1><p>One calm command surface for rooms, participants, routing ownership, and live operator control.</p><div className="login-feature-list"><LoginFeature icon={RadioTower} title="Pure SIP + RTP" detail="UDP/TCP signaling with media relay only" /><LoginFeature icon={Network} title="Capacity-aware routing" detail="Healthy PBX nodes share the call load" /><LoginFeature icon={ShieldCheck} title="Operator-safe controls" detail="Every action resolves to the owning call leg" /></div></div><div className="login-story-footer"><span><span className="live-pulse" /> Edge proxy ready</span><span>PhonArch Conference · v1.0</span></div></section><section className="login-panel-wrap"><div className="login-panel"><div className="login-panel-top"><span className="login-panel-icon"><LockKeyhole size={17} /></span><span className="secure-label"><span className="status-dot" /> Private workspace</span></div><div className="login-heading"><div className="eyebrow">Welcome back</div><h2>Sign in to Phonarch</h2><p>Access your conference rooms and live operations workspace.</p></div><form onSubmit={(event) => void submit(event)}><label className="field-label">Username<input className="field-control" value={username} onChange={(event) => setUsername(event.target.value)} autoComplete="username" autoFocus required /></label><label className="field-label">Password<div className="password-field"><input className="field-control" type={showPassword ? "text" : "password"} value={password} onChange={(event) => setPassword(event.target.value)} autoComplete="current-password" required /><button type="button" onClick={() => setShowPassword((current) => !current)} aria-label={showPassword ? "Hide password" : "Show password"}>{showPassword ? <EyeOff size={16} /> : <Eye size={16} />}</button></div></label>{error && <div className="login-error"><span><ShieldCheck size={14} /></span>{error}</div>}<button className="button primary login-submit" disabled={busy}>{busy ? "Authenticating…" : <>Open operations workspace <ArrowRight size={15} /></>}</button></form><div className="login-panel-foot"><span><Check size={13} /> Session protected by HttpOnly cookie</span><span>Need access? Contact your workspace administrator.</span><a className="login-admin-link" href="/admin/login">Product owner or manager? Open the Admin Console</a></div></div><div className="login-orbit orbit-login-a" /><div className="login-orbit orbit-login-b" /></section></main>;
}

function LoginFeature({ icon: Icon, title, detail }: { icon: typeof RadioTower; title: string; detail: string }) {
  return <div className="login-feature"><span className="login-feature-icon"><Icon size={15} /></span><span><strong>{title}</strong><small>{detail}</small></span></div>;
}
