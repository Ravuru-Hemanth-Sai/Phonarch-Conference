package main

import (
	"fmt"
	"net"
	"strings"
	"time"
)

func header(message, name string) string {
	for _, line := range strings.Split(message, "\r\n") {
		if strings.HasPrefix(strings.ToLower(line), strings.ToLower(name)+":") {
			return strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
		}
	}
	return ""
}
func readResponse(conn *net.UDPConn) (string, error) {
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 65535)
	n, _, err := conn.ReadFromUDP(buf)
	return string(buf[:n]), err
}
func main() {
	local, _ := net.ResolveUDPAddr("udp", "127.0.0.1:5099")
	remote, _ := net.ResolveUDPAddr("udp", "127.0.0.1:5060")
	conn, err := net.ListenUDP("udp", local)
	if err != nil {
		panic(err)
	}
	defer conn.Close()
	callID := fmt.Sprintf("probe-%d@lab", time.Now().UnixNano())
	from := "<sip:probe@lab>;tag=probe-1"
	to := "<sip:+15550123@provider>"
	via := "SIP/2.0/UDP 127.0.0.1:5099;branch=z9hG4bK-probe"
	invite := fmt.Sprintf("INVITE sip:+15550123@127.0.0.1:5060 SIP/2.0\r\nVia: %s\r\nFrom: %s\r\nTo: %s\r\nCall-ID: %s\r\nCSeq: 1 INVITE\r\nMax-Forwards: 70\r\nContact: <sip:probe@127.0.0.1:5099>\r\nContent-Length: 0\r\n\r\n", via, from, to, callID)
	_, _ = conn.WriteToUDP([]byte(invite), remote)
	var ok string
	for i := 0; i < 4; i++ {
		response, err := readResponse(conn)
		if err != nil {
			panic(err)
		}
		fmt.Printf("response: %s\n", strings.SplitN(response, "\r\n", 2)[0])
		if strings.HasPrefix(response, "SIP/2.0 200") {
			ok = response
			break
		}
	}
	if ok == "" {
		panic("no 200 response")
	}
	to = header(ok, "To")
	ack := fmt.Sprintf("ACK sip:+15550123@127.0.0.1:5060 SIP/2.0\r\nVia: %s\r\nFrom: %s\r\nTo: %s\r\nCall-ID: %s\r\nCSeq: 1 ACK\r\nMax-Forwards: 70\r\nContent-Length: 0\r\n\r\n", via, from, to, callID)
	_, _ = conn.WriteToUDP([]byte(ack), remote)
	time.Sleep(100 * time.Millisecond)
	bye := fmt.Sprintf("BYE sip:+15550123@127.0.0.1:5060 SIP/2.0\r\nVia: %s\r\nFrom: %s\r\nTo: %s\r\nCall-ID: %s\r\nCSeq: 2 BYE\r\nMax-Forwards: 70\r\nContent-Length: 0\r\n\r\n", via, from, to, callID)
	_, _ = conn.WriteToUDP([]byte(bye), remote)
	response, err := readResponse(conn)
	if err != nil {
		panic(err)
	}
	fmt.Printf("bye: %s\n", strings.SplitN(response, "\r\n", 2)[0])
}
