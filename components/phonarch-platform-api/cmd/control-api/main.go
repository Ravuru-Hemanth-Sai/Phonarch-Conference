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
	"golang.org/x/crypto/bcrypt"
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
	Role  string `json:"role"`
}

type batchItem struct {
	ID    string
	Name  string
	Phone string
}
type api struct {
	db            *sql.DB
	sipgoURLs     []string
	rdb           *redis.Client
	sessions      map[string]session
	adminSessions map[string]session
	mu            sync.RWMutex
	worker        sync.WaitGroup
}

type session struct {
	expiresAt     time.Time
	workspaceID   string
	operatorID    string
	username      string
	displayName   string
	workspaceRole string
	platformAdmin bool
}

var supportedDialRegions = map[string]struct{}{
	"+1": {}, "+44": {}, "+61": {}, "+65": {}, "+91": {}, "+971": {},
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
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
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
func (a *api) currentSession(r *http.Request) (session, bool) {
	c, err := r.Cookie("phonarch_session")
	if err != nil {
		return session{}, false
	}
	a.mu.RLock()
	s, ok := a.sessions[c.Value]
	a.mu.RUnlock()
	return s, ok && time.Now().Before(s.expiresAt)
}
func (a *api) currentAdminSession(r *http.Request) (session, bool) {
	c, err := r.Cookie("phonarch_admin_session")
	if err != nil {
		return session{}, false
	}
	a.mu.RLock()
	s, ok := a.adminSessions[c.Value]
	a.mu.RUnlock()
	return s, ok && time.Now().Before(s.expiresAt)
}
func (a *api) auth(r *http.Request) bool {
	if _, ok := a.currentSession(r); ok {
		return true
	}
	_, ok := a.currentAdminSession(r)
	return ok
}

func (a *api) workspaceID(r *http.Request) (string, bool) {
	if s, ok := a.currentSession(r); ok && s.workspaceID != "" {
		// Customer sessions select exactly one workspace at login. The workspace
		// is never accepted from a request parameter for customer sessions.
		return s.workspaceID, true
	}
	if _, ok := a.currentAdminSession(r); ok {
		workspaceID := strings.TrimSpace(r.URL.Query().Get("workspace_id"))
		if workspaceID == "" {
			return "", false
		}
		var status string
		if err := a.db.QueryRowContext(r.Context(), `SELECT status FROM workspaces WHERE id=$1`, workspaceID).Scan(&status); err != nil || status != "ACTIVE" {
			return "", false
		}
		return workspaceID, true
	}
	return "", false
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

func (a *api) requirePlatformAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := a.currentAdminSession(r); !ok {
			a.json(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
			return
		}
		next(w, r)
	}
}

func (a *api) requireWorkspaceRole(w http.ResponseWriter, r *http.Request, allowed ...string) (string, string, bool) {
	if _, admin := a.currentAdminSession(r); admin {
		workspaceID, ok := a.workspaceID(r)
		if !ok {
			a.json(w, http.StatusUnauthorized, map[string]string{"error": "workspace context unavailable"})
			return "", "", false
		}
		// Product administrators are authorized across every selected active
		// workspace. The product-admin cookie is never usable without the
		// explicit workspace_id scope above.
		return workspaceID, "OWNER", true
	}
	s, ok := a.currentSession(r)
	if !ok || s.workspaceID == "" {
		a.json(w, http.StatusUnauthorized, map[string]string{"error": "workspace context unavailable"})
		return "", "", false
	}
	var role string
	err := a.db.QueryRowContext(r.Context(), `SELECT wm.role FROM workspace_members wm JOIN workspaces ws ON ws.id=wm.workspace_id WHERE wm.workspace_id=$1 AND wm.operator_id=$2 AND ws.status='ACTIVE' AND wm.role IN ('OWNER','ADMIN','OPERATOR','VIEWER')`, s.workspaceID, s.operatorID).Scan(&role)
	if err != nil {
		a.json(w, http.StatusForbidden, map[string]string{"error": "workspace access is no longer active"})
		return "", "", false
	}
	for _, candidate := range allowed {
		if role == candidate {
			return s.workspaceID, role, true
		}
	}
	a.json(w, http.StatusForbidden, map[string]string{"error": "workspace role does not allow this action"})
	return "", role, false
}

func (a *api) workspaceRole(r *http.Request) (string, bool) {
	if _, admin := a.currentAdminSession(r); admin {
		if _, ok := a.workspaceID(r); ok {
			return "OWNER", true
		}
		return "", false
	}
	s, ok := a.currentSession(r)
	if !ok || s.workspaceID == "" {
		return "", false
	}
	var role string
	if err := a.db.QueryRowContext(r.Context(), `SELECT wm.role FROM workspace_members wm JOIN workspaces ws ON ws.id=wm.workspace_id WHERE wm.workspace_id=$1 AND wm.operator_id=$2 AND ws.status='ACTIVE'`, s.workspaceID, s.operatorID).Scan(&role); err != nil {
		return "", false
	}
	return role, true
}

func (a *api) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil || strings.TrimSpace(in.Username) == "" || in.Password == "" {
		a.json(w, 401, map[string]string{"error": "invalid credentials"})
		return
	}
	in.Username = strings.TrimSpace(in.Username)
	var operatorID, displayName, passwordHash, platformRole, status string
	if err := a.db.QueryRowContext(r.Context(), `SELECT id::text,password_hash,COALESCE(display_name,''),COALESCE(platform_role,'WORKSPACE_OPERATOR'),COALESCE(status,'ACTIVE') FROM operators WHERE username=$1`, in.Username).Scan(&operatorID, &passwordHash, &displayName, &platformRole, &status); err != nil || status != "ACTIVE" || bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(in.Password)) != nil {
		a.json(w, http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
		return
	}
	if platformRole == "PLATFORM_OWNER" || platformRole == "PLATFORM_ADMIN" {
		a.json(w, http.StatusForbidden, map[string]string{"error": "product administrator accounts must use the product admin login"})
		return
	}
	var workspaceID, workspaceRole string
	if err := a.db.QueryRowContext(r.Context(), `SELECT wm.workspace_id::text,wm.role FROM workspace_members wm JOIN workspaces w ON w.id=wm.workspace_id WHERE wm.operator_id=$1 AND w.status='ACTIVE' ORDER BY CASE wm.role WHEN 'OWNER' THEN 0 WHEN 'ADMIN' THEN 1 WHEN 'OPERATOR' THEN 2 ELSE 3 END LIMIT 1`, operatorID).Scan(&workspaceID, &workspaceRole); err != nil {
		a.json(w, http.StatusForbidden, map[string]string{"error": "user has no active workspace access"})
		return
	}
	id := newID()
	a.mu.Lock()
	a.sessions[id] = session{expiresAt: time.Now().Add(12 * time.Hour), workspaceID: workspaceID, operatorID: operatorID, username: in.Username, displayName: coalesce(displayName, in.Username), workspaceRole: workspaceRole}
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "phonarch_session", Value: id, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 43200})
	a.json(w, 200, map[string]string{"status": "ok"})
}

func (a *api) adminLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil || strings.TrimSpace(in.Username) == "" || in.Password == "" {
		a.json(w, http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
		return
	}
	var operatorID, displayName, passwordHash, role, status string
	err := a.db.QueryRowContext(r.Context(), `SELECT id::text,COALESCE(display_name,''),password_hash,COALESCE(platform_role,'WORKSPACE_OPERATOR'),COALESCE(status,'ACTIVE') FROM operators WHERE username=$1`, strings.TrimSpace(in.Username)).Scan(&operatorID, &displayName, &passwordHash, &role, &status)
	if err != nil || status != "ACTIVE" || (role != "PLATFORM_OWNER" && role != "PLATFORM_ADMIN") || bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(in.Password)) != nil {
		a.json(w, http.StatusUnauthorized, map[string]string{"error": "invalid product administrator credentials"})
		return
	}
	id := newID()
	a.mu.Lock()
	a.adminSessions[id] = session{expiresAt: time.Now().Add(12 * time.Hour), operatorID: operatorID, username: strings.TrimSpace(in.Username), displayName: coalesce(displayName, in.Username), platformAdmin: true}
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "phonarch_admin_session", Value: id, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 43200})
	a.json(w, 200, map[string]string{"status": "ok"})
}

func (a *api) me(w http.ResponseWriter, r *http.Request) {
	s, ok := a.currentSession(r)
	if !ok {
		a.json(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}
	var workspace map[string]any = map[string]any{}
	if s.workspaceID != "" {
		var slug, name, status string
		if a.db.QueryRowContext(r.Context(), `SELECT slug,name,status FROM workspaces WHERE id=$1`, s.workspaceID).Scan(&slug, &name, &status) == nil {
			workspace = map[string]any{"id": s.workspaceID, "slug": slug, "name": name, "status": status}
		}
	}
	role, _ := a.workspaceRole(r)
	a.json(w, http.StatusOK, map[string]any{"username": s.username, "display_name": s.displayName, "operator_id": s.operatorID, "workspace_role": role, "can_manage_users": role == "OWNER" || role == "ADMIN", "workspace": workspace})
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

func (a *api) adminMe(w http.ResponseWriter, r *http.Request) {
	s, ok := a.currentAdminSession(r)
	if !ok {
		a.json(w, http.StatusUnauthorized, map[string]string{"error": "product administrator authentication required"})
		return
	}
	a.json(w, http.StatusOK, map[string]any{"username": s.username, "display_name": s.displayName, "operator_id": s.operatorID, "platform_admin": true})
}

func (a *api) adminLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("phonarch_admin_session"); err == nil {
		a.mu.Lock()
		delete(a.adminSessions, c.Value)
		a.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "phonarch_admin_session", MaxAge: -1, Path: "/"})
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

func (a *api) workspaceMembers(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := a.requireWorkspace(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodGet {
		rows, err := a.db.QueryContext(r.Context(), `SELECT o.id::text,o.username,COALESCE(o.display_name,''),o.status,wm.role FROM workspace_members wm JOIN operators o ON o.id=wm.operator_id WHERE wm.workspace_id=$1 ORDER BY CASE wm.role WHEN 'OWNER' THEN 0 WHEN 'ADMIN' THEN 1 WHEN 'OPERATOR' THEN 2 ELSE 3 END,o.username`, workspaceID)
		if err != nil {
			a.json(w, 500, map[string]string{"error": err.Error()})
			return
		}
		defer rows.Close()
		members := []map[string]any{}
		for rows.Next() {
			var id, username, displayName, status, role string
			if rows.Scan(&id, &username, &displayName, &status, &role) == nil {
				members = append(members, map[string]any{"operator_id": id, "username": username, "display_name": displayName, "status": status, "role": role})
			}
		}
		a.json(w, 200, members)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if _, _, ok := a.requireWorkspaceRole(w, r, "OWNER", "ADMIN"); !ok {
		return
	}
	var in struct {
		Username    string `json:"username"`
		DisplayName string `json:"display_name"`
		Password    string `json:"password"`
		Role        string `json:"role"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil || strings.TrimSpace(in.Username) == "" || len(in.Password) < 8 {
		a.json(w, 400, map[string]string{"error": "username and a password of at least 8 characters are required"})
		return
	}
	if in.Role != "ADMIN" && in.Role != "VIEWER" {
		in.Role = "OPERATOR"
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil {
		a.json(w, 500, map[string]string{"error": err.Error()})
		return
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		a.json(w, 500, map[string]string{"error": err.Error()})
		return
	}
	defer tx.Rollback()
	var operatorID string
	if err := tx.QueryRowContext(r.Context(), `INSERT INTO operators(username,password_hash,display_name,platform_role,status) VALUES($1,$2,$3,'WORKSPACE_OPERATOR','ACTIVE') RETURNING id::text`, strings.TrimSpace(in.Username), string(hash), strings.TrimSpace(in.DisplayName)).Scan(&operatorID); err != nil {
		a.json(w, http.StatusConflict, map[string]string{"error": "username already exists"})
		return
	}
	if _, err := tx.ExecContext(r.Context(), `INSERT INTO workspace_members(workspace_id,operator_id,role) VALUES($1,$2,$3)`, workspaceID, operatorID, in.Role); err != nil {
		a.json(w, http.StatusConflict, map[string]string{"error": "user could not be added to this workspace"})
		return
	}
	if err := tx.Commit(); err != nil {
		a.json(w, 500, map[string]string{"error": err.Error()})
		return
	}
	a.json(w, http.StatusCreated, map[string]any{"operator_id": operatorID, "username": strings.TrimSpace(in.Username), "display_name": strings.TrimSpace(in.DisplayName), "role": in.Role, "status": "ACTIVE"})
}

func (a *api) workspaceMember(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := a.requireWorkspace(w, r)
	if !ok {
		return
	}
	operatorID := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/workspace/members/"), "/")
	if operatorID == "" || (r.Method != http.MethodPut && r.Method != http.MethodDelete) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if _, _, ok := a.requireWorkspaceRole(w, r, "OWNER", "ADMIN"); !ok {
		return
	}
	var currentRole, platformRole string
	if err := a.db.QueryRowContext(r.Context(), `SELECT wm.role,o.platform_role FROM workspace_members wm JOIN operators o ON o.id=wm.operator_id WHERE wm.workspace_id=$1 AND wm.operator_id=$2`, workspaceID, operatorID).Scan(&currentRole, &platformRole); err != nil {
		a.json(w, 404, map[string]string{"error": "workspace member not found"})
		return
	}
	if currentRole == "OWNER" || platformRole == "PLATFORM_OWNER" || platformRole == "PLATFORM_ADMIN" {
		a.json(w, http.StatusConflict, map[string]string{"error": "workspace owner and product administrator access cannot be changed here"})
		return
	}
	if r.Method == http.MethodDelete {
		_, err := a.db.ExecContext(r.Context(), `DELETE FROM workspace_members WHERE workspace_id=$1 AND operator_id=$2`, workspaceID, operatorID)
		if err != nil {
			a.json(w, 409, map[string]string{"error": err.Error()})
			return
		}
		a.json(w, 200, map[string]string{"status": "removed", "operator_id": operatorID})
		return
	}
	var in struct {
		Role string `json:"role"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil || (in.Role != "ADMIN" && in.Role != "OPERATOR" && in.Role != "VIEWER") {
		a.json(w, 400, map[string]string{"error": "role must be ADMIN, OPERATOR, or VIEWER"})
		return
	}
	if _, err := a.db.ExecContext(r.Context(), `UPDATE workspace_members SET role=$1 WHERE workspace_id=$2 AND operator_id=$3`, in.Role, workspaceID, operatorID); err != nil {
		a.json(w, 409, map[string]string{"error": err.Error()})
		return
	}
	a.json(w, 200, map[string]string{"status": "saved", "operator_id": operatorID, "role": in.Role})
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
func chooseNode(nodes []node, callRole string) (node, error) {
	var best node
	ok := false
	for _, n := range nodes {
		// Host and listener calls belong on leaf/listener media nodes. Root
		// nodes are reserved for room media placement by the RustPBX layer.
		if strings.EqualFold(callRole, "HOST") || strings.EqualFold(callRole, "LISTENER") {
			if strings.EqualFold(n.Role, "root") || strings.EqualFold(n.Role, "mixer") {
				continue
			}
		}
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
		rows, err := a.db.QueryContext(r.Context(), `SELECT b.id::text,b.name,b.status,b.room_state,COALESCE(b.host_name,''),COALESCE(b.host_phone,''),COALESCE(b.default_region,'+91'),b.participant_limit,COALESCE(t.id::text,''),COALESCE(t.e164_number,''),COALESCE(t.label,''),b.created_at FROM bridges b LEFT JOIN telephony_numbers t ON t.id=b.tfn_id WHERE b.workspace_id=$1 ORDER BY b.created_at DESC`, workspaceID)
		if err != nil {
			a.json(w, 500, map[string]string{"error": err.Error()})
			return
		}
		defer rows.Close()
		out := []map[string]any{}
		for rows.Next() {
			var id, name, status, roomState, hostName, hostPhone, defaultRegion, tfnID, tfnNumber, tfnLabel string
			var participantLimit int
			var created time.Time
			_ = rows.Scan(&id, &name, &status, &roomState, &hostName, &hostPhone, &defaultRegion, &participantLimit, &tfnID, &tfnNumber, &tfnLabel, &created)
			out = append(out, map[string]any{"id": id, "name": name, "status": status, "room_state": roomState, "host_name": hostName, "host_phone": hostPhone, "default_region": normalizeDialRegion(defaultRegion), "participant_limit": participantLimit, "tfn_id": tfnID, "tfn_number": tfnNumber, "tfn_label": tfnLabel, "created_at": created})
		}
		a.json(w, 200, out)
	case http.MethodPost:
		if _, _, ok := a.requireWorkspaceRole(w, r, "OWNER", "ADMIN"); !ok {
			return
		}
		var in struct {
			Name string `json:"name"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil || strings.TrimSpace(in.Name) == "" {
			a.json(w, 400, map[string]string{"error": "name required"})
			return
		}
		var id string
		err := a.db.QueryRowContext(r.Context(), `INSERT INTO bridges(workspace_id,name,participant_limit) SELECT $1,$2,max_participants FROM workspaces WHERE id=$1 RETURNING id::text`, workspaceID, in.Name).Scan(&id)
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
	configurationChange := r.Method == http.MethodDelete || (len(parts) == 2 && (parts[1] == "host" || parts[1] == "settings" || parts[1] == "participants" || parts[1] == "batches"))
	if configurationChange {
		if _, _, ok := a.requireWorkspaceRole(w, r, "OWNER", "ADMIN"); !ok {
			return
		}
	} else if r.Method != http.MethodGet {
		if _, _, ok := a.requireWorkspaceRole(w, r, "OWNER", "ADMIN", "OPERATOR"); !ok {
			return
		}
	}
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
	if len(parts) == 2 && parts[1] == "host" && (r.Method == http.MethodPut || r.Method == http.MethodDelete) {
		a.host(w, r, workspaceID, bridgeID)
		return
	}
	if len(parts) == 2 && parts[1] == "settings" && r.Method == http.MethodPut {
		a.bridgeSettings(w, r, workspaceID, bridgeID)
		return
	}
	if len(parts) == 2 && parts[1] == "participants" && r.Method == http.MethodPost {
		a.addRosterParticipant(w, r, workspaceID, bridgeID)
		return
	}
	if len(parts) == 2 && parts[1] == "start" && r.Method == http.MethodPost {
		a.startRoom(w, r, workspaceID, bridgeID)
		return
	}
	if len(parts) == 2 && parts[1] == "stop" && r.Method == http.MethodPost {
		a.stopRoom(w, r, workspaceID, bridgeID)
		return
	}
	if len(parts) == 2 && parts[1] == "sessions" && r.Method == http.MethodGet {
		a.listSessions(w, r, workspaceID, bridgeID)
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

func (a *api) roomStartChecks(ctx context.Context, workspaceID, bridgeID string) ([]map[string]any, bool) {
	var hostName, hostPhone, tfnID, tfnNumber, tfnLabel, tfnStatus string
	var participantLimit, participantCount int
	if err := a.db.QueryRowContext(ctx, `SELECT COALESCE(b.host_name,''),COALESCE(b.host_phone,''),b.participant_limit,COUNT(p.id),COALESCE(t.id::text,''),COALESCE(t.e164_number,''),COALESCE(t.label,''),COALESCE(t.status,'') FROM bridges b LEFT JOIN participants p ON p.bridge_id=b.id AND p.workspace_id=b.workspace_id LEFT JOIN telephony_numbers t ON t.id=b.tfn_id AND t.workspace_id=b.workspace_id WHERE b.id=$1 AND b.workspace_id=$2 GROUP BY b.id,t.id`, bridgeID, workspaceID).Scan(&hostName, &hostPhone, &participantLimit, &participantCount, &tfnID, &tfnNumber, &tfnLabel, &tfnStatus); err != nil {
		return []map[string]any{{"key": "room", "label": "Room", "ok": false, "detail": "Room configuration could not be read"}}, false
	}
	hostOK := strings.TrimSpace(hostName) != "" && strings.TrimSpace(hostPhone) != ""
	tfnOK := tfnID != "" && tfnStatus == "ACTIVE" && strings.TrimSpace(tfnNumber) != ""
	capacityOK := participantLimit > 0 && participantCount <= participantLimit
	checks := []map[string]any{
		{"key": "host", "label": "Fixed host", "ok": hostOK, "detail": func() string {
			if hostOK {
				return hostName
			}
			return "Add a host in Room settings"
		}()},
		{"key": "tfn", "label": "Room TFN", "ok": tfnOK, "detail": func() string {
			if tfnOK {
				if tfnLabel != "" {
					return tfnLabel + " · " + tfnNumber
				}
				return tfnNumber
			}
			return "Product admin must assign an active TFN"
		}()},
		{"key": "capacity", "label": "Participant limit", "ok": capacityOK, "detail": fmt.Sprintf("%d of %d rostered", participantCount, participantLimit)},
	}
	ready := hostOK && tfnOK && capacityOK
	return checks, ready
}

func (a *api) bridgeState(w http.ResponseWriter, r *http.Request, workspaceID, id string) {
	var roomState, hostName, hostPhone, defaultRegion, tfnID, tfnNumber, tfnLabel string
	var participantLimit int
	if err := a.db.QueryRowContext(r.Context(), `SELECT b.room_state,COALESCE(b.host_name,''),COALESCE(b.host_phone,''),COALESCE(b.default_region,'+91'),b.participant_limit,COALESCE(t.id::text,''),COALESCE(t.e164_number,''),COALESCE(t.label,'') FROM bridges b LEFT JOIN telephony_numbers t ON t.id=b.tfn_id WHERE b.id=$1 AND b.workspace_id=$2`, id, workspaceID).Scan(&roomState, &hostName, &hostPhone, &defaultRegion, &participantLimit, &tfnID, &tfnNumber, &tfnLabel); err != nil {
		if err == sql.ErrNoRows {
			a.json(w, 404, map[string]string{"error": "room not found"})
			return
		}
		a.json(w, 500, map[string]string{"error": err.Error()})
		return
	}
	currentEpochs := map[string]int64{}
	if nodes, nodeErr := a.nodes(r.Context()); nodeErr == nil {
		for _, current := range nodes {
			currentEpochs[current.NodeID] = current.NodeEpoch
		}
	}
	rows, err := a.db.QueryContext(r.Context(), `SELECT p.id::text,p.display_name,p.phone_number,COALESCE(p.role,'LISTENER'),p.desired_state,COALESCE(c.id::text,''),COALESCE(c.state,''),COALESCE(c.node_id,''),COALESCE(c.node_epoch,0) FROM participants p LEFT JOIN LATERAL (SELECT * FROM call_legs WHERE participant_id=p.id AND workspace_id=$2 ORDER BY created_at DESC LIMIT 1)c ON true WHERE p.bridge_id=$1 AND p.workspace_id=$2 ORDER BY CASE WHEN p.role='HOST' THEN 0 ELSE 1 END,p.created_at`, id, workspaceID)
	if err != nil {
		a.json(w, 500, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var pid, name, phone, role, desired, callID, state, nodeID string
		var epoch int64
		_ = rows.Scan(&pid, &name, &phone, &role, &desired, &callID, &state, &nodeID, &epoch)
		if callID != "" && state != "" && state != "ENDED" && state != "FAILED" && state != "DROPPED" {
			if current, ok := currentEpochs[nodeID]; ok && epoch != 0 && current != epoch {
				staleCallID := callID
				callID, state, nodeID = "", "", ""
				_, _ = a.db.ExecContext(r.Context(), `UPDATE call_legs SET state='ENDED',ended_at=COALESCE(ended_at,now()),updated_at=now(),last_error='sidecar epoch changed; call no longer active' WHERE id=$1 AND workspace_id=$2 AND state NOT IN ('ENDED','FAILED','DROPPED')`, staleCallID, workspaceID)
			}
		}
		out = append(out, map[string]any{"participant_id": pid, "name": name, "phone_number": phone, "role": normalizeParticipantRole(role), "desired_state": desired, "call_id": callID, "state": state, "node_id": nodeID})
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
	checks, ready := a.roomStartChecks(r.Context(), workspaceID, id)
	tfn := map[string]string{"id": tfnID, "number": tfnNumber, "label": tfnLabel}
	a.json(w, 200, map[string]any{"workspace_id": workspaceID, "bridge_id": id, "room_state": roomState, "default_region": normalizeDialRegion(defaultRegion), "participant_limit": participantLimit, "tfn": tfn, "start_ready": ready, "start_checks": checks, "host": map[string]string{"name": hostName, "phone_number": hostPhone}, "participants": out, "speaker_requests": requests, "sessions": a.sessionRows(r.Context(), workspaceID, id)})
}

func (a *api) sessionRows(ctx context.Context, workspaceID, bridgeID string) []map[string]any {
	rows, err := a.db.QueryContext(ctx, `SELECT rs.id::text,rs.status,rs.started_at,rs.ended_at,COALESCE(rs.duration_seconds,EXTRACT(EPOCH FROM (COALESCE(rs.ended_at,now())-rs.started_at))::bigint),COALESCE(json_agg(json_build_object('participant_id',rsp.participant_id::text,'name',rsp.display_name,'phone_number',rsp.phone_number,'role',rsp.role,'state',rsp.call_state,'joined_at',rsp.joined_at,'left_at',rsp.left_at) ORDER BY rsp.created_at) FILTER (WHERE rsp.id IS NOT NULL),'[]'::json) FROM room_sessions rs LEFT JOIN room_session_participants rsp ON rsp.session_id=rs.id WHERE rs.workspace_id=$1 AND rs.bridge_id=$2 GROUP BY rs.id ORDER BY rs.started_at DESC LIMIT 25`, workspaceID, bridgeID)
	if err != nil {
		return []map[string]any{}
	}
	defer rows.Close()
	result := []map[string]any{}
	for rows.Next() {
		var id, status string
		var startedAt, endedAt sql.NullTime
		var duration int64
		var participants []byte
		if rows.Scan(&id, &status, &startedAt, &endedAt, &duration, &participants) != nil {
			continue
		}
		var roster any
		if json.Unmarshal(participants, &roster) != nil {
			roster = []any{}
		}
		row := map[string]any{"session_id": id, "status": status, "duration_seconds": duration, "participants": roster}
		if startedAt.Valid {
			row["started_at"] = startedAt.Time
		}
		if endedAt.Valid {
			row["ended_at"] = endedAt.Time
		}
		result = append(result, row)
	}
	return result
}

func (a *api) host(w http.ResponseWriter, r *http.Request, workspaceID, bridgeID string) {
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		a.json(w, 500, map[string]string{"error": err.Error()})
		return
	}
	defer tx.Rollback()
	var roomState string
	if err := tx.QueryRowContext(r.Context(), `SELECT room_state FROM bridges WHERE id=$1 AND workspace_id=$2 FOR UPDATE`, bridgeID, workspaceID).Scan(&roomState); err != nil {
		a.json(w, 404, map[string]string{"error": "room not found"})
		return
	}
	if roomState == "RUNNING" || roomState == "STARTING" {
		a.json(w, http.StatusConflict, map[string]string{"error": "stop the room before editing its host"})
		return
	}
	if r.Method == http.MethodDelete {
		if _, err := tx.ExecContext(r.Context(), `UPDATE bridges SET host_name=NULL,host_phone=NULL,updated_at=now() WHERE id=$1 AND workspace_id=$2`, bridgeID, workspaceID); err != nil {
			a.json(w, 500, map[string]string{"error": err.Error()})
			return
		}
		if _, err := tx.ExecContext(r.Context(), `DELETE FROM participants WHERE bridge_id=$1 AND workspace_id=$2 AND role='HOST'`, bridgeID, workspaceID); err != nil {
			a.json(w, 500, map[string]string{"error": err.Error()})
			return
		}
		if err := tx.Commit(); err != nil {
			a.json(w, 500, map[string]string{"error": err.Error()})
			return
		}
		a.json(w, 200, map[string]string{"status": "host_cleared", "bridge_id": bridgeID})
		return
	}
	var in struct {
		Name  string `json:"name"`
		Phone string `json:"phone_number"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil || strings.TrimSpace(in.Name) == "" || strings.TrimSpace(in.Phone) == "" {
		a.json(w, 400, map[string]string{"error": "host name and phone_number are required"})
		return
	}
	in.Name, in.Phone = strings.TrimSpace(in.Name), strings.TrimSpace(in.Phone)
	if _, err := tx.ExecContext(r.Context(), `UPDATE bridges SET host_name=$1,host_phone=$2,updated_at=now() WHERE id=$3 AND workspace_id=$4`, in.Name, in.Phone, bridgeID, workspaceID); err != nil {
		a.json(w, 500, map[string]string{"error": err.Error()})
		return
	}
	var hostID string
	err = tx.QueryRowContext(r.Context(), `SELECT id::text FROM participants WHERE bridge_id=$1 AND workspace_id=$2 AND role='HOST' LIMIT 1`, bridgeID, workspaceID).Scan(&hostID)
	if err == sql.ErrNoRows {
		err = tx.QueryRowContext(r.Context(), `INSERT INTO participants(workspace_id,bridge_id,display_name,phone_number,role,desired_state) VALUES($1,$2,$3,$4,'HOST','ACTIVE') RETURNING id::text`, workspaceID, bridgeID, in.Name, in.Phone).Scan(&hostID)
	} else if err == nil {
		_, err = tx.ExecContext(r.Context(), `UPDATE participants SET display_name=$1,phone_number=$2,desired_state='ACTIVE' WHERE id=$3 AND workspace_id=$4`, in.Name, in.Phone, hostID, workspaceID)
	}
	if err != nil {
		a.json(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if err := tx.Commit(); err != nil {
		a.json(w, 500, map[string]string{"error": err.Error()})
		return
	}
	a.json(w, 200, map[string]any{"status": "saved", "bridge_id": bridgeID, "host": map[string]string{"participant_id": hostID, "name": in.Name, "phone_number": in.Phone}})
}

func (a *api) bridgeSettings(w http.ResponseWriter, r *http.Request, workspaceID, bridgeID string) {
	var in struct {
		DefaultRegion string `json:"default_region"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		a.json(w, http.StatusBadRequest, map[string]string{"error": "invalid settings payload"})
		return
	}
	region := normalizeDialRegion(in.DefaultRegion)
	if strings.TrimSpace(in.DefaultRegion) != "" && region != strings.TrimSpace(in.DefaultRegion) {
		a.json(w, http.StatusBadRequest, map[string]string{"error": "unsupported default_region"})
		return
	}
	var roomState string
	if err := a.db.QueryRowContext(r.Context(), `SELECT room_state FROM bridges WHERE id=$1 AND workspace_id=$2`, bridgeID, workspaceID).Scan(&roomState); err != nil {
		if err == sql.ErrNoRows {
			a.json(w, http.StatusNotFound, map[string]string{"error": "room not found"})
			return
		}
		a.json(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if roomState == "RUNNING" || roomState == "STARTING" {
		a.json(w, http.StatusConflict, map[string]string{"error": "stop the room before changing its dialing region"})
		return
	}
	if _, err := a.db.ExecContext(r.Context(), `UPDATE bridges SET default_region=$1,updated_at=now() WHERE id=$2 AND workspace_id=$3`, region, bridgeID, workspaceID); err != nil {
		a.json(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	a.json(w, http.StatusOK, map[string]any{"status": "saved", "bridge_id": bridgeID, "default_region": region})
}

func (a *api) addRosterParticipant(w http.ResponseWriter, r *http.Request, workspaceID, bridgeID string) {
	var roomState string
	if err := a.db.QueryRowContext(r.Context(), `SELECT room_state FROM bridges WHERE id=$1 AND workspace_id=$2`, bridgeID, workspaceID).Scan(&roomState); err != nil {
		a.json(w, 404, map[string]string{"error": "room not found"})
		return
	}
	if roomState == "RUNNING" || roomState == "STARTING" {
		a.json(w, http.StatusConflict, map[string]string{"error": "stop the room before editing its participant roster"})
		return
	}
	var participantLimit, participantCount int
	if err := a.db.QueryRowContext(r.Context(), `SELECT b.participant_limit,COUNT(p.id) FROM bridges b LEFT JOIN participants p ON p.bridge_id=b.id AND p.workspace_id=b.workspace_id WHERE b.id=$1 AND b.workspace_id=$2 GROUP BY b.id`, bridgeID, workspaceID).Scan(&participantLimit, &participantCount); err != nil {
		a.json(w, 404, map[string]string{"error": "room not found"})
		return
	}
	if participantCount >= participantLimit {
		a.json(w, http.StatusConflict, map[string]any{"error": "room participant limit reached", "participant_limit": participantLimit, "participant_count": participantCount})
		return
	}
	var in struct {
		Name  string `json:"name"`
		Phone string `json:"phone_number"`
		Role  string `json:"role"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil || strings.TrimSpace(in.Name) == "" || strings.TrimSpace(in.Phone) == "" {
		a.json(w, 400, map[string]string{"error": "name and phone_number are required"})
		return
	}
	if normalizeParticipantRole(in.Role) == "HOST" {
		a.json(w, 400, map[string]string{"error": "configure the room host in room settings"})
		return
	}
	var id string
	if err := a.db.QueryRowContext(r.Context(), `INSERT INTO participants(workspace_id,bridge_id,display_name,phone_number,role,desired_state) VALUES($1,$2,$3,$4,'LISTENER','MUTED') RETURNING id::text`, workspaceID, bridgeID, strings.TrimSpace(in.Name), strings.TrimSpace(in.Phone)).Scan(&id); err != nil {
		a.json(w, 500, map[string]string{"error": err.Error()})
		return
	}
	a.json(w, 201, map[string]any{"participant_id": id, "name": in.Name, "phone_number": in.Phone, "role": "LISTENER", "state": "ROSTERED"})
}

func (a *api) editRosterParticipant(w http.ResponseWriter, r *http.Request, participantID string) {
	workspaceID, ok := a.requireWorkspace(w, r)
	if !ok {
		return
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		a.json(w, 500, map[string]string{"error": err.Error()})
		return
	}
	defer tx.Rollback()
	var bridgeID, role, roomState string
	if err := tx.QueryRowContext(r.Context(), `SELECT p.bridge_id::text,p.role,b.room_state FROM participants p JOIN bridges b ON b.id=p.bridge_id WHERE p.id=$1 AND p.workspace_id=$2 FOR UPDATE`, participantID, workspaceID).Scan(&bridgeID, &role, &roomState); err != nil {
		a.json(w, 404, map[string]string{"error": "participant not found"})
		return
	}
	if role == "HOST" {
		a.json(w, 400, map[string]string{"error": "edit the host from Room settings"})
		return
	}
	if roomState == "RUNNING" || roomState == "STARTING" {
		a.json(w, http.StatusConflict, map[string]string{"error": "stop the room before editing its participant roster"})
		return
	}
	var in struct {
		Name  string `json:"name"`
		Phone string `json:"phone_number"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil || strings.TrimSpace(in.Name) == "" || strings.TrimSpace(in.Phone) == "" {
		a.json(w, 400, map[string]string{"error": "name and phone_number are required"})
		return
	}
	in.Name, in.Phone = strings.TrimSpace(in.Name), strings.TrimSpace(in.Phone)
	if _, err := tx.ExecContext(r.Context(), `UPDATE participants SET display_name=$1,phone_number=$2 WHERE id=$3 AND workspace_id=$4`, in.Name, in.Phone, participantID, workspaceID); err != nil {
		a.json(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if _, err := tx.ExecContext(r.Context(), `UPDATE dial_batch_items SET name=$1,phone_number=$2,updated_at=now() WHERE participant_id=$3 AND workspace_id=$4`, in.Name, in.Phone, participantID, workspaceID); err != nil {
		a.json(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if err := tx.Commit(); err != nil {
		a.json(w, 500, map[string]string{"error": err.Error()})
		return
	}
	a.json(w, 200, map[string]any{"status": "saved", "participant_id": participantID, "bridge_id": bridgeID, "name": in.Name, "phone_number": in.Phone, "role": "LISTENER"})
}

func (a *api) startRoom(w http.ResponseWriter, r *http.Request, workspaceID, bridgeID string) {
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		a.json(w, 500, map[string]string{"error": err.Error()})
		return
	}
	defer tx.Rollback()
	var roomState, hostName, hostPhone, tfnID, tfnNumber, tfnStatus string
	var participantLimit, participantCount int
	if err := tx.QueryRowContext(r.Context(), `SELECT b.room_state,COALESCE(b.host_name,''),COALESCE(b.host_phone,''),b.participant_limit,COUNT(p.id),COALESCE(t.id::text,''),COALESCE(t.e164_number,''),COALESCE(t.status,'') FROM bridges b LEFT JOIN participants p ON p.bridge_id=b.id AND p.workspace_id=b.workspace_id LEFT JOIN telephony_numbers t ON t.id=b.tfn_id AND t.workspace_id=b.workspace_id WHERE b.id=$1 AND b.workspace_id=$2 GROUP BY b.id,t.id`, bridgeID, workspaceID).Scan(&roomState, &hostName, &hostPhone, &participantLimit, &participantCount, &tfnID, &tfnNumber, &tfnStatus); err != nil {
		a.json(w, 404, map[string]string{"error": "room not found"})
		return
	}
	if roomState == "RUNNING" || roomState == "STARTING" {
		a.json(w, http.StatusConflict, map[string]string{"error": "room is already running"})
		return
	}
	if strings.TrimSpace(hostName) == "" || strings.TrimSpace(hostPhone) == "" {
		a.json(w, http.StatusConflict, map[string]any{"error": "configure a host before starting the room", "code": "ROOM_START_PREREQUISITES", "checks": []map[string]any{{"key": "host", "label": "Fixed host", "ok": false, "detail": "Add a host in Room settings"}, {"key": "tfn", "label": "Room TFN", "ok": tfnID != "" && tfnStatus == "ACTIVE", "detail": "Product admin must assign an active TFN"}, {"key": "capacity", "label": "Participant limit", "ok": participantCount <= participantLimit, "detail": fmt.Sprintf("%d of %d rostered", participantCount, participantLimit)}}})
		return
	}
	if tfnID == "" || tfnStatus != "ACTIVE" || strings.TrimSpace(tfnNumber) == "" {
		a.json(w, http.StatusConflict, map[string]any{"error": "assign an active TFN to this room before starting it", "code": "ROOM_START_PREREQUISITES", "checks": []map[string]any{{"key": "host", "label": "Fixed host", "ok": true, "detail": hostName}, {"key": "tfn", "label": "Room TFN", "ok": false, "detail": "Product admin must assign an active TFN"}, {"key": "capacity", "label": "Participant limit", "ok": participantCount <= participantLimit, "detail": fmt.Sprintf("%d of %d rostered", participantCount, participantLimit)}}})
		return
	}
	if participantLimit <= 0 || participantCount > participantLimit {
		a.json(w, http.StatusConflict, map[string]any{"error": "room participant limit exceeded", "code": "ROOM_START_PREREQUISITES", "checks": []map[string]any{{"key": "host", "label": "Fixed host", "ok": true, "detail": hostName}, {"key": "tfn", "label": "Room TFN", "ok": true, "detail": tfnNumber}, {"key": "capacity", "label": "Participant limit", "ok": false, "detail": fmt.Sprintf("%d of %d rostered", participantCount, participantLimit)}}})
		return
	}
	var hostID string
	err = tx.QueryRowContext(r.Context(), `SELECT id::text FROM participants WHERE bridge_id=$1 AND workspace_id=$2 AND role='HOST' LIMIT 1`, bridgeID, workspaceID).Scan(&hostID)
	if err == sql.ErrNoRows {
		err = tx.QueryRowContext(r.Context(), `INSERT INTO participants(workspace_id,bridge_id,display_name,phone_number,role,desired_state) VALUES($1,$2,$3,$4,'HOST','ACTIVE') RETURNING id::text`, workspaceID, bridgeID, hostName, hostPhone).Scan(&hostID)
	}
	if err != nil {
		a.json(w, 500, map[string]string{"error": err.Error()})
		return
	}
	var sessionID string
	if err := tx.QueryRowContext(r.Context(), `INSERT INTO room_sessions(workspace_id,bridge_id,host_participant_id,status) VALUES($1,$2,$3,'STARTING') RETURNING id::text`, workspaceID, bridgeID, hostID).Scan(&sessionID); err != nil {
		a.json(w, http.StatusConflict, map[string]string{"error": "room already has an active session"})
		return
	}
	if _, err := tx.ExecContext(r.Context(), `UPDATE bridges SET room_state='STARTING',updated_at=now() WHERE id=$1 AND workspace_id=$2`, bridgeID, workspaceID); err != nil {
		a.json(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if err := tx.Commit(); err != nil {
		a.json(w, 500, map[string]string{"error": err.Error()})
		return
	}
	hostCall, err := a.dispatchExistingParticipant(r.Context(), workspaceID, bridgeID, hostID, sessionID)
	if err != nil {
		_, _ = a.db.ExecContext(r.Context(), `UPDATE room_sessions SET status='FAILED',ended_at=now(),duration_seconds=0 WHERE id=$1`, sessionID)
		_, _ = a.db.ExecContext(r.Context(), `UPDATE bridges SET room_state='READY',updated_at=now() WHERE id=$1 AND workspace_id=$2`, bridgeID, workspaceID)
		a.json(w, 502, map[string]string{"error": "host call could not be dispatched: " + err.Error()})
		return
	}
	go a.activateRoomAfterHost(sessionID, workspaceID, bridgeID, hostCall["call_id"].(string))
	a.json(w, http.StatusAccepted, map[string]any{"session_id": sessionID, "room_state": "STARTING", "host": hostCall})
}

func (a *api) activateRoomAfterHost(sessionID, workspaceID, bridgeID, hostCallID string) {
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		var state string
		if err := a.db.QueryRow(`SELECT state FROM call_legs WHERE id=$1`, hostCallID).Scan(&state); err == nil {
			if state == "ANSWERED" || state == "IN_BRIDGE" {
				result, updateErr := a.db.Exec(`UPDATE room_sessions SET status='RUNNING' WHERE id=$1 AND status='STARTING'`, sessionID)
				if updateErr != nil {
					return
				}
				changed, _ := result.RowsAffected()
				if changed != 1 {
					// Stop Room may have closed the session while the host call was
					// still transitioning. Never resurrect a stopped room.
					return
				}
				_, _ = a.db.Exec(`UPDATE bridges SET room_state='RUNNING',updated_at=now() WHERE id=$1 AND workspace_id=$2 AND room_state='STARTING'`, bridgeID, workspaceID)
				rows, _ := a.db.Query(`SELECT id::text FROM participants WHERE bridge_id=$1 AND workspace_id=$2 AND role='LISTENER' ORDER BY created_at`, bridgeID, workspaceID)
				var ids []string
				if rows != nil {
					for rows.Next() {
						var id string
						if rows.Scan(&id) == nil {
							ids = append(ids, id)
						}
					}
					rows.Close()
				}
				var wg sync.WaitGroup
				semaphore := make(chan struct{}, 32)
				for _, participantID := range ids {
					participantID := participantID
					wg.Add(1)
					go func() {
						defer wg.Done()
						semaphore <- struct{}{}
						defer func() { <-semaphore }()
						_, _ = a.dispatchExistingParticipant(context.Background(), workspaceID, bridgeID, participantID, sessionID)
					}()
				}
				wg.Wait()
				return
			}
			if state == "FAILED" || state == "ENDED" || state == "DROPPED" {
				_, _ = a.db.Exec(`UPDATE room_sessions SET status='FAILED',ended_at=now() WHERE id=$1 AND status='STARTING'`, sessionID)
				_, _ = a.db.Exec(`UPDATE bridges SET room_state='READY',updated_at=now() WHERE id=$1 AND workspace_id=$2`, bridgeID, workspaceID)
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	_, _ = a.db.Exec(`UPDATE room_sessions SET status='FAILED',ended_at=now() WHERE id=$1 AND status='STARTING'`, sessionID)
	_, _ = a.db.Exec(`UPDATE bridges SET room_state='READY',updated_at=now() WHERE id=$1 AND workspace_id=$2`, bridgeID, workspaceID)
}

func (a *api) stopRoom(w http.ResponseWriter, r *http.Request, workspaceID, bridgeID string) {
	var sessionID string
	if err := a.db.QueryRowContext(r.Context(), `SELECT id::text FROM room_sessions WHERE bridge_id=$1 AND workspace_id=$2 AND status IN ('STARTING','RUNNING') ORDER BY started_at DESC LIMIT 1`, bridgeID, workspaceID).Scan(&sessionID); err != nil {
		if err == sql.ErrNoRows {
			a.json(w, 200, map[string]string{"status": "already_stopped", "room_state": "READY"})
			return
		}
		a.json(w, 500, map[string]string{"error": err.Error()})
		return
	}
	rows, err := a.db.QueryContext(r.Context(), `SELECT id::text,participant_id::text,node_id,node_epoch FROM call_legs WHERE bridge_id=$1 AND workspace_id=$2 AND state NOT IN ('ENDED','FAILED','DROPPED')`, bridgeID, workspaceID)
	if err == nil {
		for rows.Next() {
			var callID, participantID, nodeID string
			var epoch int64
			if rows.Scan(&callID, &participantID, &nodeID, &epoch) == nil {
				_ = a.dropCall(r.Context(), workspaceID, callID, participantID, nodeID, epoch)
			}
		}
		rows.Close()
	}
	_, _ = a.db.ExecContext(r.Context(), `UPDATE room_sessions SET status='ENDED',ended_at=now(),duration_seconds=EXTRACT(EPOCH FROM (now()-started_at))::bigint WHERE id=$1 AND workspace_id=$2`, sessionID, workspaceID)
	_, _ = a.db.ExecContext(r.Context(), `UPDATE room_session_participants SET left_at=COALESCE(left_at,now()) WHERE session_id=$1 AND left_at IS NULL`, sessionID)
	_, _ = a.db.ExecContext(r.Context(), `UPDATE bridges SET room_state='READY',updated_at=now() WHERE id=$1 AND workspace_id=$2`, bridgeID, workspaceID)
	a.json(w, 200, map[string]string{"status": "stopped", "room_state": "READY", "session_id": sessionID})
}

func (a *api) listSessions(w http.ResponseWriter, r *http.Request, workspaceID, bridgeID string) {
	a.json(w, 200, map[string]any{"workspace_id": workspaceID, "bridge_id": bridgeID, "sessions": a.sessionRows(r.Context(), workspaceID, bridgeID)})
}

func (a *api) dropCall(ctx context.Context, workspaceID, callID, participantID, nodeID string, nodeEpoch int64) error {
	nodes, err := a.nodes(ctx)
	if err != nil {
		return err
	}
	var owner node
	for _, candidate := range nodes {
		if candidate.NodeID == nodeID {
			owner = candidate
			break
		}
	}
	if owner.NodeID == "" {
		return fmt.Errorf("owning PBX is unavailable")
	}
	command := newID()
	if err := a.sidecar(ctx, owner, "/v1/participants/"+participantID+"/drop", map[string]any{"command_id": command, "call_id": callID, "participant_id": participantID, "node_epoch": nodeEpoch}); err != nil {
		return err
	}
	_, _ = a.db.ExecContext(ctx, `UPDATE call_legs SET state='DROPPED',ended_at=COALESCE(ended_at,now()),updated_at=now() WHERE id=$1 AND workspace_id=$2`, callID, workspaceID)
	return nil
}

func normalizeParticipantRole(role string) string {
	if strings.EqualFold(strings.TrimSpace(role), "HOST") {
		return "HOST"
	}
	return "LISTENER"
}

func normalizeDialRegion(region string) string {
	region = strings.TrimSpace(region)
	if _, ok := supportedDialRegions[region]; ok {
		return region
	}
	return "+91"
}

func normalizeDialNumber(phone, region string) string {
	phone = strings.TrimSpace(phone)
	if phone == "" {
		return ""
	}
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, phone)
	if digits == "" {
		return ""
	}
	if strings.HasPrefix(phone, "+") {
		return "+" + digits
	}
	digits = strings.TrimLeft(digits, "0")
	if digits == "" {
		return ""
	}
	return normalizeDialRegion(region) + digits
}

func (a *api) dispatchExistingParticipant(ctx context.Context, workspaceID, bridgeID, participantID, sessionID string) (map[string]any, error) {
	var name, phone, role, defaultRegion, tfnNumber, tfnStatus string
	if err := a.db.QueryRowContext(ctx, `SELECT p.display_name,p.phone_number,COALESCE(p.role,'LISTENER'),COALESCE(b.default_region,'+91'),COALESCE(t.e164_number,''),COALESCE(t.status,'') FROM participants p JOIN bridges b ON b.id=p.bridge_id AND b.workspace_id=p.workspace_id LEFT JOIN telephony_numbers t ON t.id=b.tfn_id AND t.workspace_id=b.workspace_id WHERE p.id=$1 AND p.workspace_id=$2 AND p.bridge_id=$3`, participantID, workspaceID, bridgeID).Scan(&name, &phone, &role, &defaultRegion, &tfnNumber, &tfnStatus); err != nil {
		return nil, err
	}
	if tfnStatus != "ACTIVE" || strings.TrimSpace(tfnNumber) == "" {
		return nil, errors.New("room has no active TFN assigned")
	}
	dialNumber := normalizeDialNumber(phone, defaultRegion)
	if dialNumber == "" {
		return nil, errors.New("participant phone number is invalid")
	}
	role = normalizeParticipantRole(role)
	nodes, err := a.nodes(ctx)
	if err != nil {
		return nil, err
	}
	n, err := chooseNode(nodes, role)
	if err != nil {
		return nil, err
	}
	desiredMute := role != "HOST"
	var callID string
	if err := a.db.QueryRowContext(ctx, `INSERT INTO call_legs(workspace_id,participant_id,bridge_id,node_id,node_epoch,sidecar_instance_id,state,desired_mute) VALUES($1,$2,$3,$4,$5,'','DISPATCHED',$6) RETURNING id::text`, workspaceID, participantID, bridgeID, n.NodeID, n.NodeEpoch, desiredMute).Scan(&callID); err != nil {
		return nil, err
	}
	command := newID()
	body := map[string]any{"command_id": command, "call_id": callID, "bridge_id": bridgeID, "participant_id": participantID, "destination": dialNumber, "caller_id": tfnNumber, "role": role, "desired_mute": desiredMute}
	if err := a.sidecar(ctx, n, "/v1/calls/originate", body); err != nil {
		_, _ = a.db.ExecContext(ctx, `UPDATE call_legs SET state='FAILED',last_error=$1,ended_at=now(),updated_at=now() WHERE id=$2`, err.Error(), callID)
		return nil, err
	}
	participantState := "MUTED"
	if role == "HOST" {
		participantState = "ACTIVE"
	}
	_, _ = a.db.ExecContext(ctx, `UPDATE participants SET desired_state=$1 WHERE id=$2 AND workspace_id=$3`, participantState, participantID, workspaceID)
	if sessionID != "" {
		_, _ = a.db.ExecContext(ctx, `INSERT INTO room_session_participants(session_id,participant_id,call_leg_id,display_name,phone_number,role,call_state) VALUES($1,$2,$3,$4,$5,$6,'DISPATCHED')`, sessionID, participantID, callID, name, dialNumber, role)
	}
	return map[string]any{"participant_id": participantID, "call_id": callID, "node_id": n.NodeID, "role": role, "state": "DISPATCHED"}, nil
}

func (a *api) dispatchDial(ctx context.Context, workspaceID, bridgeID, name, phone string) (map[string]any, error) {
	var roomState, tfnStatus, tfnNumber string
	var participantLimit, participantCount int
	if err := a.db.QueryRowContext(ctx, `SELECT b.room_state,b.participant_limit,COUNT(p.id),COALESCE(t.status,''),COALESCE(t.e164_number,'') FROM bridges b LEFT JOIN participants p ON p.bridge_id=b.id AND p.workspace_id=b.workspace_id LEFT JOIN telephony_numbers t ON t.id=b.tfn_id AND t.workspace_id=b.workspace_id WHERE b.id=$1 AND b.workspace_id=$2 GROUP BY b.id,t.id`, bridgeID, workspaceID).Scan(&roomState, &participantLimit, &participantCount, &tfnStatus, &tfnNumber); err != nil {
		return nil, err
	}
	if roomState != "RUNNING" {
		return nil, errors.New("start the room before using the live dialer")
	}
	if participantCount >= participantLimit {
		return nil, fmt.Errorf("room participant limit reached (%d)", participantLimit)
	}
	if tfnStatus != "ACTIVE" || strings.TrimSpace(tfnNumber) == "" {
		return nil, errors.New("room has no active TFN assigned")
	}
	var participantID string
	if err := a.db.QueryRowContext(ctx, `INSERT INTO participants(workspace_id,bridge_id,display_name,phone_number,role,desired_state) VALUES($1,$2,$3,$4,'LISTENER','MUTED') RETURNING id::text`, workspaceID, bridgeID, coalesce(name, phone), phone).Scan(&participantID); err != nil {
		return nil, err
	}
	return a.dispatchExistingParticipant(ctx, workspaceID, bridgeID, participantID, "")
}

func (a *api) dial(w http.ResponseWriter, r *http.Request, workspaceID, bridgeID string) {
	var in struct {
		Name   string `json:"name"`
		Phone  string `json:"phone_number"`
		Region string `json:"region"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil || in.Phone == "" {
		a.json(w, 400, map[string]string{"error": "phone_number required"})
		return
	}
	if strings.TrimSpace(in.Region) != "" {
		in.Phone = normalizeDialNumber(in.Phone, in.Region)
		if in.Phone == "" {
			a.json(w, http.StatusBadRequest, map[string]string{"error": "phone_number is invalid"})
			return
		}
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
		if len(parts) == 1 && r.Method == http.MethodPut {
			if _, _, ok := a.requireWorkspaceRole(w, r, "OWNER", "ADMIN"); !ok {
				return
			}
			a.editRosterParticipant(w, r, parts[0])
			return
		}
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
	if action == "remove" {
		if _, _, ok := a.requireWorkspaceRole(w, r, "OWNER", "ADMIN"); !ok {
			return
		}
		var active int
		if err := a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM call_legs WHERE participant_id=$1 AND workspace_id=$2 AND state NOT IN ('ENDED','FAILED','DROPPED')`, pid, workspaceID).Scan(&active); err != nil {
			a.json(w, 500, map[string]string{"error": err.Error()})
			return
		}
		if active > 0 {
			a.json(w, http.StatusConflict, map[string]string{"error": "drop the active call before removing the roster participant"})
			return
		}
		result, err := a.db.ExecContext(r.Context(), `DELETE FROM participants WHERE id=$1 AND workspace_id=$2 AND role <> 'HOST'`, pid, workspaceID)
		if err != nil {
			a.json(w, 409, map[string]string{"error": err.Error()})
			return
		}
		count, _ := result.RowsAffected()
		if count == 0 {
			a.json(w, 404, map[string]string{"error": "roster participant not found"})
			return
		}
		a.json(w, 200, map[string]any{"status": "removed", "participant_id": pid})
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
	var bridgeID string
	if err := a.db.QueryRowContext(r.Context(), `SELECT bridge_id::text FROM participants WHERE id=$1 AND workspace_id=$2`, participantID, workspaceID).Scan(&bridgeID); err != nil {
		a.json(w, 404, map[string]string{"error": "participant not found"})
		return
	}
	var roomState string
	if err := a.db.QueryRowContext(r.Context(), `SELECT room_state FROM bridges WHERE id=$1 AND workspace_id=$2`, bridgeID, workspaceID).Scan(&roomState); err != nil || roomState != "RUNNING" {
		a.json(w, http.StatusConflict, map[string]string{"error": "start the room before adding a participant call"})
		return
	}
	result, err := a.dispatchExistingParticipant(r.Context(), workspaceID, bridgeID, participantID, "")
	if err != nil {
		a.json(w, 502, map[string]string{"error": err.Error()})
		return
	}
	a.json(w, 202, result)
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
	var roomState, existingHostName, existingHostPhone string
	var participantLimit, existingParticipantCount int
	if err := a.db.QueryRowContext(r.Context(), `SELECT b.room_state,COALESCE(b.host_name,''),COALESCE(b.host_phone,''),b.participant_limit,COUNT(p.id) FROM bridges b LEFT JOIN participants p ON p.bridge_id=b.id AND p.workspace_id=b.workspace_id WHERE b.id=$1 AND b.workspace_id=$2 GROUP BY b.id`, bridgeID, workspaceID).Scan(&roomState, &existingHostName, &existingHostPhone, &participantLimit, &existingParticipantCount); err != nil {
		a.json(w, 404, map[string]string{"error": "room not found"})
		return
	}
	if roomState == "RUNNING" || roomState == "STARTING" {
		a.json(w, http.StatusConflict, map[string]string{"error": "stop the room before importing its fixed roster"})
		return
	}
	hostCount := 0
	for _, contact := range in.Contacts {
		if normalizeParticipantRole(contact.Role) == "HOST" {
			hostCount++
		}
		if strings.TrimSpace(contact.Phone) == "" || strings.TrimSpace(contact.Name) == "" {
			a.json(w, 400, map[string]string{"error": "every roster row requires name and phone_number"})
			return
		}
	}
	if hostCount > 1 {
		a.json(w, 400, map[string]string{"error": "bulk upload may contain at most one HOST row"})
		return
	}
	if hostCount == 1 && existingHostPhone != "" {
		a.json(w, http.StatusConflict, map[string]string{"error": "this room already has a host; edit the host in Room settings before importing another one"})
		return
	}
	if participantLimit <= 0 || existingParticipantCount+len(in.Contacts) > participantLimit {
		a.json(w, http.StatusConflict, map[string]any{"error": "bulk roster exceeds the room participant limit", "participant_limit": participantLimit, "existing_participants": existingParticipantCount, "requested": len(in.Contacts)})
		return
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		a.json(w, 500, map[string]string{"error": err.Error()})
		return
	}
	defer tx.Rollback()
	var batchID string
	if err := tx.QueryRowContext(r.Context(), `INSERT INTO dial_batches(workspace_id,bridge_id,status,concurrency,rate_per_second) VALUES($1,$2,'IMPORTED',$3,$4) RETURNING id::text`, workspaceID, bridgeID, maxInt(in.Concurrency, 16), maxInt(in.Rate, 8)).Scan(&batchID); err != nil {
		a.json(w, 500, map[string]string{"error": err.Error()})
		return
	}
	count := 0
	for _, contact := range in.Contacts {
		name, phone := strings.TrimSpace(contact.Name), strings.TrimSpace(contact.Phone)
		role := normalizeParticipantRole(contact.Role)
		var participantID string
		if role == "HOST" {
			if err := tx.QueryRowContext(r.Context(), `UPDATE bridges SET host_name=$1,host_phone=$2,updated_at=now() WHERE id=$3 AND workspace_id=$4 RETURNING id::text`, name, phone, bridgeID, workspaceID).Scan(new(string)); err != nil {
				a.json(w, 500, map[string]string{"error": err.Error()})
				return
			}
			if err := tx.QueryRowContext(r.Context(), `INSERT INTO participants(workspace_id,bridge_id,display_name,phone_number,role,desired_state) VALUES($1,$2,$3,$4,'HOST','ACTIVE') RETURNING id::text`, workspaceID, bridgeID, name, phone).Scan(&participantID); err != nil {
				a.json(w, 500, map[string]string{"error": err.Error()})
				return
			}
		} else if err := tx.QueryRowContext(r.Context(), `INSERT INTO participants(workspace_id,bridge_id,display_name,phone_number,role,desired_state) VALUES($1,$2,$3,$4,'LISTENER','MUTED') RETURNING id::text`, workspaceID, bridgeID, name, phone).Scan(&participantID); err != nil {
			a.json(w, 500, map[string]string{"error": err.Error()})
			return
		}
		if _, err := tx.ExecContext(r.Context(), `INSERT INTO dial_batch_items(workspace_id,batch_id,name,phone_number,status,participant_id) VALUES($1,$2,$3,$4,'ROSTERED',$5)`, workspaceID, batchID, name, phone, participantID); err != nil {
			a.json(w, 500, map[string]string{"error": err.Error()})
			return
		}
		count++
	}
	if err := tx.Commit(); err != nil {
		a.json(w, 500, map[string]string{"error": err.Error()})
		return
	}
	a.json(w, 201, map[string]any{"batch_id": batchID, "status": "IMPORTED", "count": count, "message": "fixed roster imported; start the room to call the host first, then listeners"})
}

func maxInt(value, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
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

func slugify(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
		} else if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func canonicalE164(value string) string {
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, strings.TrimSpace(value))
	if len(digits) < 7 || len(digits) > 15 || !strings.HasPrefix(strings.TrimSpace(value), "+") {
		return ""
	}
	return "+" + digits
}

func (a *api) admin(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/admin"), "/")
	parts := strings.Split(path, "/")
	if path == "overview" && r.Method == http.MethodGet {
		a.adminOverview(w, r)
		return
	}
	if path == "users" {
		if r.Method == http.MethodGet {
			a.adminUsers(w, r)
			return
		}
		if r.Method == http.MethodPost {
			a.adminCreateUser(w, r)
			return
		}
	}
	if len(parts) >= 1 && parts[0] == "users" && len(parts) == 2 && r.Method == http.MethodPut {
		a.adminUpdateUser(w, r, parts[1])
		return
	}
	if path == "workspaces" {
		if r.Method == http.MethodGet {
			a.adminWorkspaces(w, r)
			return
		}
		if r.Method == http.MethodPost {
			a.adminCreateWorkspace(w, r)
			return
		}
	}
	if len(parts) >= 2 && parts[0] == "workspaces" {
		workspaceID := parts[1]
		if len(parts) == 2 && r.Method == http.MethodPut {
			a.adminUpdateWorkspace(w, r, workspaceID)
			return
		}
		if len(parts) == 3 && parts[2] == "members" {
			if r.Method == http.MethodGet {
				a.adminMembers(w, r, workspaceID)
				return
			}
			if r.Method == http.MethodPost {
				a.adminAddMember(w, r, workspaceID)
				return
			}
		}
		if len(parts) == 4 && parts[2] == "members" && r.Method == http.MethodDelete {
			a.adminRemoveMember(w, r, workspaceID, parts[3])
			return
		}
	}
	if path == "rooms" && r.Method == http.MethodGet {
		a.adminRooms(w, r)
		return
	}
	if path == "rooms" && r.Method == http.MethodPost {
		a.adminCreateRoom(w, r)
		return
	}
	if len(parts) == 2 && parts[0] == "rooms" && r.Method == http.MethodPut {
		a.adminUpdateRoom(w, r, parts[1])
		return
	}
	if path == "tfns" && r.Method == http.MethodGet {
		a.adminTFNs(w, r)
		return
	}
	if path == "tfns" && r.Method == http.MethodPost {
		a.adminCreateTFN(w, r)
		return
	}
	if len(parts) == 2 && parts[0] == "tfns" && r.Method == http.MethodDelete {
		a.adminDeleteTFN(w, r, parts[1])
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func (a *api) adminOverview(w http.ResponseWriter, r *http.Request) {
	workspaceID := strings.TrimSpace(r.URL.Query().Get("workspace_id"))
	var workspaces, rooms, users, tfns int
	if workspaceID != "" {
		var exists bool
		if err := a.db.QueryRowContext(r.Context(), `SELECT EXISTS(SELECT 1 FROM workspaces WHERE id=$1)`, workspaceID).Scan(&exists); err != nil || !exists {
			a.json(w, 404, map[string]string{"error": "workspace not found"})
			return
		}
		_ = a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM workspaces WHERE id=$1 AND status='ACTIVE'`, workspaceID).Scan(&workspaces)
		_ = a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM bridges WHERE workspace_id=$1 AND status='ACTIVE'`, workspaceID).Scan(&rooms)
		_ = a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM workspace_members WHERE workspace_id=$1`, workspaceID).Scan(&users)
		_ = a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM telephony_numbers WHERE workspace_id=$1 AND status='ACTIVE'`, workspaceID).Scan(&tfns)
		a.json(w, http.StatusOK, map[string]any{"workspace_id": workspaceID, "workspaces": workspaces, "rooms": rooms, "users": users, "tfns": tfns})
		return
	}
	_ = a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM workspaces WHERE status='ACTIVE'`).Scan(&workspaces)
	_ = a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM bridges WHERE status='ACTIVE'`).Scan(&rooms)
	_ = a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM operators WHERE status='ACTIVE'`).Scan(&users)
	_ = a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM telephony_numbers WHERE status='ACTIVE'`).Scan(&tfns)
	a.json(w, http.StatusOK, map[string]any{"workspaces": workspaces, "rooms": rooms, "users": users, "tfns": tfns})
}

func (a *api) adminWorkspaces(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.QueryContext(r.Context(), `SELECT w.id::text,w.slug,w.name,w.status,w.max_participants,COUNT(DISTINCT b.id),COUNT(DISTINCT t.id) FROM workspaces w LEFT JOIN bridges b ON b.workspace_id=w.id LEFT JOIN telephony_numbers t ON t.workspace_id=w.id GROUP BY w.id ORDER BY w.created_at DESC`)
	if err != nil {
		a.json(w, 500, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, slug, name, status string
		var maxParticipants, roomCount, tfnCount int
		if rows.Scan(&id, &slug, &name, &status, &maxParticipants, &roomCount, &tfnCount) == nil {
			out = append(out, map[string]any{"id": id, "slug": slug, "name": name, "status": status, "max_participants": maxParticipants, "room_count": roomCount, "tfn_count": tfnCount})
		}
	}
	a.json(w, http.StatusOK, out)
}

func (a *api) adminCreateWorkspace(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name            string `json:"name"`
		Slug            string `json:"slug"`
		MaxParticipants int    `json:"max_participants"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil || strings.TrimSpace(in.Name) == "" {
		a.json(w, 400, map[string]string{"error": "workspace name required"})
		return
	}
	if in.MaxParticipants <= 0 {
		in.MaxParticipants = 500
	}
	if in.MaxParticipants > 100000 {
		a.json(w, 400, map[string]string{"error": "max_participants must be between 1 and 100000"})
		return
	}
	slug := slugify(in.Slug)
	if slug == "" {
		slug = slugify(in.Name)
	}
	var id string
	err := a.db.QueryRowContext(r.Context(), `INSERT INTO workspaces(slug,name,max_participants) VALUES($1,$2,$3) RETURNING id::text`, slug, strings.TrimSpace(in.Name), in.MaxParticipants).Scan(&id)
	if err != nil {
		a.json(w, http.StatusConflict, map[string]string{"error": "workspace slug already exists or limit is invalid"})
		return
	}
	a.json(w, http.StatusCreated, map[string]any{"id": id, "slug": slug, "name": strings.TrimSpace(in.Name), "max_participants": in.MaxParticipants, "status": "ACTIVE"})
}

func (a *api) adminUpdateWorkspace(w http.ResponseWriter, r *http.Request, workspaceID string) {
	var in struct {
		Name            string `json:"name"`
		Status          string `json:"status"`
		MaxParticipants int    `json:"max_participants"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil || strings.TrimSpace(in.Name) == "" || in.MaxParticipants <= 0 {
		a.json(w, 400, map[string]string{"error": "name and positive max_participants required"})
		return
	}
	if in.Status != "SUSPENDED" {
		in.Status = "ACTIVE"
	}
	result, err := a.db.ExecContext(r.Context(), `UPDATE workspaces SET name=$1,status=$2,max_participants=$3,updated_at=now() WHERE id=$4`, strings.TrimSpace(in.Name), in.Status, in.MaxParticipants, workspaceID)
	if err != nil {
		a.json(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		a.json(w, 404, map[string]string{"error": "workspace not found"})
		return
	}
	a.json(w, 200, map[string]any{"status": "saved", "workspace_id": workspaceID})
}

func (a *api) adminRooms(w http.ResponseWriter, r *http.Request) {
	workspaceID := strings.TrimSpace(r.URL.Query().Get("workspace_id"))
	if workspaceID == "" {
		a.json(w, 400, map[string]string{"error": "workspace_id is required"})
		return
	}
	rows, err := a.db.QueryContext(r.Context(), `SELECT b.id::text,b.workspace_id::text,w.name,b.name,b.status,b.room_state,b.participant_limit,COALESCE(t.id::text,''),COALESCE(t.e164_number,''),COALESCE(t.label,'') FROM bridges b JOIN workspaces w ON w.id=b.workspace_id LEFT JOIN telephony_numbers t ON t.id=b.tfn_id WHERE b.workspace_id=$1 ORDER BY b.created_at DESC`, workspaceID)
	if err != nil {
		a.json(w, 500, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, workspaceID, workspaceName, name, status, roomState, tfnID, tfnNumber, tfnLabel string
		var limit int
		if rows.Scan(&id, &workspaceID, &workspaceName, &name, &status, &roomState, &limit, &tfnID, &tfnNumber, &tfnLabel) == nil {
			out = append(out, map[string]any{"id": id, "workspace_id": workspaceID, "workspace_name": workspaceName, "name": name, "status": status, "room_state": roomState, "participant_limit": limit, "tfn_id": tfnID, "tfn_number": tfnNumber, "tfn_label": tfnLabel})
		}
	}
	a.json(w, http.StatusOK, out)
}

func (a *api) adminCreateRoom(w http.ResponseWriter, r *http.Request) {
	var in struct {
		WorkspaceID      string `json:"workspace_id"`
		Name             string `json:"name"`
		ParticipantLimit int    `json:"participant_limit"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil || strings.TrimSpace(in.WorkspaceID) == "" || strings.TrimSpace(in.Name) == "" {
		a.json(w, 400, map[string]string{"error": "workspace_id and room name required"})
		return
	}
	if in.ParticipantLimit <= 0 {
		_ = a.db.QueryRowContext(r.Context(), `SELECT max_participants FROM workspaces WHERE id=$1`, in.WorkspaceID).Scan(&in.ParticipantLimit)
	}
	if in.ParticipantLimit <= 0 || in.ParticipantLimit > 100000 {
		a.json(w, 400, map[string]string{"error": "participant_limit must be between 1 and 100000"})
		return
	}
	var id string
	err := a.db.QueryRowContext(r.Context(), `INSERT INTO bridges(workspace_id,name,participant_limit) VALUES($1,$2,$3) RETURNING id::text`, in.WorkspaceID, strings.TrimSpace(in.Name), in.ParticipantLimit).Scan(&id)
	if err != nil {
		a.json(w, http.StatusConflict, map[string]string{"error": "room could not be created; check workspace, name, and participant limit"})
		return
	}
	a.json(w, http.StatusCreated, map[string]any{"id": id, "workspace_id": in.WorkspaceID, "name": strings.TrimSpace(in.Name), "participant_limit": in.ParticipantLimit, "room_state": "READY"})
}

func (a *api) adminUpdateRoom(w http.ResponseWriter, r *http.Request, roomID string) {
	var in struct {
		Name             string  `json:"name"`
		ParticipantLimit int     `json:"participant_limit"`
		TFNID            *string `json:"tfn_id"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil || strings.TrimSpace(in.Name) == "" || in.ParticipantLimit <= 0 {
		a.json(w, 400, map[string]string{"error": "name and positive participant_limit required"})
		return
	}
	adminWorkspaceID := strings.TrimSpace(r.URL.Query().Get("workspace_id"))
	if adminWorkspaceID == "" {
		a.json(w, 400, map[string]string{"error": "workspace_id is required"})
		return
	}
	var workspaceID, roomState string
	if err := a.db.QueryRowContext(r.Context(), `SELECT workspace_id::text,room_state FROM bridges WHERE id=$1 AND workspace_id=$2`, roomID, adminWorkspaceID).Scan(&workspaceID, &roomState); err != nil {
		a.json(w, 404, map[string]string{"error": "room not found"})
		return
	}
	if roomState == "RUNNING" || roomState == "STARTING" {
		a.json(w, http.StatusConflict, map[string]string{"error": "stop the room before changing its limit or TFN"})
		return
	}
	var currentCount int
	_ = a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM participants WHERE bridge_id=$1`, roomID).Scan(&currentCount)
	if in.ParticipantLimit < currentCount {
		a.json(w, http.StatusConflict, map[string]string{"error": "participant_limit cannot be below the current roster count"})
		return
	}
	tfnID := ""
	if in.TFNID != nil {
		tfnID = strings.TrimSpace(*in.TFNID)
		if tfnID != "" {
			var tfnWorkspace, tfnStatus string
			if err := a.db.QueryRowContext(r.Context(), `SELECT workspace_id::text,status FROM telephony_numbers WHERE id=$1`, tfnID).Scan(&tfnWorkspace, &tfnStatus); err != nil || tfnWorkspace != workspaceID || tfnStatus != "ACTIVE" {
				a.json(w, http.StatusConflict, map[string]string{"error": "TFN is not active or does not belong to this workspace"})
				return
			}
		}
	}
	// tfn_id is UUID. Keep the empty selection as SQL NULL instead of allowing
	// PostgreSQL to attempt an empty-string UUID cast.
	_, err := a.db.ExecContext(r.Context(), `UPDATE bridges SET name=$1,participant_limit=$2,tfn_id=CASE WHEN $3='' THEN NULL ELSE $3::uuid END,updated_at=now() WHERE id=$4`, strings.TrimSpace(in.Name), in.ParticipantLimit, tfnID, roomID)
	if err != nil {
		a.json(w, http.StatusConflict, map[string]string{"error": "room update failed; TFN may already be assigned to another room"})
		return
	}
	a.json(w, 200, map[string]any{"status": "saved", "room_id": roomID})
}

func (a *api) adminTFNs(w http.ResponseWriter, r *http.Request) {
	workspaceID := strings.TrimSpace(r.URL.Query().Get("workspace_id"))
	if workspaceID == "" {
		a.json(w, 400, map[string]string{"error": "workspace_id is required"})
		return
	}
	rows, err := a.db.QueryContext(r.Context(), `SELECT t.id::text,t.workspace_id::text,w.name,t.e164_number,t.label,t.provider_ref,t.status,COALESCE(b.id::text,''),COALESCE(b.name,'') FROM telephony_numbers t JOIN workspaces w ON w.id=t.workspace_id LEFT JOIN bridges b ON b.tfn_id=t.id WHERE t.workspace_id=$1 ORDER BY t.created_at DESC`, workspaceID)
	if err != nil {
		a.json(w, 500, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, workspaceID, workspaceName, number, label, providerRef, status, roomID, roomName string
		if rows.Scan(&id, &workspaceID, &workspaceName, &number, &label, &providerRef, &status, &roomID, &roomName) == nil {
			out = append(out, map[string]any{"id": id, "workspace_id": workspaceID, "workspace_name": workspaceName, "number": number, "label": label, "provider_ref": providerRef, "status": status, "room_id": roomID, "room_name": roomName})
		}
	}
	a.json(w, http.StatusOK, out)
}

func (a *api) adminCreateTFN(w http.ResponseWriter, r *http.Request) {
	var in struct {
		WorkspaceID string `json:"workspace_id"`
		Number      string `json:"number"`
		Label       string `json:"label"`
		ProviderRef string `json:"provider_ref"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil || strings.TrimSpace(in.WorkspaceID) == "" {
		a.json(w, 400, map[string]string{"error": "workspace_id required"})
		return
	}
	number := canonicalE164(in.Number)
	if number == "" {
		a.json(w, 400, map[string]string{"error": "number must be an E.164 value such as +919876543210"})
		return
	}
	var id string
	err := a.db.QueryRowContext(r.Context(), `INSERT INTO telephony_numbers(workspace_id,e164_number,label,provider_ref) VALUES($1,$2,$3,$4) RETURNING id::text`, in.WorkspaceID, number, strings.TrimSpace(in.Label), strings.TrimSpace(in.ProviderRef)).Scan(&id)
	if err != nil {
		a.json(w, http.StatusConflict, map[string]string{"error": "TFN already exists or workspace is invalid"})
		return
	}
	a.json(w, http.StatusCreated, map[string]any{"id": id, "workspace_id": in.WorkspaceID, "number": number, "label": in.Label, "status": "ACTIVE"})
}

func (a *api) adminDeleteTFN(w http.ResponseWriter, r *http.Request, tfnID string) {
	workspaceID := strings.TrimSpace(r.URL.Query().Get("workspace_id"))
	if workspaceID == "" {
		a.json(w, 400, map[string]string{"error": "workspace_id is required"})
		return
	}
	result, err := a.db.ExecContext(r.Context(), `DELETE FROM telephony_numbers WHERE id=$1 AND workspace_id=$2 AND NOT EXISTS (SELECT 1 FROM bridges WHERE tfn_id=$1)`, tfnID, workspaceID)
	if err != nil {
		a.json(w, 409, map[string]string{"error": "TFN could not be removed"})
		return
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		a.json(w, http.StatusConflict, map[string]string{"error": "TFN is assigned to a room or does not exist"})
		return
	}
	a.json(w, 200, map[string]string{"status": "deleted"})
}

func (a *api) adminMembers(w http.ResponseWriter, r *http.Request, workspaceID string) {
	rows, err := a.db.QueryContext(r.Context(), `SELECT o.id::text,o.username,COALESCE(o.display_name,''),o.platform_role,o.status,wm.role FROM workspace_members wm JOIN operators o ON o.id=wm.operator_id WHERE wm.workspace_id=$1 ORDER BY o.username`, workspaceID)
	if err != nil {
		a.json(w, 500, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, username, displayName, platformRole, status, role string
		if rows.Scan(&id, &username, &displayName, &platformRole, &status, &role) == nil {
			out = append(out, map[string]any{"operator_id": id, "username": username, "display_name": displayName, "platform_role": platformRole, "status": status, "role": role})
		}
	}
	a.json(w, 200, out)
}

func (a *api) adminAddMember(w http.ResponseWriter, r *http.Request, workspaceID string) {
	var in struct {
		OperatorID string `json:"operator_id"`
		Role       string `json:"role"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil || strings.TrimSpace(in.OperatorID) == "" {
		a.json(w, 400, map[string]string{"error": "operator_id required"})
		return
	}
	if in.Role != "OWNER" && in.Role != "ADMIN" && in.Role != "VIEWER" {
		in.Role = "OPERATOR"
	}
	_, err := a.db.ExecContext(r.Context(), `INSERT INTO workspace_members(workspace_id,operator_id,role) VALUES($1,$2,$3) ON CONFLICT(workspace_id,operator_id) DO UPDATE SET role=EXCLUDED.role`, workspaceID, in.OperatorID, in.Role)
	if err != nil {
		a.json(w, 409, map[string]string{"error": "workspace member could not be saved"})
		return
	}
	a.json(w, 200, map[string]string{"status": "saved"})
}

func (a *api) adminRemoveMember(w http.ResponseWriter, r *http.Request, workspaceID, operatorID string) {
	result, err := a.db.ExecContext(r.Context(), `DELETE FROM workspace_members WHERE workspace_id=$1 AND operator_id=$2 AND role <> 'OWNER'`, workspaceID, operatorID)
	if err != nil {
		a.json(w, 409, map[string]string{"error": "member could not be removed"})
		return
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		a.json(w, 409, map[string]string{"error": "owner membership cannot be removed"})
		return
	}
	a.json(w, 200, map[string]string{"status": "removed"})
}

func (a *api) adminUsers(w http.ResponseWriter, r *http.Request) {
	workspaceID := strings.TrimSpace(r.URL.Query().Get("workspace_id"))
	if workspaceID == "" {
		a.json(w, 400, map[string]string{"error": "workspace_id is required"})
		return
	}
	rows, err := a.db.QueryContext(r.Context(), `SELECT o.id::text,o.username,COALESCE(o.display_name,''),o.platform_role,o.status,o.created_at FROM workspace_members wm JOIN operators o ON o.id=wm.operator_id WHERE wm.workspace_id=$1 ORDER BY o.created_at DESC`, workspaceID)
	if err != nil {
		a.json(w, 500, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, username, displayName, platformRole, status string
		var createdAt time.Time
		if rows.Scan(&id, &username, &displayName, &platformRole, &status, &createdAt) == nil {
			out = append(out, map[string]any{"id": id, "username": username, "display_name": displayName, "platform_role": platformRole, "status": status, "created_at": createdAt})
		}
	}
	a.json(w, 200, out)
}

func (a *api) adminCreateUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username      string `json:"username"`
		DisplayName   string `json:"display_name"`
		Password      string `json:"password"`
		PlatformRole  string `json:"platform_role"`
		WorkspaceID   string `json:"workspace_id"`
		WorkspaceRole string `json:"workspace_role"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil || strings.TrimSpace(in.Username) == "" || len(in.Password) < 8 || strings.TrimSpace(in.WorkspaceID) == "" {
		a.json(w, 400, map[string]string{"error": "workspace_id, username, and a password of at least 8 characters are required"})
		return
	}
	if in.PlatformRole != "PLATFORM_ADMIN" {
		in.PlatformRole = "WORKSPACE_OPERATOR"
	}
	if in.WorkspaceRole != "OWNER" && in.WorkspaceRole != "ADMIN" && in.WorkspaceRole != "VIEWER" {
		in.WorkspaceRole = "OPERATOR"
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil {
		a.json(w, 500, map[string]string{"error": err.Error()})
		return
	}
	var id string
	err = a.db.QueryRowContext(r.Context(), `INSERT INTO operators(username,password_hash,display_name,platform_role,status) VALUES($1,$2,$3,$4,'ACTIVE') RETURNING id::text`, strings.TrimSpace(in.Username), string(hash), strings.TrimSpace(in.DisplayName), in.PlatformRole).Scan(&id)
	if err != nil {
		a.json(w, http.StatusConflict, map[string]string{"error": "username already exists"})
		return
	}
	if in.WorkspaceID != "" {
		if _, err = a.db.ExecContext(r.Context(), `INSERT INTO workspace_members(workspace_id,operator_id,role) VALUES($1,$2,$3)`, in.WorkspaceID, id, in.WorkspaceRole); err != nil {
			_, _ = a.db.ExecContext(r.Context(), `DELETE FROM operators WHERE id=$1`, id)
			a.json(w, http.StatusConflict, map[string]string{"error": "user created but workspace membership was invalid"})
			return
		}
	}
	a.json(w, http.StatusCreated, map[string]any{"id": id, "username": in.Username, "display_name": in.DisplayName, "platform_role": in.PlatformRole, "status": "ACTIVE"})
}

func (a *api) adminUpdateUser(w http.ResponseWriter, r *http.Request, operatorID string) {
	var in struct {
		DisplayName  string `json:"display_name"`
		Status       string `json:"status"`
		PlatformRole string `json:"platform_role"`
		Password     string `json:"password"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		a.json(w, 400, map[string]string{"error": "invalid user payload"})
		return
	}
	if in.Status != "DISABLED" {
		in.Status = "ACTIVE"
	}
	if in.PlatformRole != "PLATFORM_ADMIN" && in.PlatformRole != "PLATFORM_OWNER" {
		in.PlatformRole = "WORKSPACE_OPERATOR"
	}
	workspaceID := strings.TrimSpace(r.URL.Query().Get("workspace_id"))
	if workspaceID == "" {
		a.json(w, 400, map[string]string{"error": "workspace_id is required"})
		return
	}
	var existingRole, existingStatus string
	if err := a.db.QueryRowContext(r.Context(), `SELECT o.platform_role,o.status FROM operators o JOIN workspace_members wm ON wm.operator_id=o.id WHERE o.id=$1 AND wm.workspace_id=$2`, operatorID, workspaceID).Scan(&existingRole, &existingStatus); err != nil {
		a.json(w, 404, map[string]string{"error": "user not found"})
		return
	}
	current, currentOK := a.currentAdminSession(r)
	if currentOK && current.operatorID == operatorID && (in.Status != "ACTIVE" || (in.PlatformRole != "PLATFORM_ADMIN" && in.PlatformRole != "PLATFORM_OWNER")) {
		a.json(w, http.StatusConflict, map[string]string{"error": "you cannot remove your own product administrator access"})
		return
	}
	existingAdmin := existingRole == "PLATFORM_OWNER" || existingRole == "PLATFORM_ADMIN"
	remainingAdmin := in.PlatformRole == "PLATFORM_OWNER" || in.PlatformRole == "PLATFORM_ADMIN"
	if existingAdmin && (!remainingAdmin || in.Status != "ACTIVE") {
		var activeAdmins int
		_ = a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM operators WHERE status='ACTIVE' AND platform_role IN ('PLATFORM_OWNER','PLATFORM_ADMIN')`).Scan(&activeAdmins)
		if activeAdmins <= 1 {
			a.json(w, http.StatusConflict, map[string]string{"error": "at least one active product administrator must remain"})
			return
		}
	}
	if strings.TrimSpace(in.Password) != "" {
		if len(in.Password) < 8 {
			a.json(w, 400, map[string]string{"error": "password must be at least 8 characters"})
			return
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
		if err != nil {
			a.json(w, 500, map[string]string{"error": err.Error()})
			return
		}
		_, err = a.db.ExecContext(r.Context(), `UPDATE operators SET display_name=$1,status=$2,platform_role=$3,password_hash=$4 WHERE id=$5`, strings.TrimSpace(in.DisplayName), in.Status, in.PlatformRole, string(hash), operatorID)
		if err != nil {
			a.json(w, 409, map[string]string{"error": err.Error()})
			return
		}
	} else if _, err := a.db.ExecContext(r.Context(), `UPDATE operators SET display_name=$1,status=$2,platform_role=$3 WHERE id=$4`, strings.TrimSpace(in.DisplayName), in.Status, in.PlatformRole, operatorID); err != nil {
		a.json(w, 409, map[string]string{"error": err.Error()})
		return
	}
	a.json(w, 200, map[string]string{"status": "saved"})
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
	workspaceID, _, ok := a.requireWorkspaceRole(w, r, "OWNER", "ADMIN", "OPERATOR")
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
		_, _ = a.db.ExecContext(r.Context(), `UPDATE room_session_participants SET call_state=$1,joined_at=CASE WHEN $1 IN ('ANSWERED','IN_BRIDGE') THEN COALESCE(joined_at,now()) ELSE joined_at END,left_at=CASE WHEN $1 IN ('ENDED','FAILED','DROPPED') THEN COALESCE(left_at,now()) ELSE left_at END WHERE call_leg_id=$2`, state, id)
		a.finishHostSession(r.Context(), id, state)
	}
	a.json(w, 202, map[string]string{"status": "accepted"})
}

func (a *api) finishHostSession(ctx context.Context, callID, state string) {
	if state != "ENDED" && state != "FAILED" && state != "DROPPED" {
		return
	}
	var sessionID, sessionStatus, bridgeID, workspaceID string
	err := a.db.QueryRowContext(ctx, `SELECT rs.id::text,rs.status,rs.bridge_id::text,rs.workspace_id::text FROM room_sessions rs JOIN room_session_participants rsp ON rsp.session_id=rs.id JOIN participants p ON p.id=rsp.participant_id WHERE rsp.call_leg_id=$1 AND p.role='HOST' AND rs.status IN ('STARTING','RUNNING') LIMIT 1`, callID).Scan(&sessionID, &sessionStatus, &bridgeID, &workspaceID)
	if err != nil {
		return
	}
	terminal := "ENDED"
	if sessionStatus == "STARTING" {
		terminal = "FAILED"
	}
	_, _ = a.db.ExecContext(ctx, `UPDATE room_sessions SET status=$1,ended_at=COALESCE(ended_at,now()),duration_seconds=EXTRACT(EPOCH FROM (COALESCE(ended_at,now())-started_at))::bigint WHERE id=$2 AND status IN ('STARTING','RUNNING')`, terminal, sessionID)
	_, _ = a.db.ExecContext(ctx, `UPDATE room_session_participants SET left_at=COALESCE(left_at,now()) WHERE session_id=$1 AND left_at IS NULL`, sessionID)
	_, _ = a.db.ExecContext(ctx, `UPDATE bridges SET room_state='READY',updated_at=now() WHERE id=$1 AND workspace_id=$2 AND room_state IN ('STARTING','RUNNING')`, bridgeID, workspaceID)
}

func ensureBootstrapAdmin(db *sql.DB) error {
	username := env("ADMIN_USERNAME", "admin")
	password := env("ADMIN_PASSWORD", "change-me")
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO operators(username,password_hash,display_name,platform_role,status) VALUES($1,$2,$3,'PLATFORM_OWNER','ACTIVE') ON CONFLICT(username) DO UPDATE SET display_name=EXCLUDED.display_name,platform_role='PLATFORM_OWNER',status='ACTIVE',password_hash=EXCLUDED.password_hash`, username, string(hash), username)
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO workspace_members(workspace_id,operator_id,role) SELECT id,(SELECT id FROM operators WHERE username=$1),'OWNER' FROM workspaces WHERE slug=$2 ON CONFLICT(workspace_id,operator_id) DO UPDATE SET role='OWNER'`, username, env("DEFAULT_WORKSPACE_SLUG", "operations"))
	return err
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
	if err := ensureBootstrapAdmin(db); err != nil {
		slog.Error("bootstrap product admin unavailable", "error", err)
		os.Exit(1)
	}
	rdb := redis.NewClient(&redis.Options{Addr: env("REDIS_ADDR", "127.0.0.1:6379")})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		slog.Warn("Redis unavailable; control API will use configured SipGo HTTP targets", "error", err)
	}
	a := &api{db: db, sipgoURLs: httpTargets(env("SIPGO_HTTP_TARGETS", env("SIPGO_HTTP", "http://127.0.0.1:8080"))), rdb: rdb, sessions: map[string]session{}, adminSessions: map[string]session{}}
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
	mux.HandleFunc("/api/v1/auth/me", a.require(a.me))
	mux.HandleFunc("/api/v1/auth/admin/login", a.adminLogin)
	mux.HandleFunc("/api/v1/auth/admin/logout", a.requirePlatformAdmin(a.adminLogout))
	mux.HandleFunc("/api/v1/auth/admin/me", a.requirePlatformAdmin(a.adminMe))
	mux.HandleFunc("/api/v1/workspace", a.require(a.workspace))
	mux.HandleFunc("/api/v1/workspace/members", a.require(a.workspaceMembers))
	mux.HandleFunc("/api/v1/workspace/members/", a.require(a.workspaceMember))
	mux.HandleFunc("/api/v1/bridges", a.require(a.bridges))
	mux.HandleFunc("/api/v1/bridges/", a.require(a.bridge))
	mux.HandleFunc("/api/v1/participants/", a.require(a.action))
	mux.HandleFunc("/api/v1/batches/", a.require(a.batchState))
	mux.HandleFunc("/api/v1/speaker-requests/", a.require(a.speakerRequestAction))
	mux.HandleFunc("/api/v1/admin/", a.requirePlatformAdmin(a.admin))
	mux.HandleFunc("/internal/v1/events", a.events)
	mux.HandleFunc("/internal/v1/dtmf", a.dtmf)
	addr := env("CONTROL_API_LISTEN", "127.0.0.1:8081")
	uiOrigin := strings.TrimRight(env("UI_ORIGIN", ""), "/")
	slog.Info("control API listening", "addr", addr)
	if err := http.ListenAndServe(addr, cors(uiOrigin, mux)); err != nil {
		slog.Error("control API", "error", err)
	}
}
