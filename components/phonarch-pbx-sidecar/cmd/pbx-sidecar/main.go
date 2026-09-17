package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
)

type config struct {
	NodeID         string
	PrivateIP      string
	MediaPublicIP  string
	RedisAddr      string
	SIPUDPPort     int
	SIPTCPPort     int
	ControlURL     string
	SIPGoTargets   []string
	EventURL       string
	RustPBXURL     string
	Listen         string
	Capacity       int
	Role           string
	DrainState     string
	HeartbeatEvery time.Duration
}

func getenv(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}
func getenvInt(k string, fallback int) int {
	v, _ := strconv.Atoi(getenv(k, strconv.Itoa(fallback)))
	if v <= 0 {
		return fallback
	}
	return v
}
func getenvDuration(k string, fallback time.Duration) time.Duration {
	d, err := time.ParseDuration(getenv(k, fallback.String()))
	if err != nil {
		return fallback
	}
	return d
}

func splitTargets(value string) []string {
	seen := map[string]struct{}{}
	var targets []string
	for _, raw := range strings.Split(value, ",") {
		target := strings.TrimSpace(raw)
		if target == "" {
			continue
		}
		if _, exists := seen[target]; exists {
			continue
		}
		seen[target] = struct{}{}
		targets = append(targets, target)
	}
	return targets
}

func loadConfig() config {
	privateIP := getenv("PRIVATE_IP", "127.0.0.1")
	id := getenv("NODE_ID", "")
	if id == "" {
		id = "core-" + strings.NewReplacer(".", "-", ":", "-").Replace(privateIP)
	}
	heartbeatTargets := splitTargets(getenv("SIDECAR_HEARTBEAT_TARGETS", getenv("SIDECAR_HEARTBEAT_TARGET", "")))
	return config{
		NodeID: id, PrivateIP: privateIP, MediaPublicIP: getenv("MEDIA_PUBLIC_IP", ""), RedisAddr: getenv("REDIS_ADDR", "127.0.0.1:6379"), SIPUDPPort: getenvInt("SIP_UDP_PORT", 5060), SIPTCPPort: getenvInt("SIP_TCP_PORT", 5060),
		ControlURL: getenv("SIDECAR_CONTROL_URL", "http://127.0.0.1:9443"), SIPGoTargets: heartbeatTargets, EventURL: getenv("SIDECAR_EVENT_URL", "http://127.0.0.1:8081/internal/v1/events"), RustPBXURL: getenv("RUSTPBX_CONTROL_URL", "http://127.0.0.1:9090"), Listen: getenv("SIDECAR_LISTEN", "127.0.0.1:9443"), Capacity: getenvInt("NODE_CAPACITY", 500), Role: getenv("NODE_ROLE", "listener"), DrainState: getenv("NODE_DRAIN_STATE", "READY"), HeartbeatEvery: getenvDuration("SIDECAR_HEARTBEAT_INTERVAL", 2*time.Second),
	}
}

type sipgoTarget struct {
	PrivateIP  string `json:"private_ip"`
	SIPUDPPort int    `json:"sip_udp_port"`
}

func (s *server) discoverSIPGoTargets(ctx context.Context) []string {
	if s.rdb == nil {
		return s.cfg.SIPGoTargets
	}
	seen := map[string]struct{}{}
	var targets []string
	iterator := s.rdb.Scan(ctx, 0, "active-sipgo:*", 0).Iterator()
	for iterator.Next(ctx) {
		body, err := s.rdb.Get(ctx, iterator.Val()).Bytes()
		if err != nil {
			continue
		}
		var target sipgoTarget
		if json.Unmarshal(body, &target) != nil || target.PrivateIP == "" || target.SIPUDPPort <= 0 {
			continue
		}
		address := fmt.Sprintf("%s:%d", target.PrivateIP, target.SIPUDPPort)
		if _, exists := seen[address]; !exists {
			seen[address] = struct{}{}
			targets = append(targets, address)
		}
	}
	if iterator.Err() != nil || len(targets) == 0 {
		return s.cfg.SIPGoTargets
	}
	return targets
}

type callState struct {
	CallID        string    `json:"call_id"`
	BridgeID      string    `json:"bridge_id"`
	ParticipantID string    `json:"participant_id"`
	PBXMemberID   string    `json:"pbx_member_id,omitempty"`
	Destination   string    `json:"destination"`
	State         string    `json:"state"`
	Muted         bool      `json:"muted"`
	VolumeDB      float64   `json:"volume_db,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type store struct {
	mu       sync.RWMutex
	calls    map[string]callState
	commands map[string]json.RawMessage
	active   atomic.Int64
	bridges  atomic.Int64
	epoch    int64
	instance string
}

func newStore() *store {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return &store{calls: map[string]callState{}, commands: map[string]json.RawMessage{}, epoch: time.Now().UnixNano(), instance: hex.EncodeToString(b[:])}
}
func (s *store) load() (int, int, float64) {
	calls := int(s.active.Load())
	cap := getenvInt("NODE_CAPACITY", 500)
	return calls, int(s.bridges.Load()), float64(calls) / float64(cap)
}

type rustPBXClient struct {
	base string
	http *http.Client
}

func (c *rustPBXClient) post(ctx context.Context, path string, request any, response any) error {
	b, _ := json.Marshal(request)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.base, "/")+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		return fmt.Errorf("rustpbx returned %s", res.Status)
	}
	if response != nil {
		return json.NewDecoder(res.Body).Decode(response)
	}
	return nil
}

func (c *rustPBXClient) get(ctx context.Context, path string, response any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.base, "/")+path, nil)
	if err != nil {
		return err
	}
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		return fmt.Errorf("rustpbx returned %s", res.Status)
	}
	return json.NewDecoder(res.Body).Decode(response)
}

type server struct {
	cfg   config
	state *store
	pbx   *rustPBXClient
	rdb   *redis.Client
}

func (s *server) event(ctx context.Context, event any) {
	b, _ := json.Marshal(event)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.EventURL, bytes.NewReader(b))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if token := os.Getenv("PHONARCH_INTERNAL_TOKEN"); token != "" {
		req.Header.Set("X-PhonArch-Internal-Token", token)
	}
	res, err := http.DefaultClient.Do(req)
	if err == nil {
		res.Body.Close()
	}
}
func (s *server) commandID(r *http.Request) string { return r.Header.Get("Idempotency-Key") }
func (s *server) write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *server) originate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		CommandID     string `json:"command_id"`
		CallID        string `json:"call_id"`
		BridgeID      string `json:"bridge_id"`
		ParticipantID string `json:"participant_id"`
		Destination   string `json:"destination"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil || in.CallID == "" || in.Destination == "" {
		s.write(w, 400, map[string]string{"error": "call_id and destination are required"})
		return
	}
	if in.CommandID == "" {
		in.CommandID = s.commandID(r)
	}
	if in.CommandID == "" {
		in.CommandID = in.CallID + ":originate"
	}
	s.state.mu.Lock()
	if result, ok := s.state.commands[in.CommandID]; ok {
		s.state.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(result)
		return
	}
	s.state.mu.Unlock()
	var out struct {
		PBXMemberID string `json:"pbx_member_id"`
	}
	if err := s.pbx.post(r.Context(), "/v1/calls/originate", in, &out); err != nil {
		s.write(w, 502, map[string]string{"error": err.Error()})
		return
	}
	call := callState{CallID: in.CallID, BridgeID: in.BridgeID, ParticipantID: in.ParticipantID, PBXMemberID: out.PBXMemberID, Destination: in.Destination, State: "DISPATCHED", UpdatedAt: time.Now()}
	s.state.mu.Lock()
	s.state.calls[in.CallID] = call
	s.state.commands[in.CommandID], _ = json.Marshal(call)
	s.state.mu.Unlock()
	s.state.active.Add(1)
	go s.event(context.Background(), map[string]any{"type": "call.dispatched", "node_id": s.cfg.NodeID, "node_epoch": s.state.epoch, "sidecar_instance_id": s.state.instance, "call": call})
	s.write(w, 202, call)
}

func (s *server) participantAction(action string, w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/participants/")
	id = strings.TrimSuffix(id, "/"+action)
	if id == "" {
		s.write(w, 400, map[string]string{"error": "participant id required"})
		return
	}
	var in struct {
		CommandID   string `json:"command_id"`
		CallID      string `json:"call_id"`
		PBXMemberID string `json:"pbx_member_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	if in.PBXMemberID == "" {
		s.state.mu.RLock()
		for _, call := range s.state.calls {
			if (in.CallID != "" && call.CallID == in.CallID) || (in.CallID == "" && call.ParticipantID == id) {
				in.CallID = call.CallID
				in.PBXMemberID = call.PBXMemberID
				break
			}
		}
		s.state.mu.RUnlock()
	}
	if in.CommandID == "" {
		in.CommandID = s.commandID(r)
	}
	if in.CommandID == "" {
		in.CommandID = id + ":" + action
	}
	s.state.mu.RLock()
	cached := s.state.commands[in.CommandID]
	s.state.mu.RUnlock()
	if cached != nil {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(cached)
		return
	}
	var out map[string]any
	if err := s.pbx.post(r.Context(), "/v1/participants/"+id+"/"+action, in, &out); err != nil {
		s.write(w, 502, map[string]string{"error": err.Error()})
		return
	}
	s.state.mu.Lock()
	for callID, call := range s.state.calls {
		if call.ParticipantID == id {
			if action == "mute" {
				call.Muted = true
			}
			if action == "unmute" {
				call.Muted = false
			}
			if action == "drop" {
				call.State = "DROPPED"
				s.state.active.Add(-1)
			}
			call.UpdatedAt = time.Now()
			s.state.calls[callID] = call
		}
	}
	encoded, _ := json.Marshal(out)
	s.state.commands[in.CommandID] = encoded
	s.state.mu.Unlock()
	go s.event(context.Background(), map[string]any{"type": "participant." + action, "node_id": s.cfg.NodeID, "node_epoch": s.state.epoch, "participant_id": id, "call_id": in.CallID, "result": out})
	s.write(w, 200, out)
}

// roomMediaAction is the stable control-plane boundary for the future Rust
// room mixer/fan-out engine. The sidecar does not implement media itself; it
// fences commands by node epoch and forwards them to the local engine.
func (s *server) roomMediaAction(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/rooms/"), "/"), "/")
	if len(parts) != 2 || parts[1] == "" {
		s.write(w, 400, map[string]string{"error": "room action required"})
		return
	}
	roomID, action := parts[0], parts[1]
	var in struct {
		CommandID     string `json:"command_id"`
		WorkspaceID   string `json:"workspace_id"`
		RoomID        string `json:"room_id"`
		RoomEpoch     int64  `json:"room_epoch"`
		ParticipantID string `json:"participant_id"`
		StreamID      string `json:"stream_id"`
		RootNodeID    string `json:"root_node_id"`
		Muted         bool   `json:"muted"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		s.write(w, 400, map[string]string{"error": "invalid JSON"})
		return
	}
	if in.RoomID == "" {
		in.RoomID = roomID
	}
	if in.CommandID == "" {
		in.CommandID = s.commandID(r)
	}
	if in.CommandID == "" {
		in.CommandID = s.cfg.NodeID + ":room:" + roomID + ":" + action
	}
	s.state.mu.RLock()
	if cached := s.state.commands[in.CommandID]; cached != nil {
		s.state.mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(cached)
		return
	}
	s.state.mu.RUnlock()
	var out map[string]any
	path := "/v1/rooms/" + roomID + "/media/" + action
	if err := s.pbx.post(r.Context(), path, in, &out); err != nil {
		s.write(w, 502, map[string]string{"error": err.Error()})
		return
	}
	if out == nil {
		out = map[string]any{"status": "accepted", "room_id": roomID, "action": action}
	}
	encoded, _ := json.Marshal(out)
	s.state.mu.Lock()
	s.state.commands[in.CommandID] = encoded
	s.state.mu.Unlock()
	go s.event(context.Background(), map[string]any{
		"type": "room.media." + action, "node_id": s.cfg.NodeID,
		"node_epoch": s.state.epoch, "workspace_id": in.WorkspaceID,
		"room_id": roomID, "participant_id": in.ParticipantID,
		"command_id": in.CommandID, "result": out,
	})
	s.write(w, 200, out)
}

func terminalState(state string) bool {
	return state == "ENDED" || state == "FAILED" || state == "DROPPED"
}

func (s *server) pollStates(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.state.mu.RLock()
			calls := make([]callState, 0, len(s.state.calls))
			for _, call := range s.state.calls {
				if !terminalState(call.State) {
					calls = append(calls, call)
				}
			}
			s.state.mu.RUnlock()
			for _, call := range calls {
				var remote struct {
					State       string  `json:"state"`
					PBXMemberID string  `json:"pbx_member_id"`
					Muted       bool    `json:"muted"`
					VolumeDB    float64 `json:"volume_db"`
				}
				if err := s.pbx.get(ctx, "/v1/calls/"+call.CallID, &remote); err != nil || remote.State == "" {
					continue
				}
				if call.State == remote.State && call.PBXMemberID == remote.PBXMemberID && call.Muted == remote.Muted && call.VolumeDB == remote.VolumeDB {
					continue
				}
				call.State = remote.State
				if remote.PBXMemberID != "" {
					call.PBXMemberID = remote.PBXMemberID
				}
				call.Muted = remote.Muted
				call.VolumeDB = remote.VolumeDB
				call.UpdatedAt = time.Now()
				s.state.mu.Lock()
				previous, exists := s.state.calls[call.CallID]
				s.state.calls[call.CallID] = call
				s.state.mu.Unlock()
				if exists && !terminalState(previous.State) && terminalState(call.State) {
					s.state.active.Add(-1)
				}
				go s.event(context.Background(), map[string]any{"type": "call.state", "node_id": s.cfg.NodeID, "node_epoch": s.state.epoch, "sidecar_instance_id": s.state.instance, "call": call})
			}
		}
	}
}

func (s *server) heartbeat(ctx context.Context) {
	seq := 1
	for {
		select {
		case <-ctx.Done():
			s.deregister(context.Background())
			return
		case <-time.After(s.cfg.HeartbeatEvery):
			for _, target := range s.discoverSIPGoTargets(ctx) {
				s.sendOptions(ctx, target, seq)
			}
			seq++
		}
	}
}
func (s *server) sendOptions(ctx context.Context, target string, seq int) {
	calls, bridges, load := s.state.load()
	msg := fmt.Sprintf("OPTIONS sip:phonarch-sipgo@%s SIP/2.0\r\nVia: SIP/2.0/UDP %s:%d;branch=z9hG4bK-%s-%d\r\nFrom: <sip:%s@phonarch>;tag=%s\r\nTo: <sip:phonarch-sipgo@phonarch>\r\nCall-ID: phonarch-heartbeat-%s\r\nCSeq: %d OPTIONS\r\nMax-Forwards: 1\r\nX-PhonArch-Node-ID: %s\r\nX-PhonArch-Private-IP: %s\r\nX-PhonArch-Media-Public-IP: %s\r\nX-PhonArch-Control-URL: %s\r\nX-PhonArch-SIP-UDP-Port: %d\r\nX-PhonArch-SIP-TCP-Port: %d\r\nX-PhonArch-Role: %s\r\nX-PhonArch-Drain-State: %s\r\nX-PhonArch-Capacity: %d\r\nX-PhonArch-Active-Calls: %d\r\nX-PhonArch-Active-RTP-Sessions: %d\r\nX-PhonArch-RTP-Packets-Per-Second: %d\r\nX-PhonArch-Active-Speakers: %d\r\nX-PhonArch-Active-Bridges: %d\r\nX-PhonArch-Load-Score: %.6f\r\nX-PhonArch-Node-Epoch: %d\r\nContent-Length: 0\r\n\r\n", target, s.cfg.PrivateIP, s.cfg.SIPUDPPort, s.state.instance, seq, s.cfg.NodeID, s.state.instance, s.cfg.NodeID, seq, s.cfg.NodeID, s.cfg.PrivateIP, s.cfg.MediaPublicIP, s.cfg.ControlURL, s.cfg.SIPUDPPort, s.cfg.SIPTCPPort, s.cfg.Role, s.cfg.DrainState, s.cfg.Capacity, calls, calls, calls*50, 0, bridges, load, s.state.epoch)
	addr, err := net.ResolveUDPAddr("udp", target)
	if err != nil {
		return
	}
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = conn.Write([]byte(msg))
	_ = ctx
}
func (s *server) deregister(ctx context.Context) {
	if s.rdb != nil {
		_ = s.rdb.Del(ctx, "active-pbx:"+s.cfg.NodeID).Err()
	}
}

func main() {
	cfg := loadConfig()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	rdb := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr})
	if err := rdb.Ping(ctx).Err(); err != nil {
		slog.Warn("Redis unavailable; sidecar will use configured heartbeat targets", "error", err)
	}
	s := &server{cfg: cfg, state: newStore(), pbx: &rustPBXClient{base: cfg.RustPBXURL, http: &http.Client{Timeout: 10 * time.Second}}, rdb: rdb}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		s.write(w, 200, map[string]any{"node_id": cfg.NodeID, "epoch": s.state.epoch, "instance": s.state.instance})
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { s.write(w, 200, map[string]string{"status": "ready"}) })
	mux.HandleFunc("/v1/calls/originate", s.originate)
	mux.HandleFunc("/v1/participants/", func(w http.ResponseWriter, r *http.Request) {
		for _, action := range []string{"mute", "unmute", "drop"} {
			if strings.HasSuffix(r.URL.Path, "/"+action) {
				s.participantAction(action, w, r)
				return
			}
		}
		s.write(w, 404, map[string]string{"error": "unknown action"})
	})
	mux.HandleFunc("/v1/rooms/", s.roomMediaAction)
	mux.HandleFunc("/v1/calls/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/v1/calls/")
		s.state.mu.RLock()
		call, ok := s.state.calls[id]
		s.state.mu.RUnlock()
		if !ok {
			s.write(w, 404, map[string]string{"error": "not found"})
			return
		}
		s.write(w, 200, call)
	})
	go s.heartbeat(ctx)
	go s.pollStates(ctx)
	srv := &http.Server{Addr: cfg.Listen, Handler: mux}
	go func() {
		slog.Info("sidecar listening", "node", cfg.NodeID, "addr", cfg.Listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("sidecar", "error", err)
		}
	}()
	<-ctx.Done()
	_ = srv.Shutdown(context.Background())
}
