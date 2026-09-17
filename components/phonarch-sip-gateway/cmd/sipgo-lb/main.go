package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/redis/go-redis/v9"
)

type node struct {
	NodeID              string  `json:"node_id"`
	PrivateIP           string  `json:"private_ip"`
	MediaPublicIP       string  `json:"media_public_ip"`
	SIPUDPPort          int     `json:"sip_udp_port"`
	SIPTCPPort          int     `json:"sip_tcp_port"`
	ControlURL          string  `json:"control_url"`
	Role                string  `json:"role"`
	DrainState          string  `json:"drain_state"`
	Capacity            int     `json:"capacity"`
	ActiveCalls         int     `json:"active_calls"`
	ActiveBridges       int     `json:"active_bridges"`
	ActiveRTP           int     `json:"active_rtp_sessions"`
	RTPPacketsPerSecond int64   `json:"rtp_packets_per_second"`
	ActiveSpeakers      int     `json:"active_speakers"`
	LoadScore           float64 `json:"load_score"`
	NodeEpoch           int64   `json:"node_epoch"`
	LastSeenUnix        int64   `json:"last_seen_unix"`
}

func (n node) destination() string { return fmt.Sprintf("%s:%d", n.PrivateIP, n.SIPUDPPort) }

type registry struct {
	rdb      *redis.Client
	ttl      time.Duration
	mu       sync.Mutex
	cursor   int
	affinity map[string]affinityEntry
}

type affinityEntry struct {
	CallID      string    `json:"call_id"`
	Destination string    `json:"destination"`
	FromTag     string    `json:"from_tag,omitempty"`
	ToTag       string    `json:"to_tag,omitempty"`
	NodeEpoch   int64     `json:"node_epoch,omitempty"`
	ExpiresAt   time.Time `json:"expires_at"`
}

type sipgoAdvertisement struct {
	NodeID       string `json:"node_id"`
	PrivateIP    string `json:"private_ip"`
	PublicIP     string `json:"public_ip,omitempty"`
	SIPUDPPort   int    `json:"sip_udp_port"`
	SIPTCPPort   int    `json:"sip_tcp_port"`
	HTTPURL      string `json:"http_url"`
	Role         string `json:"role"`
	NodeEpoch    int64  `json:"node_epoch"`
	LastSeenUnix int64  `json:"last_seen_unix"`
}

func newRegistry(rdb *redis.Client, ttl time.Duration) *registry {
	return &registry{rdb: rdb, ttl: ttl, affinity: make(map[string]affinityEntry)}
}

func (r *registry) upsert(ctx context.Context, n node) error {
	n.LastSeenUnix = time.Now().Unix()
	b, err := json.Marshal(n)
	if err != nil {
		return err
	}
	return r.rdb.Set(ctx, "active-pbx:"+n.NodeID, b, r.ttl).Err()
}

func (r *registry) remove(ctx context.Context, id string) error {
	return r.rdb.Del(ctx, "active-pbx:"+id).Err()
}

func (r *registry) list(ctx context.Context) ([]node, error) {
	var out []node
	iter := r.rdb.Scan(ctx, 0, "active-pbx:*", 0).Iterator()
	for iter.Next(ctx) {
		value, err := r.rdb.Get(ctx, iter.Val()).Bytes()
		if err != nil {
			continue
		}
		var n node
		if json.Unmarshal(value, &n) == nil && n.NodeID != "" && n.PrivateIP != "" && n.Capacity > 0 {
			out = append(out, n)
		}
	}
	return out, iter.Err()
}

func (r *registry) selectNode(ctx context.Context) (node, error) {
	nodes, err := r.list(ctx)
	if err != nil {
		return node{}, err
	}
	if len(nodes) == 0 {
		return node{}, errors.New("no live PBX nodes")
	}
	eligible := nodes[:0]
	for _, candidate := range nodes {
		if candidate.DrainState != "DRAINING" && candidate.DrainState != "DRAINED" && candidate.ActiveCalls < candidate.Capacity {
			eligible = append(eligible, candidate)
		}
	}
	nodes = eligible
	if len(nodes) == 0 {
		return node{}, errors.New("all PBX nodes are at capacity")
	}
	min := nodes[0].LoadScore
	for _, n := range nodes[1:] {
		if n.LoadScore < min {
			min = n.LoadScore
		}
	}
	const epsilon = 0.000001
	var candidates []node
	for _, n := range nodes {
		if n.LoadScore <= min+epsilon {
			candidates = append(candidates, n)
		}
	}
	r.mu.Lock()
	selected := candidates[r.cursor%len(candidates)]
	r.cursor++
	r.mu.Unlock()
	return selected, nil
}

func dialogTags(req *sip.Request) (string, string) {
	var fromTag, toTag string
	if h := req.From(); h != nil {
		fromTag, _ = h.Params.Get("tag")
	}
	if h := req.To(); h != nil {
		toTag, _ = h.Params.Get("tag")
	}
	return fromTag, toTag
}

func dialogKey(callID, fromTag, toTag string) string {
	if callID == "" {
		return ""
	}
	return "sip-dialog:" + callID + ":" + fromTag + ":" + toTag
}

func (r *registry) bind(ctx context.Context, req *sip.Request, destination string, nodeEpoch int64) error {
	callID := ""
	if h := req.CallID(); h != nil {
		callID = h.Value()
	}
	fromTag, toTag := dialogTags(req)
	entry := affinityEntry{CallID: callID, Destination: destination, FromTag: fromTag, ToTag: toTag, NodeEpoch: nodeEpoch, ExpiresAt: time.Now().Add(2 * time.Hour)}
	r.mu.Lock()
	r.affinity[callID] = entry
	r.mu.Unlock()
	b, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	// The base key lets a dialog survive a process restart before the final
	// To-tag is known. The tag-qualified key is written when available.
	pipe := r.rdb.TxPipeline()
	pipe.Set(ctx, "sip-dialog:"+callID, b, 2*time.Hour)
	if key := dialogKey(callID, fromTag, toTag); key != "" {
		pipe.Set(ctx, key, b, 2*time.Hour)
	}
	_, err = pipe.Exec(ctx)
	return err
}

func (r *registry) destination(ctx context.Context, req *sip.Request) string {
	callID := ""
	if h := req.CallID(); h != nil {
		callID = h.Value()
	}
	fromTag, toTag := dialogTags(req)
	r.mu.Lock()
	entry, ok := r.affinity[callID]
	if ok && time.Now().Before(entry.ExpiresAt) {
		r.mu.Unlock()
		return entry.Destination
	}
	r.mu.Unlock()
	var raw []byte
	if key := dialogKey(callID, fromTag, toTag); key != "" {
		raw, _ = r.rdb.Get(ctx, key).Bytes()
	}
	if len(raw) == 0 {
		raw, _ = r.rdb.Get(ctx, "sip-dialog:"+callID).Bytes()
	}
	if len(raw) == 0 {
		return ""
	}
	if json.Unmarshal(raw, &entry) != nil || time.Now().After(entry.ExpiresAt) {
		return ""
	}
	r.mu.Lock()
	r.affinity[callID] = entry
	r.mu.Unlock()
	return entry.Destination
}

func (r *registry) unbind(ctx context.Context, callID string) {
	r.mu.Lock()
	delete(r.affinity, callID)
	r.mu.Unlock()
	_ = r.rdb.Del(ctx, "sip-dialog:"+callID).Err()
}

func sipPort(value string, fallback int) int {
	_, port, err := net.SplitHostPort(value)
	if err != nil {
		return fallback
	}
	parsed, err := strconv.Atoi(port)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func generatedEdgeID() string {
	value := env("SIPGO_ADVERTISE_IP", "127.0.0.1") + ":" + env("SIPGO_LISTEN", "0.0.0.0:5060")
	value = strings.NewReplacer(".", "-", ":", "-", "/", "-").Replace(value)
	return "edge-" + strings.Trim(value, "-")
}

func registerAdvertisement(ctx context.Context, rdb *redis.Client, lease time.Duration) {
	listen := env("SIPGO_LISTEN", "0.0.0.0:5060")
	tcpListen := env("SIPGO_TCP_LISTEN", listen)
	httpAddress := env("SIPGO_HTTP", "127.0.0.1:8080")
	httpURL := httpBase(env("SIPGO_ADVERTISE_HTTP", httpAddress))
	advertisement := sipgoAdvertisement{
		NodeID:       env("SIPGO_NODE_ID", generatedEdgeID()),
		PrivateIP:    env("SIPGO_ADVERTISE_IP", "127.0.0.1"),
		PublicIP:     env("SIPGO_PUBLIC_IP", ""),
		SIPUDPPort:   sipPort(listen, 5060),
		SIPTCPPort:   sipPort(tcpListen, sipPort(listen, 5060)),
		HTTPURL:      httpURL,
		Role:         "sip-edge",
		NodeEpoch:    time.Now().UnixNano(),
		LastSeenUnix: time.Now().Unix(),
	}
	interval := lease / 3
	if interval < time.Second {
		interval = time.Second
	}
	key := "active-sipgo:" + advertisement.NodeID
	write := func() {
		advertisement.LastSeenUnix = time.Now().Unix()
		body, err := json.Marshal(advertisement)
		if err == nil {
			if err := rdb.Set(ctx, key, body, lease).Err(); err != nil {
				slog.Warn("SipGo advertisement failed", "key", key, "error", err)
			}
		}
	}
	write()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = rdb.Del(context.Background(), key).Err()
			return
		case <-ticker.C:
			write()
		}
	}
}

func headerValue(req *sip.Request, name string) string {
	for _, h := range req.GetHeaders(name) {
		return h.Value()
	}
	for _, h := range req.GetHeaders(strings.ToLower(name)) {
		return h.Value()
	}
	return ""
}

var dtmfBodyRE = regexp.MustCompile(`(?im)(?:^|[\r\n])\s*(?:signal|digit)\s*[:=]\s*([0-9*#abcd])`)

func parseDTMF(req *sip.Request) string {
	contentType := strings.ToLower(headerValue(req, "Content-Type"))
	if !strings.Contains(contentType, "dtmf") && !strings.Contains(contentType, "info-package") {
		return ""
	}
	matches := dtmfBodyRE.FindStringSubmatch(string(req.Body()))
	if len(matches) != 2 {
		return ""
	}
	return strings.ToUpper(strings.TrimSpace(matches[1]))
}

func notifyDTMF(ctx context.Context, req *sip.Request, digit string) {
	endpoint := strings.TrimRight(env("PHONARCH_DTMF_EVENT_URL", "http://127.0.0.1:8081/internal/v1/dtmf"), "/")
	callID := ""
	if h := req.CallID(); h != nil {
		callID = h.Value()
	}
	fromTag, toTag := dialogTags(req)
	payload := map[string]any{
		"call_id":        callID,
		"from_tag":       fromTag,
		"to_tag":         toTag,
		"digit":          digit,
		"workspace_id":   headerValue(req, "X-PhonArch-Workspace-ID"),
		"room_id":        headerValue(req, "X-PhonArch-Room-ID"),
		"participant_id": headerValue(req, "X-PhonArch-Participant-ID"),
		"received_at":    time.Now().UTC(),
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	reqOut, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(b)))
	if err != nil {
		return
	}
	reqOut.Header.Set("Content-Type", "application/json")
	if token := os.Getenv("PHONARCH_INTERNAL_TOKEN"); token != "" {
		reqOut.Header.Set("X-PhonArch-Internal-Token", token)
	}
	client := &http.Client{Timeout: 750 * time.Millisecond}
	res, err := client.Do(reqOut)
	if err == nil {
		res.Body.Close()
	} else {
		slog.Warn("DTMF event notification failed", "call_id", callID, "digit", digit, "error", err)
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func httpBase(value string) string {
	if strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://") {
		return strings.TrimRight(value, "/")
	}
	return "http://" + strings.TrimRight(value, "/")
}
func durationEnv(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	rdb := redis.NewClient(&redis.Options{Addr: env("REDIS_ADDR", "127.0.0.1:6379")})
	if err := rdb.Ping(ctx).Err(); err != nil {
		slog.Error("redis unavailable", "error", err)
		os.Exit(1)
	}
	reg := newRegistry(rdb, durationEnv("SIPGO_HEARTBEAT_LEASE", 7*time.Second))
	go registerAdvertisement(ctx, rdb, durationEnv("SIPGO_EDGE_LEASE", 7*time.Second))

	ua, err := sipgo.NewUA()
	if err != nil {
		slog.Error("create UA", "error", err)
		os.Exit(1)
	}
	srv, err := sipgo.NewServer(ua)
	if err != nil {
		slog.Error("create SIP server", "error", err)
		os.Exit(1)
	}
	client, err := sipgo.NewClient(ua, sipgo.WithClientAddr(env("SIPGO_LISTEN", "0.0.0.0:5060")))
	if err != nil {
		slog.Error("create SIP client", "error", err)
		os.Exit(1)
	}

	respond := func(tx sip.ServerTransaction, req *sip.Request, code int, reason string) {
		res := sip.NewResponseFromRequest(req, code, reason, nil)
		res.SetDestination(req.Source())
		if err := tx.Respond(res); err != nil {
			slog.Warn("respond failed", "error", err)
		}
	}

	optionsHandler := func(req *sip.Request, tx sip.ServerTransaction) {
		if id := headerValue(req, "X-PhonArch-Node-ID"); id != "" {
			capacity, _ := strconv.Atoi(headerValue(req, "X-PhonArch-Capacity"))
			calls, _ := strconv.Atoi(headerValue(req, "X-PhonArch-Active-Calls"))
			bridges, _ := strconv.Atoi(headerValue(req, "X-PhonArch-Active-Bridges"))
			rtpSessions, _ := strconv.Atoi(headerValue(req, "X-PhonArch-Active-RTP-Sessions"))
			rtpPPS, _ := strconv.ParseInt(headerValue(req, "X-PhonArch-RTP-Packets-Per-Second"), 10, 64)
			speakers, _ := strconv.Atoi(headerValue(req, "X-PhonArch-Active-Speakers"))
			epoch, _ := strconv.ParseInt(headerValue(req, "X-PhonArch-Node-Epoch"), 10, 64)
			load, _ := strconv.ParseFloat(headerValue(req, "X-PhonArch-Load-Score"), 64)
			ip := headerValue(req, "X-PhonArch-Private-IP")
			mediaIP := headerValue(req, "X-PhonArch-Media-Public-IP")
			control := headerValue(req, "X-PhonArch-Control-URL")
			role := headerValue(req, "X-PhonArch-Role")
			drain := headerValue(req, "X-PhonArch-Drain-State")
			udp, _ := strconv.Atoi(headerValue(req, "X-PhonArch-SIP-UDP-Port"))
			tcp, _ := strconv.Atoi(headerValue(req, "X-PhonArch-SIP-TCP-Port"))
			if udp == 0 {
				udp = 5060
			}
			if tcp == 0 {
				tcp = 5060
			}
			if err := reg.upsert(ctx, node{NodeID: id, PrivateIP: ip, MediaPublicIP: mediaIP, SIPUDPPort: udp, SIPTCPPort: tcp, ControlURL: control, Role: role, DrainState: drain, Capacity: capacity, ActiveCalls: calls, ActiveRTP: rtpSessions, RTPPacketsPerSecond: rtpPPS, ActiveSpeakers: speakers, ActiveBridges: bridges, LoadScore: load, NodeEpoch: epoch}); err != nil {
				respond(tx, req, 503, "Redis Unavailable")
				return
			}
			respond(tx, req, 200, "OK")
			return
		}
		respond(tx, req, 200, "OK")
	}
	srv.OnOptions(optionsHandler)

	forward := func(req *sip.Request, tx sip.ServerTransaction, destination string) {
		if destination == "" {
			respond(tx, req, 503, "No PBX Available")
			return
		}
		req.SetDestination(destination)
		clTx, err := client.TransactionRequest(ctx, req, sipgo.ClientRequestAddVia, sipgo.ClientRequestAddRecordRoute)
		if err != nil {
			respond(tx, req, 502, "Bad Gateway")
			return
		}
		defer clTx.Terminate()
		for {
			select {
			case res, more := <-clTx.Responses():
				if !more {
					return
				}
				res.SetDestination(req.Source())
				res.RemoveHeader("Via")
				_ = tx.Respond(res)
			case ack := <-tx.Acks():
				ack.SetDestination(destination)
				_ = client.WriteRequest(ack)
			case <-clTx.Done():
				return
			case <-tx.Done():
				return
			}
		}
	}

	invite := func(req *sip.Request, tx sip.ServerTransaction) {
		// An INVITE carrying a To-tag is an in-dialog re-INVITE. It must never
		// be load-balanced as a new call.
		_, toTag := dialogTags(req)
		if toTag != "" {
			if dst := reg.destination(ctx, req); dst != "" {
				forward(req, tx, dst)
				return
			}
			respond(tx, req, 481, "Dialog Does Not Exist")
			return
		}
		n, err := reg.selectNode(ctx)
		if err != nil {
			respond(tx, req, 503, "No PBX Available")
			return
		}
		if err := reg.bind(ctx, req, n.destination(), n.NodeEpoch); err != nil {
			respond(tx, req, 503, "Dialog State Unavailable")
			return
		}
		forward(req, tx, n.destination())
	}
	inDialog := func(req *sip.Request, tx sip.ServerTransaction) {
		callID := req.CallID().Value()
		dst := reg.destination(ctx, req)
		if dst == "" {
			respond(tx, req, 481, "Dialog Does Not Exist")
			return
		}
		if req.Method == sip.ACK {
			req.SetDestination(dst)
			_ = client.WriteRequest(req)
			return
		}
		forward(req, tx, dst)
		if req.Method == sip.BYE || req.Method == sip.CANCEL {
			reg.unbind(ctx, callID)
		}
	}
	srv.OnInvite(invite)
	srv.OnAck(inDialog)
	srv.OnBye(inDialog)
	srv.OnCancel(inDialog)
	srv.OnUpdate(inDialog)

	// INFO is terminated at the edge only for DTMF relay bodies. This keeps
	// the event available to the room coordinator while still returning a
	// timely 200 response to the carrier. Non-DTMF INFO is forwarded normally.
	srv.OnInfo(func(req *sip.Request, tx sip.ServerTransaction) {
		digit := parseDTMF(req)
		if digit == "" {
			inDialog(req, tx)
			return
		}
		if dst := reg.destination(ctx, req); dst == "" {
			respond(tx, req, 481, "Dialog Does Not Exist")
			return
		}
		notifyDTMF(ctx, req, digit)
		respond(tx, req, 200, "OK")
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := rdb.Ping(r.Context()).Err(); err != nil {
			http.Error(w, err.Error(), 503)
			return
		}
		w.WriteHeader(200)
	})
	mux.HandleFunc("/internal/v1/nodes", func(w http.ResponseWriter, r *http.Request) {
		nodes, err := reg.list(r.Context())
		if err != nil {
			http.Error(w, err.Error(), 503)
			return
		}
		_ = json.NewEncoder(w).Encode(nodes)
	})
	mux.HandleFunc("/internal/v1/nodes/deregister", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			NodeID string `json:"node_id"`
		}
		if json.NewDecoder(r.Body).Decode(&body) == nil && body.NodeID != "" {
			_ = reg.remove(r.Context(), body.NodeID)
		}
		w.WriteHeader(204)
	})
	go func() {
		addr := env("SIPGO_HTTP", "127.0.0.1:8080")
		slog.Info("SIPGo HTTP", "addr", addr)
		if err := http.ListenAndServe(addr, mux); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http", "error", err)
		}
	}()

	go func() {
		if err := srv.ListenAndServe(ctx, "udp", env("SIPGO_LISTEN", "0.0.0.0:5060")); err != nil {
			slog.Error("SIP UDP", "error", err)
		}
	}()
	if err := srv.ListenAndServe(ctx, "tcp", env("SIPGO_TCP_LISTEN", "0.0.0.0:5060")); err != nil {
		slog.Error("SIP TCP", "error", err)
	}
}
