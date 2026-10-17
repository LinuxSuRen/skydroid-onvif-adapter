// Package skydroid 实现 Skydroid（云卓）云台相机的 TP 控制协议客户端。
//
// 协议要点（以官方 rcsdk-demo 为准，逆向库互证）：
//   - 命令为 ASCII 文本，格式 #TP[单元][长度][r/w][命令][参数][校验]
//     单元：UD=相机 UG=云台 UM=模块
//   - 校验：整条命令（含 '#' 与参数）全部 ASCII 字节累加和的低 8 位，以 2 位
//     十六进制追加在末尾，如 "#TPUD2wCAP01" + "3E" → "#TPUD2wCAP013E"
//   - 云台速度（GSY/GSP）为 1 字节带符号补码，范围约 ±100：
//     正值右转/上仰，负值左转/下俯（0x64=+100，0x9C=-100）
//   - 端口（官方 demo）：C10Pro 走 UDP（本地 5000 → 相机 5000）；
//     C20 相机走 TCP 8100，C20 云台走 TCP 5000
//   - 相机默认 IP 192.168.144.108，RTSP 视频端口 554（主/子码流 stream=0/1）
package skydroid

import (
	"fmt"
	"log/slog"
	"math"
	"net"
	"strings"
	"sync"
	"time"
)

// TP 协议命令模板（不带校验，发送时自动追加）。
const (
	// 云台（UG）。
	CmdPTZStop = "#TPUG2wPTZ00" // 停止云台
	// 八方向恒动（发送后持续运动直到 Stop）。
	CmdPTZUp        = "#TPUG2wPTZ01"
	CmdPTZDown      = "#TPUG2wPTZ02"
	CmdPTZLeft      = "#TPUG2wPTZ03"
	CmdPTZRight     = "#TPUG2wPTZ04"
	CmdPTZUpLeft    = "#TPUG2wPTZ05"
	CmdPTZUpRight   = "#TPUG2wPTZ06"
	CmdPTZDownLeft  = "#TPUG2wPTZ07"
	CmdPTZDownRight = "#TPUG2wPTZ08"
	CmdPTZCenter    = "#TPUG2wPTZ09" // 回中
	CmdPTZZoomIn    = "#TPUG2wPTZ0A"
	CmdPTZZoomOut   = "#TPUG2wPTZ0B"
	CmdPTZLookDown  = "#TPUG2wPTZ10" // 垂直向下（-90°）
	CmdPTZLookFwd   = "#TPUG2wPTZ11" // 水平朝前（0°）
	CmdPTZFollow    = "#TPUG2wPTZ12" // Follow 模式
	CmdPTZLock      = "#TPUG2wPTZ13" // Lock 模式
	CmdPTZFPV       = "#TPUG2wPTZ14" // FPV 模式

	CmdSetYawSpeedPrefix   = "#TPUG2wGSY" // + 2 位十六进制带符号速度
	CmdSetPitchSpeedPrefix = "#TPUG2wGSP" // + 2 位十六进制带符号速度
	CmdGotoYawPrefix       = "#TPUG6wGAY" // + 6 字节角度（偏航）
	CmdGotoPitchPrefix     = "#TPUG6wGAP" // + 6 字节角度（俯仰）

	// 相机（UD）。
	CmdGetVersion = "#TPUD2rVER00" // 查询固件版本（TCP）
	CmdTakePhoto  = "#TPUD2wCAP01" // 拍照
	CmdRecStart   = "#TPUD2wREC01" // 开始录像
	CmdRecStop    = "#TPUD2wREC00" // 停止录像
)

// Transport 命令通道类型。
type Transport string

// 支持的命令通道。
const (
	TransportUDP Transport = "udp" // C10Pro：UDP 到相机 5000
	TransportTCP Transport = "tcp" // C20：TCP 到相机 8100（相机）/5000（云台）
)

// Client TP 协议客户端。
type Client struct {
	transport Transport
	udpAddr   string
	tcpAddr   string
	logger    *slog.Logger

	udpConn      *net.UDPConn
	udpRetries   int
	retryTimeout time.Duration
	tcpTimeout   time.Duration

	attMu    sync.Mutex
	attYaw   float64
	attPitch float64
	attRoll  float64
	attAt    time.Time
	attValid bool

	// UDP 查询等待机制（issue #3）：QueryUDP 注册 waiter 后发送命令，
	// readLoop/handleFrame 收到查询应答帧（含 "VER" 且非 GAC）时投递给 waiter；
	// 无 waiter 时投递是空操作，readLoop 行为与原来完全一致。
	queryMu     sync.Mutex
	queryWaiter chan string
}

// NewClient 创建客户端。udpAddr/tcpAddr 形如 192.168.144.108:5000。
// listenUDPPort > 0 时绑定本地端口接收云台主动上报的姿态帧（SDK 默认本地 5000）。
func NewClient(transport Transport, udpAddr, tcpAddr string, listenUDPPort int, logger *slog.Logger) (*Client, error) {
	if logger == nil {
		logger = slog.Default()
	}
	c := &Client{
		transport:    transport,
		udpAddr:      udpAddr,
		tcpAddr:      tcpAddr,
		logger:       logger,
		udpRetries:   3,
		retryTimeout: 30 * time.Millisecond,
		tcpTimeout:   2 * time.Second,
	}
	if transport == TransportUDP {
		raddr, err := net.ResolveUDPAddr("udp4", udpAddr)
		if err != nil {
			return nil, fmt.Errorf("resolve udp addr: %w", err)
		}
		// 优先绑定固定本地端口接收上报；失败则回退临时端口（无姿态回报）。
		var conn *net.UDPConn
		if listenUDPPort > 0 {
			if lconn, err := net.DialUDP("udp4", &net.UDPAddr{Port: listenUDPPort}, raddr); err == nil {
				conn = lconn
			} else {
				logger.Warn("bind local udp port failed, attitude report unavailable",
					"port", listenUDPPort, "err", err.Error())
			}
		}
		if conn == nil {
			lconn, err := net.DialUDP("udp4", nil, raddr)
			if err != nil {
				return nil, fmt.Errorf("dial udp: %w", err)
			}
			conn = lconn
		}
		c.udpConn = conn
		go c.readLoop()
	}
	return c, nil
}

// readLoop 是 udpConn 上唯一的读取方：消费全部入站帧——GAC 姿态帧更新姿态，
// 查询应答帧投递给挂起的 waiter（handleFrame 分发）。UDP 查询不得另开并发读
// 同一 socket，否则会与 readLoop 互相抢包。
func (c *Client) readLoop() {
	buf := make([]byte, 512)
	for {
		n, _, err := c.udpConn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		c.handleFrame(string(buf[:n]))
	}
}

// handleFrame 分发 readLoop 收到的帧：
//   - GAC 姿态上报帧（#tp*GAC<yaw4><pitch4><roll4><crc2>）→ 更新姿态缓存；
//   - 其余含 "VER" 的查询应答帧（如 #tpDU3rVER01<版本><校验>）→ 投递给挂起的
//     UDP 查询 waiter（见 QueryUDP）；无 waiter 时忽略，行为与原来一致。
func (c *Client) handleFrame(frame string) {
	if len(frame) < 4 || frame[0] != '#' || (frame[1] != 't' && frame[1] != 'T') {
		return
	}
	if idx := strings.Index(frame, "GAC"); idx >= 0 {
		if len(frame) < idx+3+12 {
			return
		}
		arg := frame[idx+3 : idx+3+12]
		yaw, err1 := HexToAngle(arg[0:4])
		pitch, err2 := HexToAngle(arg[4:8])
		roll, err3 := HexToAngle(arg[8:12])
		if err1 != nil || err2 != nil || err3 != nil {
			return
		}
		c.attMu.Lock()
		c.attYaw, c.attPitch, c.attRoll = yaw, pitch, roll
		c.attAt, c.attValid = time.Now(), true
		c.attMu.Unlock()
		c.logger.Debug("skydroid attitude", "yaw", yaw, "pitch", pitch, "roll", roll)
		return
	}
	// VER 查询应答帧（含 "VER" 且非 GAC）：交给挂起的 UDP 查询 waiter。
	if strings.Contains(frame, "VER") {
		c.deliverQueryResponse(frame)
	}
}

// deliverQueryResponse 把查询应答帧投递给挂起的 waiter（无 waiter 时丢弃，
// 不影响 readLoop）。非阻塞投递，缓冲为 1，重复帧自动丢弃。
func (c *Client) deliverQueryResponse(frame string) {
	c.queryMu.Lock()
	ch := c.queryWaiter
	c.queryMu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- frame:
	default:
	}
}

// Attitude 返回最近一次云台上报的姿态；fresh 表示数据在 maxAge 内。
func (c *Client) Attitude(maxAge time.Duration) (yaw, pitch, roll float64, fresh bool) {
	c.attMu.Lock()
	defer c.attMu.Unlock()
	if !c.attValid || time.Since(c.attAt) > maxAge {
		return 0, 0, 0, false
	}
	return c.attYaw, c.attPitch, c.attRoll, true
}

// Close 释放资源。
func (c *Client) Close() error {
	if c.udpConn != nil {
		return c.udpConn.Close()
	}
	return nil
}

// Checksum 返回命令的累加校验（2 位十六进制）：整条命令全部 ASCII 字节
// 求和取低 8 位。示例：Checksum("#TPUD2wCAP01") == "3E"。
func Checksum(cmd string) string {
	var sum byte
	for i := 0; i < len(cmd); i++ {
		sum += cmd[i]
	}
	return fmt.Sprintf("%02X", sum)
}

// Frame 为命令追加校验，返回完整帧。
func Frame(cmd string) string { return cmd + Checksum(cmd) }

// ParseVERResponse 解析 VER 查询应答帧，提取固件版本号（非应答帧返回空串）。
// 应答形如 "#tpDU3rVER01<版本数据><校验2>"："VER" 后 2 位为状态位（实测 "01"），
// 其后即版本数据。
//
// 校验实测结论（issue #3 真机应答原文 "#tpDU3rVER01050860"）：末 2 位 "60" 恰好
// 等于去掉末 2 位后整帧（含 '#'）全部 ASCII 字节的累加校验
// （Checksum("#tpDU3rVER010508") == "60"），与 GAC 姿态上报帧一样带 2 位尾校验。
// 因此末 2 位校验吻合时校验并剥离后再取版本号——真机应答的版本数据实为 "0508"
// （固件 05.08，把校验位 "60" 读成 "050860"/05.08.60 属于误读）；个别固件若不带
// 校验（末 2 位与累加校验不吻合）则不剥离，保留全串。
func ParseVERResponse(resp string) string {
	resp = strings.TrimSpace(resp)
	idx := strings.Index(resp, "VER")
	if idx < 0 || len(resp) < idx+5 { // "VER" + 2 位状态位
		return ""
	}
	body := resp[idx+5:]
	// 末 2 位与整帧累加校验吻合 → 是校验位，剥离。
	if len(body) > 2 && strings.EqualFold(Checksum(resp[:len(resp)-2]), resp[len(resp)-2:]) {
		body = body[:len(body)-2]
	}
	return body
}

// Send 发送控制命令（自动追加校验）：
// UDP 通道无 ACK，按固定次数重发；TCP 通道单次发送（多数控制命令无响应，
// 读不到回包不算失败）。
func (c *Client) Send(cmd string) error {
	frame := Frame(cmd)
	if c.transport == TransportTCP {
		conn, err := net.DialTimeout("tcp", c.tcpAddr, c.tcpTimeout)
		if err != nil {
			return fmt.Errorf("tcp dial %s: %w", c.tcpAddr, err)
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(c.tcpTimeout))
		if _, err := conn.Write([]byte(frame)); err != nil {
			return fmt.Errorf("tcp send %q: %w", frame, err)
		}
		c.logger.Debug("skydroid tcp sent", "frame", frame)
		return nil
	}
	for i := 0; i < c.udpRetries; i++ {
		if _, err := c.udpConn.Write([]byte(frame)); err != nil {
			return fmt.Errorf("udp send %q: %w", frame, err)
		}
		c.logger.Debug("skydroid udp sent", "frame", frame, "attempt", i+1)
		if i < c.udpRetries-1 {
			time.Sleep(c.retryTimeout)
		}
	}
	return nil
}

// SetRetryTimeout 调整 UDP 重发间隔（测试用）。
func (c *Client) SetRetryTimeout(d time.Duration) { c.retryTimeout = d }

// QueryTCP 经 TCP 发送并读取 ASCII 响应。
func (c *Client) QueryTCP(cmd string, timeout time.Duration) (string, error) {
	conn, err := net.DialTimeout("tcp", c.tcpAddr, timeout)
	if err != nil {
		return "", fmt.Errorf("tcp dial %s: %w", c.tcpAddr, err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.Write([]byte(cmd)); err != nil {
		return "", fmt.Errorf("tcp send %q: %w", cmd, err)
	}
	c.logger.Debug("skydroid tcp sent", "cmd", cmd)
	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	if err != nil {
		return "", fmt.Errorf("tcp read: %w", err)
	}
	resp := string(buf[:n])
	c.logger.Debug("skydroid tcp recv", "resp", resp)
	return resp, nil
}

// beginUDPQuery 注册 UDP 查询 waiter，返回应答通道；同一时刻只允许一个挂起查询。
func (c *Client) beginUDPQuery() (chan string, error) {
	c.queryMu.Lock()
	defer c.queryMu.Unlock()
	if c.queryWaiter != nil {
		return nil, fmt.Errorf("udp query already pending")
	}
	ch := make(chan string, 1)
	c.queryWaiter = ch
	return ch, nil
}

// endUDPQuery 注销挂起的 waiter。
func (c *Client) endUDPQuery() {
	c.queryMu.Lock()
	c.queryWaiter = nil
	c.queryMu.Unlock()
}

// QueryUDP 经 UDP 发送查询命令（自动追加校验）并等待应答，适用于 UDP-only 机型
// 的读类命令（如 VER 固件版本查询，issue #3）。应答不单独开读 socket，而是由
// readLoop 统一接收、handleFrame 识别后投递给本方法挂起的 waiter。
// timeout 覆盖全部发送尝试并按尝试次数均分：每次等待超时即重发一次以应对 UDP
// 丢包，共 udpRetries 次尝试（默认 3 次，即重发 2 次）。
func (c *Client) QueryUDP(cmd string, timeout time.Duration) (string, error) {
	if c.udpConn == nil {
		return "", fmt.Errorf("udp query unavailable: transport=%s", c.transport)
	}
	ch, err := c.beginUDPQuery()
	if err != nil {
		return "", err
	}
	defer c.endUDPQuery()

	frame := Frame(cmd)
	attempts := c.udpRetries
	if attempts < 1 {
		attempts = 1
	}
	wait := timeout / time.Duration(attempts)
	for i := 0; i < attempts; i++ {
		if _, err := c.udpConn.Write([]byte(frame)); err != nil {
			return "", fmt.Errorf("udp send %q: %w", frame, err)
		}
		c.logger.Debug("skydroid udp sent", "frame", frame, "attempt", i+1)
		select {
		case resp := <-ch:
			c.logger.Debug("skydroid udp recv", "resp", resp)
			return resp, nil
		case <-time.After(wait):
			// 本次尝试未等到应答，继续重发。
		}
	}
	return "", fmt.Errorf("udp query %q: no response within %v", cmd, timeout)
}

// AngleToHex 把角度编码为协议的十六进制文本（SDK String2ByteArrayUtils.short2Hex
// 对齐）：值 = round(angle*100)，取 16 位有符号补码，格式 %04X（4 位）。
// SDK 支持范围：偏航/俯仰均 ±90°。
func AngleToHex(deg float64) string {
	v := int(math.Round(deg * 100))
	return fmt.Sprintf("%04X", uint16(int16(v)))
}

// HexToAngle 把 4 位十六进制文本解码回角度（解析响应用）。
func HexToAngle(hex string) (float64, error) {
	var v int64
	if _, err := fmt.Sscanf(hex, "%X", &v); err != nil {
		return 0, fmt.Errorf("parse angle hex %q: %w", hex, err)
	}
	if v > 0xFFFF/2 {
		v = v - 0xFFFF - 1
	}
	return float64(v) / 100, nil
}

// 角度范围（SDK gotoYaw/gotoPitch 钳位）。
const (
	AngleMax = 90.0
	AngleMin = -90.0
)

// SpeedSigned 把 [-1,1] 的速度分量映射为 GSY/GSP 的 1 字节带符号补码
// （±100 刻度），返回 2 位十六进制文本。正值右转/上仰。
func SpeedSigned(v float64) string {
	if v > 1 {
		v = 1
	}
	if v < -1 {
		v = -1
	}
	s := int(v*100 + 0.5)
	if v < 0 {
		s = int(v*100 - 0.5)
	}
	return fmt.Sprintf("%02X", byte(int8(s)))
}
