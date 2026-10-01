// Package discovery 实现 WS-Discovery 应答器：
// 加入 UDP 3702 组播组并响应 Probe（ProbeMatch），让局域网内 ONVIF 客户端自动发现本适配器。
package discovery

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"log/slog"
	"net"
	"regexp"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/ipv4"
)

const (
	multicastAddr = "239.255.255.250:3702"
	appInstance   = 1
)

// Responder WS-Discovery 应答器。
type Responder struct {
	EndpointURN string // 如 urn:uuid:skydroid-xxxx
	XAddr       string // http://ip:port/onvif/device_service
	Scopes      []string
	logger      *slog.Logger
}

// New 创建应答器。
func New(endpointURN, xaddr string, scopes []string, logger *slog.Logger) *Responder {
	if logger == nil {
		logger = slog.Default()
	}
	return &Responder{EndpointURN: endpointURN, XAddr: xaddr, Scopes: scopes, logger: logger}
}

var messageIDRe = regexp.MustCompile(`<[^>]*:MessageID[^>]*>([^<]+)<`)

// Run 阻塞运行；端口 3702 被占用时降级为周期性组播 Hello。
func (r *Responder) Run(ctx context.Context) {
	conn, err := listenUDP()
	if err != nil {
		r.logger.Warn("ws-discovery port 3702 busy, fallback to periodic Hello", "err", err.Error())
		r.runHelloFallback(ctx)
		return
	}
	defer func() { _ = conn.Close() }()
	addr := mustResolve(multicastAddr)
	if err := joinGroup(conn, addr); err != nil {
		r.logger.Warn("ws-discovery join multicast failed", "err", err.Error())
	}
	r.logger.Info("ws-discovery responder started", "multicast", multicastAddr, "xaddr", r.XAddr)

	buf := make([]byte, 8192)
	for {
		if ctx.Err() != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			if ctx.Err() != nil {
				return
			}
			continue
		}
		msg := string(buf[:n])
		if !strings.Contains(msg, ":Probe") && !strings.Contains(msg, "Probe>") {
			continue
		}
		resp := r.probeMatch(extractMessageID(msg))
		if _, err := conn.WriteToUDP([]byte(resp), from); err != nil {
			r.logger.Warn("ws-discovery reply failed", "err", err.Error())
		} else {
			r.logger.Debug("ws-discovery probematch sent", "to", from.String())
		}
	}
}

func listenUDP() (*net.UDPConn, error) {
	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			var sockErr error
			err := c.Control(func(fd uintptr) {
				sockErr = setReuseAddr(fd)
			})
			if err != nil {
				return err
			}
			return sockErr
		},
	}
	pc, err := lc.ListenPacket(context.Background(), "udp4", multicastAddr)
	if err != nil {
		return nil, err
	}
	udp, ok := pc.(*net.UDPConn)
	if !ok {
		_ = pc.Close()
		return nil, fmt.Errorf("unexpected connection type %T", pc)
	}
	return udp, nil
}

func joinGroup(conn *net.UDPConn, addr *net.UDPAddr) error {
	interfaces, err := net.Interfaces()
	if err != nil {
		return err
	}
	pconn := ipv4.NewPacketConn(conn)
	joined := false
	for _, ifi := range interfaces {
		if ifi.Flags&net.FlagMulticast == 0 || ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		if err := pconn.JoinGroup(&ifi, addr); err == nil {
			joined = true
		}
	}
	if !joined {
		return fmt.Errorf("no interface joined multicast group")
	}
	return nil
}

func extractMessageID(msg string) string {
	if m := messageIDRe.FindStringSubmatch(msg); len(m) > 1 {
		return m[1]
	}
	return "urn:unknown"
}

func (r *Responder) probeMatch(relatesTo string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>` +
		`<SOAP-ENV:Envelope xmlns:SOAP-ENV="http://www.w3.org/2003/05/soap-envelope"` +
		` xmlns:wsa="http://schemas.xmlsoap.org/ws/2004/08/addressing"` +
		` xmlns:d="http://schemas.xmlsoap.org/ws/2005/04/discovery"` +
		` xmlns:dn="http://www.onvif.org/ver10/network/wsdl">` +
		`<SOAP-ENV:Header>` +
		`<wsa:Action>http://schemas.xmlsoap.org/ws/2005/04/discovery/ProbeMatches</wsa:Action>` +
		`<wsa:RelatesTo>` + xmlEscape(relatesTo) + `</wsa:RelatesTo>` +
		`<wsa:To>http://schemas.xmlsoap.org/ws/2004/08/addressing/role/anonymous</wsa:To>` +
		`<wsa:MessageID>urn:uuid:` + newUUID() + `</wsa:MessageID>` +
		fmt.Sprintf(`<d:AppSequence InstanceId="%d" MessageNumber="%d"/>`, appInstance, time.Now().UnixNano()%1e9) +
		`</SOAP-ENV:Header>` +
		`<SOAP-ENV:Body><d:ProbeMatches><d:ProbeMatch>` +
		`<wsa:EndpointReference><wsa:Address>` + xmlEscape(r.EndpointURN) + `</wsa:Address></wsa:EndpointReference>` +
		`<d:Types>dn:NetworkVideoTransmitter</d:Types>` +
		`<d:Scopes>` + xmlEscape(strings.Join(r.Scopes, " ")) + `</d:Scopes>` +
		`<d:XAddrs>` + xmlEscape(r.XAddr) + `</d:XAddrs>` +
		`<d:MetadataVersion>1</d:MetadataVersion>` +
		`</d:ProbeMatch></d:ProbeMatches></SOAP-ENV:Body></SOAP-ENV:Envelope>`
}

func (r *Responder) hello(seq int64) string {
	return `<?xml version="1.0" encoding="UTF-8"?>` +
		`<SOAP-ENV:Envelope xmlns:SOAP-ENV="http://www.w3.org/2003/05/soap-envelope"` +
		` xmlns:wsa="http://schemas.xmlsoap.org/ws/2004/08/addressing"` +
		` xmlns:d="http://schemas.xmlsoap.org/ws/2005/04/discovery"` +
		` xmlns:dn="http://www.onvif.org/ver10/network/wsdl">` +
		`<SOAP-ENV:Header>` +
		`<wsa:Action>http://schemas.xmlsoap.org/ws/2005/04/discovery/Hello</wsa:Action>` +
		`<wsa:MessageID>urn:uuid:` + newUUID() + `</wsa:MessageID>` +
		`<wsa:To>urn:schemas-xmlsoap-org:ws:2005:04:discovery</wsa:To>` +
		fmt.Sprintf(`<d:AppSequence InstanceId="%d" MessageNumber="%d"/>`, appInstance, seq) +
		`</SOAP-ENV:Header>` +
		`<SOAP-ENV:Body><d:Hello>` +
		`<wsa:EndpointReference><wsa:Address>` + xmlEscape(r.EndpointURN) + `</wsa:Address></wsa:EndpointReference>` +
		`<d:Types>dn:NetworkVideoTransmitter</d:Types>` +
		`<d:Scopes>` + xmlEscape(strings.Join(r.Scopes, " ")) + `</d:Scopes>` +
		`<d:XAddrs>` + xmlEscape(r.XAddr) + `</d:XAddrs>` +
		`<d:MetadataVersion>1</d:MetadataVersion>` +
		`</d:Hello></SOAP-ENV:Body></SOAP-ENV:Envelope>`
}

func (r *Responder) runHelloFallback(ctx context.Context) {
	conn, err := net.DialUDP("udp4", nil, mustResolve(multicastAddr))
	if err != nil {
		r.logger.Error("discovery hello fallback failed", "err", err.Error())
		return
	}
	defer func() { _ = conn.Close() }()
	var seq int64
	for {
		if _, err := conn.Write([]byte(r.hello(seq))); err != nil {
			r.logger.Warn("discovery hello send failed", "err", err.Error())
		}
		seq++
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
	}
}

func mustResolve(addr string) *net.UDPAddr {
	a, err := net.ResolveUDPAddr("udp4", addr)
	if err != nil {
		return &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 3702}
	}
	return a
}

func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func xmlEscape(s string) string {
	var b strings.Builder
	if err := xml.EscapeText(&b, []byte(s)); err != nil {
		return ""
	}
	return b.String()
}
