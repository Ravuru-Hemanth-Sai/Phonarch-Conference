package main

// Deterministic lab double for the external RustPBX engine. It provides the
// local HTTP adapter contract and a tiny SIP UAS for SIPGo routing tests.
import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type call struct {
	CallID      string  `json:"call_id"`
	PBXMemberID string  `json:"pbx_member_id"`
	State       string  `json:"state"`
	Muted       bool    `json:"muted"`
	VolumeDB    float64 `json:"volume_db"`
}

var (
	mu    sync.RWMutex
	calls = map[string]call{}
	seq   atomic.Int64
)

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func update(id string, fn func(*call)) {
	mu.Lock()
	current := calls[id]
	fn(&current)
	calls[id] = current
	mu.Unlock()
}

func originate(w http.ResponseWriter, r *http.Request) {
	var input struct {
		CallID      string `json:"call_id"`
		DesiredMute bool   `json:"desired_mute"`
	}
	if json.NewDecoder(r.Body).Decode(&input) != nil || input.CallID == "" {
		http.Error(w, "call_id required", http.StatusBadRequest)
		return
	}
	member := fmt.Sprintf("mock-member-%d", seq.Add(1))
	mu.Lock()
	calls[input.CallID] = call{CallID: input.CallID, PBXMemberID: member, State: "DISPATCHED", Muted: input.DesiredMute, VolumeDB: -18}
	mu.Unlock()
	go func() {
		for _, state := range []string{"RINGING", "ANSWERED", "IN_BRIDGE"} {
			time.Sleep(700 * time.Millisecond)
			update(input.CallID, func(c *call) { c.State = state })
		}
	}()
	_ = json.NewEncoder(w).Encode(map[string]string{"pbx_member_id": member})
}

func httpServer() {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/v1/calls/originate", originate)
	mux.HandleFunc("/v1/calls/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/v1/calls/")
		mu.RLock()
		current, ok := calls[id]
		mu.RUnlock()
		if !ok {
			http.Error(w, "not found", 404)
			return
		}
		_ = json.NewEncoder(w).Encode(current)
	})
	mux.HandleFunc("/v1/participants/", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			CallID string `json:"call_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&input)
		if strings.HasSuffix(r.URL.Path, "/mute") {
			update(input.CallID, func(c *call) { c.Muted = true })
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "muted": true})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/unmute") {
			update(input.CallID, func(c *call) { c.Muted = false })
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "muted": false})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/drop") {
			update(input.CallID, func(c *call) { c.State = "ENDED" })
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
			return
		}
		http.Error(w, "unknown action", 404)
	})
	listen := env("MOCK_LISTEN", ":9090")
	slog.Info("mock RustPBX HTTP", "listen", listen)
	if err := http.ListenAndServe(listen, mux); err != nil {
		slog.Error("mock HTTP", "error", err)
	}
}

func sipHeader(message, name string) string {
	for _, line := range strings.Split(message, "\r\n") {
		if strings.HasPrefix(strings.ToLower(line), strings.ToLower(name)+":") {
			return strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
		}
	}
	return ""
}

func sipServer() {
	listen := env("MOCK_SIP_LISTEN", "127.0.0.1:5071")
	addr, err := net.ResolveUDPAddr("udp", listen)
	if err != nil {
		slog.Error("mock SIP address", "error", err)
		return
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		slog.Error("mock SIP listen", "error", err)
		return
	}
	defer conn.Close()
	buffer := make([]byte, 65535)
	for {
		size, remote, readErr := conn.ReadFromUDP(buffer)
		if readErr != nil {
			continue
		}
		message := string(buffer[:size])
		first := strings.SplitN(message, "\r\n", 2)[0]
		if strings.HasPrefix(first, "SIP/2.0") {
			continue
		}
		method := strings.Fields(first)
		if len(method) == 0 {
			continue
		}
		via, from, to, callID, cseq := sipHeader(message, "Via"), sipHeader(message, "From"), sipHeader(message, "To"), sipHeader(message, "Call-ID"), sipHeader(message, "CSeq")
		switch strings.ToUpper(method[0]) {
		case "INVITE":
			go func() {
				for _, item := range []struct {
					code   int
					reason string
					wait   time.Duration
				}{{100, "Trying", 0}, {180, "Ringing", 150 * time.Millisecond}, {200, "OK", 250 * time.Millisecond}} {
					time.Sleep(item.wait)
					responseTo(conn, remote, item.code, item.reason, via, from, to, callID, cseq)
				}
			}()
		case "BYE", "CANCEL":
			responseTo(conn, remote, 200, "OK", via, from, to, callID, cseq)
		}
	}
}

func responseTo(conn *net.UDPConn, remote *net.UDPAddr, code int, reason, via, from, to, callID, cseq string) {
	if !strings.Contains(to, ";tag=") {
		to += ";tag=mock-" + strconv.Itoa(code)
	}
	response := fmt.Sprintf("SIP/2.0 %d %s\r\nVia: %s\r\nFrom: %s\r\nTo: %s\r\nCall-ID: %s\r\nCSeq: %s\r\nContent-Length: 0\r\n\r\n", code, reason, via, from, to, callID, cseq)
	_, _ = conn.WriteToUDP([]byte(response), remote)
}

func main() { go httpServer(); sipServer() }
