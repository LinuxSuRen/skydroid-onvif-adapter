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

// readLoop 接收云台上报帧，解析姿态（#tp*GAC<yaw4><pitch4><roll4><crc2>）。
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

func (c *Client) handleFrame(frame string) {
	if len(frame) < 4 || frame[0] != '#' || (frame[1] != 't' && frame[1] != 'T') {
		return
	}
	idx := strings.Index(frame, "GAC")
	if idx < 0 || len(frame) < idx+3+12 {
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
