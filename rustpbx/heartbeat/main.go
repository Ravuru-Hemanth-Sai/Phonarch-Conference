package main

// Standalone fallback heartbeat utility. The normal deployment uses the PBX
// sidecar, which discovers active SipGo edges from Redis. This utility keeps
// the same multi-edge OPTIONS contract for engines that cannot load the full
// sidecar.
import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func targets(value string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, raw := range strings.Split(value, ",") {
		target := strings.TrimSpace(raw)
		if target == "" {
			continue
		}
		if _, ok := seen[target]; ok {
			continue
		}
		seen[target] = struct{}{}
		out = append(out, target)
	}
	return out
}

func main() {
	privateIP := env("PRIVATE_IP", "127.0.0.1")
	nodeID := env("NODE_ID", "")
	if nodeID == "" {
		nodeID = "core-" + strings.NewReplacer(".", "-", ":", "-").Replace(privateIP)
	}
	edgeTargets := targets(env("SIPGO_HEARTBEAT_TARGETS", env("SIPGO_HEARTBEAT_TARGET", "")))
	interval, _ := time.ParseDuration(env("HEARTBEAT_INTERVAL", "2s"))
	if interval <= 0 {
		interval = 2 * time.Second
	}
	capacity, _ := strconv.Atoi(env("NODE_CAPACITY", "500"))
	if capacity <= 0 {
		capacity = 500
	}
	sipPort := env("SIP_UDP_PORT", "5060")
	tcpPort := env("SIP_TCP_PORT", sipPort)
	mediaIP := env("MEDIA_PUBLIC_IP", "")
	controlURL := env("SIDECAR_CONTROL_URL", "")
	role := env("NODE_ROLE", "listener")
	drain := env("NODE_DRAIN_STATE", "READY")
	instance := fmt.Sprintf("%d", time.Now().UnixNano())
	seq := 1
	for {
		for _, target := range edgeTargets {
			addr, err := net.ResolveUDPAddr("udp", target)
			if err != nil {
				continue
			}
			conn, err := net.DialUDP("udp", nil, addr)
			if err != nil {
				continue
			}
			msg := fmt.Sprintf("OPTIONS sip:phonarch-sipgo@%s SIP/2.0\r\nVia: SIP/2.0/UDP %s:%s\r\nFrom: <sip:%s@phonarch>;tag=%s\r\nTo: <sip:phonarch-sipgo@phonarch>\r\nCall-ID: heartbeat-%s-%d\r\nCSeq: %d OPTIONS\r\nMax-Forwards: 1\r\nX-PhonArch-Node-ID: %s\r\nX-PhonArch-Private-IP: %s\r\nX-PhonArch-Media-Public-IP: %s\r\nX-PhonArch-Control-URL: %s\r\nX-PhonArch-SIP-UDP-Port: %s\r\nX-PhonArch-SIP-TCP-Port: %s\r\nX-PhonArch-Role: %s\r\nX-PhonArch-Drain-State: %s\r\nX-PhonArch-Capacity: %d\r\nX-PhonArch-Active-Calls: 0\r\nX-PhonArch-Active-Bridges: 0\r\nX-PhonArch-Load-Score: 0\r\nX-PhonArch-Node-Epoch: %d\r\nContent-Length: 0\r\n\r\n", target, privateIP, sipPort, nodeID, instance, nodeID, seq, seq, nodeID, privateIP, mediaIP, controlURL, sipPort, tcpPort, role, drain, capacity, time.Now().UnixNano())
			_, _ = conn.Write([]byte(msg))
			_ = conn.Close()
		}
		seq++
		time.Sleep(interval)
	}
}
