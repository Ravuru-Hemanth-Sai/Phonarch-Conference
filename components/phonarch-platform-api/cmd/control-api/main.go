package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"
)

type node struct {
	NodeID              string  `json:"node_id"`
	ControlURL          string  `json:"control_url"`
	PrivateIP           string  `json:"private_ip"`
	MediaPublicIP       string  `json:"media_public_ip"`
	Role                string  `json:"role"`
	DrainState          string  `json:"drain_state"`
	Capacity            int     `json:"capacity"`
	ActiveCalls         int     `json:"active_calls"`
	ActiveRTP           int     `json:"active_rtp_sessions"`
	RTPPacketsPerSecond int64   `json:"rtp_packets_per_second"`
	ActiveSpeakers      int     `json:"active_speakers"`
	LoadScore           float64 `json:"load_score"`
	NodeEpoch           int64   `json:"node_epoch"`
}

type batchContact struct {
	Name  string `json:"name"`
	Phone string `json:"phone_number"`
}

type batchItem struct {
	ID    string
	Name  string
	Phone string
}
type api struct {
	db                 *sql.DB
	sipgoURLs          []string
	rdb                *redis.Client
	sessions           map[string]session
	defaultWorkspaceID string
	mu                 sync.RWMutex
	worker             sync.WaitGroup
}

type session struct {
	expiresAt   time.Time
	workspaceID string
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func httpBase(value string) string {
	if strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://") {
		return strings.TrimRight(value, "/")
	}
	return "http://" + strings.TrimRight(value, "/")
}

func httpTargets(value string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, raw := range strings.Split(value, ",") {
		target := strings.TrimSpace(raw)
		if target == "" {
			continue
		}
		target = httpBase(target)
		if _, exists := seen[target]; exists {
			continue
		}
		seen[target] = struct{}{}
		out = append(out, target)
	}
	return out
}

func cors(origin string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestOrigin := r.Header.Get("Origin")
		if requestOrigin != "" && origin != "" && requestOrigin == origin {
			w.Header().Set("Access-Control-Allow-Origin", requestOrigin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			w.Header().Set("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func newID() string { var b [16]byte; _, _ = rand.Read(b[:]); return hex.EncodeToString(b[:]) }
func (a *api) json(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
func (a *api) auth(r *http.Request) bool {
	c, err := r.Cookie("phonarch_session")
	if err != nil {
		return false
	}
	a.mu.RLock()
	s, ok := a.sessions[c.Value]
	a.mu.RUnlock()
	return ok && time.Now().Before(s.expiresAt)
}

func (a *api) workspaceID(r *http.Request) (string, bool) {
	c, err := r.Cookie("phonarch_session")
	if err != nil {
		return "", false
	}
	a.mu.RLock()
	s, ok := a.sessions[c.Value]
	a.mu.RUnlock()
	if !ok || time.Now().After(s.expiresAt) || s.workspaceID == "" {
		return "", false
	}
	// The workspace is selected by the authenticated session. A future
	// workspace switch must re-issue the session after checking membership;
	// accepting an arbitrary header here would break tenant isolation.
	return s.workspaceID, true
}

func (a *api) requireWorkspace(w http.ResponseWriter, r *http.Request) (string, bool) {
	id, ok := a.workspaceID(r)
	if !ok {
		a.json(w, http.StatusUnauthorized, map[string]string{"error": "workspace context unavailable"})
	}
	return id, ok
}
func (a *api) require(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.auth(r) {
			a.json(w, 401, map[string]string{"error": "authentication required"})
			return
		}
		next(w, r)
	}
}

func (a *api) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil || in.Username != env("ADMIN_USERNAME", "admin") || in.Password != env("ADMIN_PASSWORD", "change-me") {
		a.json(w, 401, map[string]string{"error": "invalid credentials"})
		return
	}
	workspaceID := a.defaultWorkspaceID
	if workspaceID == "" {
		if err := a.db.QueryRowContext(r.Context(), `SELECT id::text FROM workspaces WHERE slug=$1`, env("DEFAULT_WORKSPACE_SLUG", "operations")).Scan(&workspaceID); err != nil {
			a.json(w, 503, map[string]string{"error": "workspace is not initialized"})
			return
		}
	}
	id := newID()
	a.mu.Lock()
	a.sessions[id] = session{expiresAt: time.Now().Add(12 * time.Hour), workspaceID: workspaceID}
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "phonarch_session", Value: id, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 43200})
	a.json(w, 200, map[string]string{"status": "ok"})
}
func (a *api) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("phonarch_session"); err == nil {
		a.mu.Lock()
		delete(a.sessions, c.Value)
		a.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "phonarch_session", MaxAge: -1, Path: "/"})
	a.json(w, 200, map[string]string{"status": "ok"})
}

func (a *api) workspace(w http.ResponseWriter, r *http.Request) {
	id, ok := a.requireWorkspace(w, r)
	if !ok {
		return
	}
	var slug, name, status string
	if err := a.db.QueryRowContext(r.Context(), `SELECT slug,name,status FROM workspaces WHERE id=$1`, id).Scan(&slug, &name, &status); err != nil {
		a.json(w, 404, map[string]string{"error": "workspace not found"})
		return
	}
	a.json(w, 200, map[string]any{"id": id, "slug": slug, "name": name, "status": status})
}

func (a *api) nodes(ctx context.Context) ([]node, error) {
	targets := a.discoveredSipgoURLs(ctx)
	targets = append(targets, a.sipgoURLs...)
	unique := map[string]struct{}{}
	ordered := make([]string, 0, len(targets))
	for _, target := range targets {
		if target == "" {
			continue
		}
		if _, exists := unique[target]; exists {
			continue
		}
		unique[target] = struct{}{}
		ordered = append(ordered, target)
	}
	var lastErr error
	for _, target := range ordered {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(target, "/")+"/internal/v1/nodes", nil)
		if err != nil {
			lastErr = err
			continue
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if res.StatusCode >= 300 {
			lastErr = fmt.Errorf("SIPGo nodes returned %s", res.Status)
			res.Body.Close()
			continue
		}
		var nodes []node
		err = json.NewDecoder(res.Body).Decode(&nodes)
		res.Body.Close()
		if err == nil {
			return nodes, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("no SipGo HTTP targets configured")
	}
	return nil, lastErr
}

func (a *api) discoveredSipgoURLs(ctx context.Context) []string {
	if a.rdb == nil {
		return nil
	}
	iterator := a.rdb.Scan(ctx, 0, "active-sipgo:*", 0).Iterator()
	var targets []string
	for iterator.Next(ctx) {
		body, err := a.rdb.Get(ctx, iterator.Val()).Bytes()
		if err != nil {
			continue
		}
		var advertisement struct {
			HTTPURL string `json:"http_url"`
		}
		if json.Unmarshal(body, &advertisement) == nil && advertisement.HTTPURL != "" {
			targets = append(targets, httpBase(advertisement.HTTPURL))
		}
	}
	return targets
}
func chooseNode(nodes []node) (node, error) {
	var best node
	ok := false
	for _, n := range nodes {
		if n.ControlURL == "" || n.DrainState == "DRAINING" || n.DrainState == "DRAINED" || n.Capacity <= n.ActiveCalls {
			continue
		}
		if !ok || n.LoadScore < best.LoadScore {
			best = n
			ok = true
		}
	}
	if !ok {
		return node{}, fmt.Errorf("no available PBX sidecar")
	}
	return best, nil
}
func (a *api) sidecar(ctx context.Context, n node, path string, body any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(n.ControlURL, "/")+path, strings.NewReader(string(b)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if id, ok := body.(map[string]any)["command_id"].(string); ok {
		req.Header.Set("Idempotency-Key", id)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		return fmt.Errorf("sidecar returned %s", res.Status)
	}
	return nil
}

func (a *api) sidecarGet(ctx context.Context, n node, path string, response any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(n.ControlURL, "/")+path, nil)
	if err != nil {
		return err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		return fmt.Errorf("sidecar returned %s", res.Status)
	}
	return json.NewDecoder(res.Body).Decode(response)
}

func (a *api) bridges(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := a.requireWorkspace(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		rows, err := a.db.QueryContext(r.Context(), `SELECT id::text,name,status,created_at FROM bridges WHERE workspace_id=$1 ORDER BY created_at DESC`, workspaceID)
		if err != nil {
			a.json(w, 500, map[string]string{"error": err.Error()})
			return
		}
		defer rows.Close()
		out := []map[string]any{}
		for rows.Next() {
			var id, name, status string
			var created time.Time
			_ = rows.Scan(&id, &name, &status, &created)
			out = append(out, map[string]any{"id": id, "name": name, "status": status, "created_at": created})
		}
		a.json(w, 200, out)
	case http.MethodPost:
		var in struct {
			Name string `json:"name"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil || strings.TrimSpace(in.Name) == "" {
			a.json(w, 400, map[string]string{"error": "name required"})
			return
		}
		var id string
		err := a.db.QueryRowContext(r.Context(), `INSERT INTO bridges(workspace_id,name) VALUES($1,$2) RETURNING id::text`, workspaceID, in.Name).Scan(&id)
		if err != nil {
			a.json(w, 500, map[string]string{"error": err.Error()})
			return
		}
		a.json(w, 201, map[string]any{"id": id, "name": in.Name})
	default:
		w.WriteHeader(405)
	}
}

func (a *api) bridge(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := a.requireWorkspace(w, r)
	if !ok {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/bridges/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 1 || parts[0] == "" {
		w.WriteHeader(404)
		return
	}
	bridgeID := parts[0]
	if len(parts) == 1 && r.Method == http.MethodGet {
		a.bridgeState(w, r, workspaceID, bridgeID)
		return
	}
	if len(parts) == 1 && r.Method == http.MethodDelete {
		a.deleteBridge(w, r, workspaceID, bridgeID)
		return
	}
	if len(parts) == 2 && parts[1] == "dial" && r.Method == http.MethodPost {
		a.dial(w, r, workspaceID, bridgeID)
		return
	}
	if len(parts) == 2 && parts[1] == "batches" && r.Method == http.MethodPost {
		a.batch(w, r, workspaceID, bridgeID)
		return
	}
	if len(parts) == 2 && parts[1] == "speaker-requests" && r.Method == http.MethodGet {
		a.speakerRequests(w, r, workspaceID, bridgeID)
		return
	}
	w.WriteHeader(404)
}

func (a *api) deleteBridge(w http.ResponseWriter, r *http.Request, workspaceID, id string) {
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		a.json(w, 500, map[string]string{"error": err.Error()})
		return
	}
	rollback := func(err error) {
		_ = tx.Rollback()
		a.json(w, 500, map[string]string{"error": err.Error()})
	}
	var bridgeID string
	if err := tx.QueryRowContext(r.Context(), `SELECT id::text FROM bridges WHERE id=$1 AND workspace_id=$2 FOR UPDATE`, id, workspaceID).Scan(&bridgeID); err != nil {
		if err == sql.ErrNoRows {
			_ = tx.Rollback()
			a.json(w, 404, map[string]string{"error": "room not found"})
			return
		}
		rollback(err)
		return
	}
	var active int
	if err := tx.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM call_legs WHERE bridge_id=$1 AND state NOT IN ('ENDED','FAILED','DROPPED')`, id).Scan(&active); err != nil {
		rollback(err)
		return
	}
	if active > 0 {
		_ = tx.Rollback()
		a.json(w, http.StatusConflict, map[string]string{"error": "room has active participants; remove them before deleting the room"})
		return
	}
	// These references predate the bridge cascade and can otherwise prevent
	// PostgreSQL from deleting a participant while deleting the room.
	if _, err := tx.ExecContext(r.Context(), `DELETE FROM node_commands WHERE call_leg_id IN (SELECT id FROM call_legs WHERE bridge_id=$1)`, id); err != nil {
		rollback(err)
		return
	}
	if _, err := tx.ExecContext(r.Context(), `DELETE FROM dial_batch_items WHERE participant_id IN (SELECT id FROM participants WHERE bridge_id=$1)`, id); err != nil {
		rollback(err)
		return
	}
	if _, err := tx.ExecContext(r.Context(), `DELETE FROM bridges WHERE id=$1`, id); err != nil {
		rollback(err)
		return
	}
	if err := tx.Commit(); err != nil {
		a.json(w, 500, map[string]string{"error": err.Error()})
		return
	}
	a.json(w, 200, map[string]string{"status": "deleted", "bridge_id": id})
}

func (a *api) bridgeState(w http.ResponseWriter, r *http.Request, workspaceID, id string) {
	currentEpochs := map[string]int64{}
	if nodes, nodeErr := a.nodes(r.Context()); nodeErr == nil {
		for _, current := range nodes {
			currentEpochs[current.NodeID] = current.NodeEpoch
		}
	}
	rows, err := a.db.QueryContext(r.Context(), `SELECT p.id::text,p.display_name,p.phone_number,p.desired_state,COALESCE(c.id::text,''),COALESCE(c.state,''),COALESCE(c.node_id,''),COALESCE(c.node_epoch,0) FROM participants p LEFT JOIN LATERAL (SELECT * FROM call_legs WHERE participant_id=p.id AND workspace_id=$2 ORDER BY created_at DESC LIMIT 1)c ON true WHERE p.bridge_id=$1 AND p.workspace_id=$2 ORDER BY p.created_at`, id, workspaceID)
	if err != nil {
		a.json(w, 500, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var pid, name, phone, desired, callID, state, nodeID string
		var epoch int64
		_ = rows.Scan(&pid, &name, &phone, &desired, &callID, &state, &nodeID, &epoch)
		if callID != "" && state != "" && state != "ENDED" && state != "FAILED" && state != "DROPPED" {
			if current, ok := currentEpochs[nodeID]; ok && epoch != 0 && current != epoch {
				staleCallID := callID
				callID, state, nodeID = "", "", ""
				_, _ = a.db.ExecContext(r.Context(), `UPDATE call_legs SET state='ENDED',ended_at=COALESCE(ended_at,now()),updated_at=now(),last_error='sidecar epoch changed; call no longer active' WHERE id=$1 AND workspace_id=$2 AND state NOT IN ('ENDED','FAILED','DROPPED')`, staleCallID, workspaceID)
			}
		}
		out = append(out, map[string]any{"participant_id": pid, "name": name, "phone_number": phone, "desired_state": desired, "call_id": callID, "state": state, "node_id": nodeID})
	}
	requestRows, err := a.db.QueryContext(r.Context(), `SELECT id::text,participant_id::text,status,digit,requested_at,COALESCE(granted_by,'') FROM speaker_requests WHERE workspace_id=$1 AND bridge_id=$2 AND status IN ('QUEUED','GRANTED','ACTIVE') ORDER BY requested_at`, workspaceID, id)
	if err != nil {
		a.json(w, 500, map[string]string{"error": err.Error()})
		return
	}
	defer requestRows.Close()
	requests := []map[string]any{}
	for requestRows.Next() {
		var requestID, participantID, requestStatus, digit, grantedBy string
		var requestedAt time.Time
		if requestRows.Scan(&requestID, &participantID, &requestStatus, &digit, &requestedAt, &grantedBy) == nil {
			requests = append(requests, map[string]any{"id": requestID, "participant_id": participantID, "status": requestStatus, "digit": digit, "requested_at": requestedAt, "granted_by": grantedBy})
		}
	}
	a.json(w, 200, map[string]any{"workspace_id": workspaceID, "bridge_id": id, "participants": out, "speaker_requests": requests})
}

func (a *api) dispatchDial(ctx context.Context, workspaceID, bridgeID, name, phone string) (map[string]any, error) {
	nodes, err := a.nodes(ctx)
	if err != nil {
		return nil, err
	}
	n, err := chooseNode(nodes)
	if err != nil {
		return nil, err
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var pid, cid string
	if err = tx.QueryRowContext(ctx, `INSERT INTO participants(workspace_id,bridge_id,display_name,phone_number) VALUES($1,$2,$3,$4) RETURNING id::text`, workspaceID, bridgeID, coalesce(name, phone), phone).Scan(&pid); err != nil {
		return nil, err
	}
	if err = tx.QueryRowContext(ctx, `INSERT INTO call_legs(workspace_id,participant_id,bridge_id,node_id,node_epoch,sidecar_instance_id,state) VALUES($1,$2,$3,$4,$5,'','DISPATCHED') RETURNING id::text`, workspaceID, pid, bridgeID, n.NodeID, n.NodeEpoch).Scan(&cid); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	command := newID()
	body := map[string]any{"command_id": command, "call_id": cid, "bridge_id": bridgeID, "participant_id": pid, "destination": phone}
	if err := a.sidecar(ctx, n, "/v1/calls/originate", body); err != nil {
		_, _ = a.db.ExecContext(ctx, `UPDATE call_legs SET state='FAILED',last_error=$1,updated_at=now() WHERE id=$2`, err.Error(), cid)
		return nil, err
	}
	_, _ = a.db.ExecContext(ctx, `UPDATE call_legs SET state='DISPATCHED',updated_at=now() WHERE id=$1`, cid)
	return map[string]any{"participant_id": pid, "call_id": cid, "node_id": n.NodeID, "state": "DISPATCHED"}, nil
}

func (a *api) dial(w http.ResponseWriter, r *http.Request, workspaceID, bridgeID string) {
	var in struct {
		Name  string `json:"name"`
		Phone string `json:"phone_number"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil || in.Phone == "" {
		a.json(w, 400, map[string]string{"error": "phone_number required"})
		return
	}
	result, err := a.dispatchDial(r.Context(), workspaceID, bridgeID, in.Name, in.Phone)
	if err != nil {
		a.json(w, 502, map[string]string{"error": err.Error()})
		return
	}
	a.json(w, 202, result)
}
func coalesce(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func (a *api) action(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := a.requireWorkspace(w, r)
	if !ok {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/participants/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 2 {
		w.WriteHeader(404)
		return
	}
	pid, action := parts[0], parts[1]
	if action == "volume" && r.Method == http.MethodGet {
		a.volume(w, r, pid)
		return
	}
	if action == "add" {
		a.addParticipant(w, r, pid)
		return
	}
	if action != "mute" && action != "unmute" && action != "drop" {
		w.WriteHeader(404)
		return
	}
	var callID, nodeID string
	var epoch int64
	err := a.db.QueryRowContext(r.Context(), `SELECT id::text,node_id,node_epoch FROM call_legs WHERE participant_id=$1 AND workspace_id=$2 AND state NOT IN ('ENDED','FAILED','DROPPED') ORDER BY created_at DESC LIMIT 1`, pid, workspaceID).Scan(&callID, &nodeID, &epoch)
	if err != nil {
		a.json(w, 404, map[string]string{"error": "active call leg not found"})
		return
	}
	nodes, _ := a.nodes(r.Context())
	var n node
	for _, candidate := range nodes {
		if candidate.NodeID == nodeID {
			n = candidate
			break
		}
	}
	if n.NodeID == "" {
		a.json(w, 409, map[string]string{"error": "owning PBX is unavailable"})
		return
	}
	command := newID()
	body := map[string]any{"command_id": command, "call_id": callID, "participant_id": pid, "node_epoch": epoch}
	if err := a.sidecar(r.Context(), n, "/v1/participants/"+pid+"/"+action, body); err != nil {
		a.json(w, 502, map[string]string{"error": err.Error()})
		return
	}
	if action == "mute" {
		_, _ = a.db.ExecContext(r.Context(), `UPDATE call_legs SET desired_mute=true,updated_at=now() WHERE id=$1 AND workspace_id=$2`, callID, workspaceID)
		_, _ = a.db.ExecContext(r.Context(), `UPDATE participants SET desired_state='MUTED' WHERE id=$1 AND workspace_id=$2`, pid, workspaceID)
	}
	if action == "unmute" {
		_, _ = a.db.ExecContext(r.Context(), `UPDATE call_legs SET desired_mute=false,updated_at=now() WHERE id=$1 AND workspace_id=$2`, callID, workspaceID)
		_, _ = a.db.ExecContext(r.Context(), `UPDATE participants SET desired_state='ACTIVE' WHERE id=$1 AND workspace_id=$2`, pid, workspaceID)
	}
	if action == "drop" {
		_, _ = a.db.ExecContext(r.Context(), `UPDATE call_legs SET state='DROPPED',ended_at=now(),updated_at=now() WHERE id=$1 AND workspace_id=$2`, callID, workspaceID)
		_, _ = a.db.ExecContext(r.Context(), `UPDATE participants SET desired_state='DROPPED' WHERE id=$1 AND workspace_id=$2`, pid, workspaceID)
	}
	a.json(w, 200, map[string]any{"workspace_id": workspaceID, "call_id": callID, "participant_id": pid, "action": action, "node_id": nodeID})
}

func (a *api) volume(w http.ResponseWriter, r *http.Request, participantID string) {
	workspaceID, ok := a.requireWorkspace(w, r)
	if !ok {
		return
	}
	var callID, nodeID string
	if err := a.db.QueryRowContext(r.Context(), `SELECT id::text,node_id FROM call_legs WHERE participant_id=$1 AND workspace_id=$2 AND state NOT IN ('ENDED','FAILED','DROPPED') ORDER BY created_at DESC LIMIT 1`, participantID, workspaceID).Scan(&callID, &nodeID); err != nil {
		a.json(w, 404, map[string]string{"error": "active call leg not found"})
		return
	}
	nodes, err := a.nodes(r.Context())
	if err != nil {
		a.json(w, 503, map[string]string{"error": err.Error()})
		return
	}
	var owner node
	for _, candidate := range nodes {
		if candidate.NodeID == nodeID {
			owner = candidate
			break
		}
	}
	if owner.NodeID == "" {
		a.json(w, 409, map[string]string{"error": "owning PBX is unavailable"})
		return
	}
	var result struct {
		VolumeDB float64 `json:"volume_db"`
		State    string  `json:"state"`
	}
	if err := a.sidecarGet(r.Context(), owner, "/v1/calls/"+callID, &result); err != nil {
		a.json(w, 502, map[string]string{"error": err.Error()})
		return
	}
	a.json(w, 200, map[string]any{"participant_id": participantID, "call_id": callID, "volume_db": result.VolumeDB, "state": result.State})
}

func (a *api) addParticipant(w http.ResponseWriter, r *http.Request, participantID string) {
	workspaceID, ok := a.requireWorkspace(w, r)
	if !ok {
		return
	}
	var bridgeID, name, phone string
	if err := a.db.QueryRowContext(r.Context(), `SELECT bridge_id::text,display_name,phone_number FROM participants WHERE id=$1 AND workspace_id=$2`, participantID, workspaceID).Scan(&bridgeID, &name, &phone); err != nil {
		a.json(w, 404, map[string]string{"error": "participant not found"})
		return
	}
	nodes, err := a.nodes(r.Context())
	if err != nil {
		a.json(w, 503, map[string]string{"error": err.Error()})
		return
	}
	n, err := chooseNode(nodes)
	if err != nil {
		a.json(w, 503, map[string]string{"error": err.Error()})
		return
	}
	var callID string
	if err := a.db.QueryRowContext(r.Context(), `INSERT INTO call_legs(workspace_id,participant_id,bridge_id,node_id,node_epoch,state) VALUES($1,$2,$3,$4,$5,'DISPATCHED') RETURNING id::text`, workspaceID, participantID, bridgeID, n.NodeID, n.NodeEpoch).Scan(&callID); err != nil {
		a.json(w, 500, map[string]string{"error": err.Error()})
		return
	}
	command := newID()
	body := map[string]any{"command_id": command, "call_id": callID, "bridge_id": bridgeID, "participant_id": participantID, "destination": phone}
	if err := a.sidecar(r.Context(), n, "/v1/calls/originate", body); err != nil {
		_, _ = a.db.ExecContext(r.Context(), `UPDATE call_legs SET state='FAILED',last_error=$1,updated_at=now() WHERE id=$2`, err.Error(), callID)
		a.json(w, 502, map[string]string{"error": err.Error()})
		return
	}
	_, _ = a.db.ExecContext(r.Context(), `UPDATE participants SET desired_state='ACTIVE' WHERE id=$1 AND workspace_id=$2`, participantID, workspaceID)
	a.json(w, 202, map[string]any{"workspace_id": workspaceID, "participant_id": participantID, "call_id": callID, "node_id": n.NodeID, "state": "DISPATCHED"})
}

func (a *api) batch(w http.ResponseWriter, r *http.Request, workspaceID, bridgeID string) {
	var in struct {
		Contacts    []batchContact `json:"contacts"`
		Concurrency int            `json:"concurrency"`
		Rate        int            `json:"rate_per_second"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil || len(in.Contacts) == 0 {
		a.json(w, 400, map[string]string{"error": "contacts required"})
		return
	}
	if in.Concurrency <= 0 {
		in.Concurrency = 16
	}
	if in.Rate <= 0 {
		in.Rate = 8
	}
	var batchID string
	if err := a.db.QueryRowContext(r.Context(), `INSERT INTO dial_batches(workspace_id,bridge_id,concurrency,rate_per_second) VALUES($1,$2,$3,$4) RETURNING id::text`, workspaceID, bridgeID, in.Concurrency, in.Rate).Scan(&batchID); err != nil {
		a.json(w, 500, map[string]string{"error": err.Error()})
		return
	}
	items := make([]batchItem, 0, len(in.Contacts))
	for _, contact := range in.Contacts {
		var itemID string
		if err := a.db.QueryRowContext(r.Context(), `INSERT INTO dial_batch_items(workspace_id,batch_id,name,phone_number) VALUES($1,$2,$3,$4) RETURNING id::text`, workspaceID, batchID, coalesce(contact.Name, contact.Phone), contact.Phone).Scan(&itemID); err == nil {
			items = append(items, batchItem{ID: itemID, Name: coalesce(contact.Name, contact.Phone), Phone: contact.Phone})
		}
	}
	a.worker.Add(1)
	go a.runBatch(batchID, workspaceID, bridgeID, items, in.Concurrency, in.Rate)
	a.json(w, 202, map[string]any{"batch_id": batchID, "status": "QUEUED", "count": len(in.Contacts)})
}

func (a *api) runBatch(batchID, workspaceID, bridgeID string, items []batchItem, concurrency, rate int) {
	defer a.worker.Done()
	_, _ = a.db.Exec(`UPDATE dial_batches SET status='RUNNING',updated_at=now() WHERE id=$1`, batchID)
	interval := time.Second / time.Duration(rate)
	if interval <= 0 {
		interval = time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, item := range items {
		<-ticker.C
		sem <- struct{}{}
		wg.Add(1)
		go func(item batchItem) {
			defer wg.Done()
			defer func() { <-sem }()
			result, err := a.dispatchDial(context.Background(), workspaceID, bridgeID, item.Name, item.Phone)
			if err != nil {
				_, _ = a.db.Exec(`UPDATE dial_batch_items SET status='FAILED',last_error=$1,updated_at=now() WHERE id=$2`, err.Error(), item.ID)
				return
			}
			_, _ = a.db.Exec(`UPDATE dial_batch_items SET status='DISPATCHED',participant_id=$1,updated_at=now() WHERE id=$2`, result["participant_id"], item.ID)
		}(item)
	}
	wg.Wait()
	_, _ = a.db.Exec(`UPDATE dial_batches SET status='DISPATCHED',updated_at=now() WHERE id=$1`, batchID)
}
func (a *api) batchState(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := a.requireWorkspace(w, r)
	if !ok {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/v1/batches/")
	var status string
	var total, done int
	if err := a.db.QueryRowContext(r.Context(), `SELECT b.status,COUNT(i.id),COUNT(i.id) FILTER(WHERE i.status IN ('ANSWERED','IN_BRIDGE','FAILED','ENDED','DROPPED')) FROM dial_batches b LEFT JOIN dial_batch_items i ON i.batch_id=b.id WHERE b.id=$1 AND b.workspace_id=$2 GROUP BY b.status`, id, workspaceID).Scan(&status, &total, &done); err != nil {
		a.json(w, 404, map[string]string{"error": "batch not found"})
		return
	}
	if total > 0 && done >= total && status != "COMPLETED" {
		status = "COMPLETED"
		_, _ = a.db.ExecContext(r.Context(), `UPDATE dial_batches SET status='COMPLETED',updated_at=now() WHERE id=$1`, id)
	}
	a.json(w, 200, map[string]any{"batch_id": id, "status": status, "total": total, "completed": done})
}

func (a *api) internalAllowed(r *http.Request) bool {
	expected := os.Getenv("PHONARCH_INTERNAL_TOKEN")
	if expected != "" {
		return r.Header.Get("X-PhonArch-Internal-Token") == expected
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	return err == nil && (host == "127.0.0.1" || host == "::1")
}

func (a *api) dtmf(w http.ResponseWriter, r *http.Request) {
	if !a.internalAllowed(r) {
		a.json(w, http.StatusForbidden, map[string]string{"error": "internal event authorization failed"})
		return
	}
	var in struct {
		CallID        string `json:"call_id"`
		FromTag       string `json:"from_tag"`
		ToTag         string `json:"to_tag"`
		Digit         string `json:"digit"`
		WorkspaceID   string `json:"workspace_id"`
		RoomID        string `json:"room_id"`
		ParticipantID string `json:"participant_id"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil || (in.Digit != "0" && in.Digit != "#") {
		a.json(w, 400, map[string]string{"error": "digit must be 0 or #"})
		return
	}
	var workspaceID, bridgeID, participantID, callLegID string
	query := `SELECT workspace_id::text,bridge_id::text,participant_id::text,id::text FROM call_legs WHERE (id::text=$1 OR pbx_call_id=$1 OR sip_call_id=$1) AND state NOT IN ('ENDED','FAILED','DROPPED') ORDER BY created_at DESC LIMIT 1`
	lookup := in.CallID
	if in.ParticipantID != "" {
		query = `SELECT workspace_id::text,bridge_id::text,participant_id::text,id::text FROM call_legs WHERE participant_id=$1 AND state NOT IN ('ENDED','FAILED','DROPPED') ORDER BY created_at DESC LIMIT 1`
		lookup = in.ParticipantID
	}
	if err := a.db.QueryRowContext(r.Context(), query, lookup).Scan(&workspaceID, &bridgeID, &participantID, &callLegID); err != nil {
		a.json(w, 404, map[string]string{"error": "active call leg not found"})
		return
	}
	if in.WorkspaceID != "" && in.WorkspaceID != workspaceID {
		a.json(w, http.StatusForbidden, map[string]string{"error": "workspace mismatch"})
		return
	}
	if in.RoomID != "" && in.RoomID != bridgeID {
		a.json(w, http.StatusForbidden, map[string]string{"error": "room mismatch"})
		return
	}
	if in.Digit == "#" {
		_, _ = a.db.ExecContext(r.Context(), `UPDATE speaker_requests SET status='WITHDRAWN',withdrawn_at=now() WHERE workspace_id=$1 AND bridge_id=$2 AND participant_id=$3 AND status IN ('QUEUED','GRANTED','ACTIVE')`, workspaceID, bridgeID, participantID)
		_, _ = a.db.ExecContext(r.Context(), `UPDATE participants SET desired_state='MUTED' WHERE id=$1 AND workspace_id=$2 AND desired_state='HAND_RAISED'`, participantID, workspaceID)
		a.roomEvent(r.Context(), workspaceID, bridgeID, participantID, "SPEAKER_REQUEST_WITHDRAWN", map[string]any{"digit": in.Digit})
		a.json(w, 202, map[string]any{"status": "WITHDRAWN", "workspace_id": workspaceID, "room_id": bridgeID, "participant_id": participantID})
		return
	}
	_, err := a.db.ExecContext(r.Context(), `INSERT INTO speaker_requests(workspace_id,bridge_id,participant_id,call_leg_id,digit) SELECT $1,$2,$3,$4,$5 WHERE NOT EXISTS (SELECT 1 FROM speaker_requests WHERE bridge_id=$2 AND participant_id=$3 AND status IN ('QUEUED','GRANTED','ACTIVE'))`, workspaceID, bridgeID, participantID, callLegID, in.Digit)
	if err != nil {
		a.json(w, 500, map[string]string{"error": err.Error()})
		return
	}
	_, _ = a.db.ExecContext(r.Context(), `UPDATE participants SET desired_state='HAND_RAISED' WHERE id=$1 AND workspace_id=$2 AND desired_state NOT IN ('DROPPED','FAILED')`, participantID, workspaceID)
	a.roomEvent(r.Context(), workspaceID, bridgeID, participantID, "SPEAKER_REQUESTED", map[string]any{"digit": in.Digit})
	a.json(w, 202, map[string]any{"status": "QUEUED", "workspace_id": workspaceID, "room_id": bridgeID, "participant_id": participantID})
}

func (a *api) roomEvent(ctx context.Context, workspaceID, bridgeID, participantID, eventType string, payload map[string]any) {
	b, _ := json.Marshal(payload)
	_, _ = a.db.ExecContext(ctx, `INSERT INTO room_events(workspace_id,bridge_id,participant_id,event_type,payload) VALUES($1,$2,$3,$4,$5)`, workspaceID, bridgeID, participantID, eventType, b)
}

func (a *api) speakerRequests(w http.ResponseWriter, r *http.Request, workspaceID, bridgeID string) {
	rows, err := a.db.QueryContext(r.Context(), `SELECT id::text,participant_id::text,status,digit,requested_at,COALESCE(granted_by,'') FROM speaker_requests WHERE workspace_id=$1 AND bridge_id=$2 AND status IN ('QUEUED','GRANTED','ACTIVE') ORDER BY requested_at`, workspaceID, bridgeID)
	if err != nil {
		a.json(w, 500, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, participantID, status, digit, grantedBy string
		var requestedAt time.Time
		if rows.Scan(&id, &participantID, &status, &digit, &requestedAt, &grantedBy) == nil {
			out = append(out, map[string]any{"id": id, "participant_id": participantID, "status": status, "digit": digit, "requested_at": requestedAt, "granted_by": grantedBy})
		}
	}
	a.json(w, 200, map[string]any{"workspace_id": workspaceID, "room_id": bridgeID, "requests": out})
}

func (a *api) speakerRequestAction(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := a.requireWorkspace(w, r)
	if !ok {
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/speaker-requests/"), "/"), "/")
	if len(parts) != 2 || (parts[1] != "grant" && parts[1] != "withdraw") {
		w.WriteHeader(404)
		return
	}
	requestID, action := parts[0], parts[1]
	var bridgeID, participantID, callID, nodeID string
	var nodeEpoch int64
	if err := a.db.QueryRowContext(r.Context(), `SELECT bridge_id::text,participant_id::text,COALESCE(call_leg_id::text,''),status FROM speaker_requests WHERE id=$1 AND workspace_id=$2`, requestID, workspaceID).Scan(&bridgeID, &participantID, &callID, new(string)); err != nil {
		a.json(w, 404, map[string]string{"error": "speaker request not found"})
		return
	}
	if action == "withdraw" {
		_, _ = a.db.ExecContext(r.Context(), `UPDATE speaker_requests SET status='WITHDRAWN',withdrawn_at=now() WHERE id=$1 AND workspace_id=$2 AND status IN ('QUEUED','GRANTED','ACTIVE')`, requestID, workspaceID)
		_, _ = a.db.ExecContext(r.Context(), `UPDATE participants SET desired_state='MUTED' WHERE id=$1 AND workspace_id=$2`, participantID, workspaceID)
		a.roomEvent(r.Context(), workspaceID, bridgeID, participantID, "SPEAKER_REQUEST_WITHDRAWN", map[string]any{"request_id": requestID, "actor": "admin"})
		a.json(w, 200, map[string]any{"status": "WITHDRAWN", "request_id": requestID})
		return
	}
	var active int
	_ = a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM speaker_requests WHERE workspace_id=$1 AND bridge_id=$2 AND status IN ('GRANTED','ACTIVE')`, workspaceID, bridgeID).Scan(&active)
	maxSpeakers := 16
	if v, err := strconv.Atoi(env("ROOM_MAX_ACTIVE_SPEAKERS", "16")); err == nil && v > 0 {
		maxSpeakers = v
	}
	if active >= maxSpeakers {
		a.json(w, http.StatusConflict, map[string]string{"error": "room active-speaker limit reached"})
		return
	}
	if callID == "" {
		_ = a.db.QueryRowContext(r.Context(), `SELECT id::text FROM call_legs WHERE participant_id=$1 AND workspace_id=$2 AND state NOT IN ('ENDED','FAILED','DROPPED') ORDER BY created_at DESC LIMIT 1`, participantID, workspaceID).Scan(&callID)
	}
	if err := a.db.QueryRowContext(r.Context(), `SELECT node_id,node_epoch FROM call_legs WHERE id=$1 AND workspace_id=$2`, callID, workspaceID).Scan(&nodeID, &nodeEpoch); err != nil {
		a.json(w, 409, map[string]string{"error": "participant call owner unavailable"})
		return
	}
	nodes, err := a.nodes(r.Context())
	if err != nil {
		a.json(w, 503, map[string]string{"error": err.Error()})
		return
	}
	var owner node
	for _, candidate := range nodes {
		if candidate.NodeID == nodeID {
			owner = candidate
			break
		}
	}
	if owner.NodeID == "" {
		a.json(w, 409, map[string]string{"error": "owning PBX is unavailable"})
		return
	}
	command := newID()
	body := map[string]any{"command_id": command, "call_id": callID, "participant_id": participantID, "node_epoch": nodeEpoch}
	if err := a.sidecar(r.Context(), owner, "/v1/participants/"+participantID+"/unmute", body); err != nil {
		a.json(w, 502, map[string]string{"error": err.Error()})
		return
	}
	_, _ = a.db.ExecContext(r.Context(), `UPDATE speaker_requests SET status='ACTIVE',granted_at=now(),granted_by='admin' WHERE id=$1 AND workspace_id=$2`, requestID, workspaceID)
	_, _ = a.db.ExecContext(r.Context(), `UPDATE participants SET desired_state='ACTIVE' WHERE id=$1 AND workspace_id=$2`, participantID, workspaceID)
	_, _ = a.db.ExecContext(r.Context(), `UPDATE call_legs SET desired_mute=false,updated_at=now() WHERE id=$1 AND workspace_id=$2`, callID, workspaceID)
	a.roomEvent(r.Context(), workspaceID, bridgeID, participantID, "SPEAKER_GRANTED", map[string]any{"request_id": requestID, "command_id": command})
	a.json(w, 200, map[string]any{"status": "ACTIVE", "request_id": requestID, "participant_id": participantID, "node_id": nodeID})
}

func (a *api) events(w http.ResponseWriter, r *http.Request) {
	if !a.internalAllowed(r) {
		a.json(w, http.StatusForbidden, map[string]string{"error": "internal event authorization failed"})
		return
	}
	var e struct {
		Type string `json:"type"`
		Call struct {
			CallID      string `json:"call_id"`
			State       string `json:"state"`
			PBXMemberID string `json:"pbx_member_id"`
		} `json:"call"`
		CallID string `json:"call_id"`
		State  string `json:"state"`
	}
	if json.NewDecoder(r.Body).Decode(&e) != nil {
		w.WriteHeader(400)
		return
	}
	id := e.CallID
	if id == "" {
		id = e.Call.CallID
	}
	state := e.State
	if state == "" {
		state = e.Call.State
	}
	if id != "" && state != "" {
		_, _ = a.db.ExecContext(r.Context(), `UPDATE call_legs SET state=$1,pbx_member_id=COALESCE(NULLIF($2,''),pbx_member_id),updated_at=now(),answered_at=CASE WHEN $1='ANSWERED' THEN now() ELSE answered_at END,ended_at=CASE WHEN $1 IN ('ENDED','FAILED','DROPPED') THEN now() ELSE ended_at END WHERE id=$3`, state, e.Call.PBXMemberID, id)
		_, _ = a.db.ExecContext(r.Context(), `UPDATE dial_batch_items SET status=$1,updated_at=now() WHERE participant_id=(SELECT participant_id FROM call_legs WHERE id=$2)`, state, id)
	}
	a.json(w, 202, map[string]string{"status": "accepted"})
}

func main() {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		slog.Error("DATABASE_URL is required")
		os.Exit(1)
	}
	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		panic(err)
	}
	if err = db.Ping(); err != nil {
		slog.Error("postgres unavailable", "error", err)
		os.Exit(1)
	}
	workspaceID := strings.TrimSpace(env("DEFAULT_WORKSPACE_ID", ""))
	if workspaceID == "" {
		_ = db.QueryRow(`SELECT id::text FROM workspaces WHERE slug=$1`, env("DEFAULT_WORKSPACE_SLUG", "operations")).Scan(&workspaceID)
	}
	rdb := redis.NewClient(&redis.Options{Addr: env("REDIS_ADDR", "127.0.0.1:6379")})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		slog.Warn("Redis unavailable; control API will use configured SipGo HTTP targets", "error", err)
	}
	a := &api{db: db, sipgoURLs: httpTargets(env("SIPGO_HTTP_TARGETS", env("SIPGO_HTTP", "http://127.0.0.1:8080"))), rdb: rdb, sessions: map[string]session{}, defaultWorkspaceID: workspaceID}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if err := a.db.Ping(); err != nil {
			http.Error(w, err.Error(), 503)
			return
		}
		w.WriteHeader(200)
	})
	mux.HandleFunc("/api/v1/auth/login", a.login)
	mux.HandleFunc("/api/v1/auth/logout", a.require(a.logout))
	mux.HandleFunc("/api/v1/workspace", a.require(a.workspace))
	mux.HandleFunc("/api/v1/bridges", a.require(a.bridges))
	mux.HandleFunc("/api/v1/bridges/", a.require(a.bridge))
	mux.HandleFunc("/api/v1/participants/", a.require(a.action))
	mux.HandleFunc("/api/v1/batches/", a.require(a.batchState))
	mux.HandleFunc("/api/v1/speaker-requests/", a.require(a.speakerRequestAction))
	mux.HandleFunc("/internal/v1/events", a.events)
	mux.HandleFunc("/internal/v1/dtmf", a.dtmf)
	addr := env("CONTROL_API_LISTEN", "127.0.0.1:8081")
	uiOrigin := strings.TrimRight(env("UI_ORIGIN", ""), "/")
	slog.Info("control API listening", "addr", addr)
	if err := http.ListenAndServe(addr, cors(uiOrigin, mux)); err != nil {
		slog.Error("control API", "error", err)
	}
}
