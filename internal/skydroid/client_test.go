package skydroid

import (
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeCamera 模拟相机的 UDP 命令与 TCP 查询通道，记录收到的完整帧。
type fakeCamera struct {
	udpPort  int
	tcpPort  int
	commands chan string
}

func startFakeCamera(t *testing.T) *fakeCamera {
	t.Helper()
	uconn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	tln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = uconn.Close()
		t.Fatal(err)
	}
	fc := &fakeCamera{
		udpPort:  uconn.LocalAddr().(*net.UDPAddr).Port,
		tcpPort:  tln.Addr().(*net.TCPAddr).Port,
		commands: make(chan string, 64),
	}
	go func() {
		buf := make([]byte, 256)
		for {
			n, from, err := uconn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			select {
			case fc.commands <- string(buf[:n]):
			default:
			}
			// 模拟云台姿态上报：#tpUG2rGAC<yaw><pitch><roll>（16 位编码 ×100）。
			att := "#tpUG2rGAC" + AngleToHex(12.5) + AngleToHex(-30) + AngleToHex(0) + "AA"
			_, _ = uconn.WriteToUDP([]byte(att), from)
		}
	}()
	go func() {
		for {
			conn, err := tln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				buf := make([]byte, 256)
				_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
				n, _ := c.Read(buf)
				cmd := string(buf[:n])
				select {
				case fc.commands <- cmd:
				default:
				}
				if strings.HasPrefix(cmd, Frame(CmdGetVersion)) {
					_, _ = c.Write([]byte(cmd + "=V2.1.7"))
				}
			}(conn)
		}
	}()
	t.Cleanup(func() {
		_ = uconn.Close()
		_ = tln.Close()
	})
	return fc
}

func (fc *fakeCamera) addr(port int) string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
}

// TestChecksumOfficialSamples 用官方 rcsdk-demo 中的真实命令逐一验证校验算法。
func TestChecksumOfficialSamples(t *testing.T) {
	cases := []struct{ cmd, cs string }{
		// 来自 gitee.com/skydroid/rcsdk-demo HomeActivity.kt
		{"#TPUD2wCAP01", "3E"}, // 拍照
		{"#TPUD2wREC01", "44"}, // 开始录像
		{"#TPUD2wREC00", "43"}, // 停止录像
		{"#TPUG2wGSY64", "69"}, // 偏航速度 +100（右）
		{"#TPUG2wGSY9C", "7B"}, // 偏航速度 -100（左）
		{"#TPUG2wGSP64", "60"}, // 俯仰速度 +100（上）
		{"#TPUG2wGSP9C", "72"}, // 俯仰速度 -100（下）
		{"#TPUG2wGSY1E", "75"}, // 偏航速度 +30
		{"#TPUG2wGSYE2", "76"}, // 偏航速度 -30
		{"#TPUG2wGSP1E", "6C"}, // 俯仰速度 +30
		{"#TPUG2wGSPE2", "6D"}, // 俯仰速度 -30
	}
	for _, c := range cases {
		if got := Checksum(c.cmd); got != c.cs {
			t.Fatalf("Checksum(%s) = %s, want %s", c.cmd, got, c.cs)
		}
	}
}

func TestFrame(t *testing.T) {
	if got := Frame("#TPUD2wCAP01"); got != "#TPUD2wCAP013E" {
		t.Fatalf("Frame = %s", got)
	}
}

func TestSendUDPDeliversWithChecksum(t *testing.T) {
	fc := startFakeCamera(t)
	c, err := NewClient(TransportUDP, fc.addr(fc.udpPort), fc.addr(fc.tcpPort), 0, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	c.SetRetryTimeout(time.Millisecond)

	if err := c.Send(CmdPTZStop); err != nil {
		t.Fatal(err)
	}
	want := Frame(CmdPTZStop)
	select {
	case got := <-fc.commands:
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("no command received")
	}
}

func TestSendUDPRetries(t *testing.T) {
	fc := startFakeCamera(t)
	c, err := NewClient(TransportUDP, fc.addr(fc.udpPort), fc.addr(fc.tcpPort), 0, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	c.SetRetryTimeout(time.Millisecond)

	if err := c.Send(CmdPTZLeft); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		select {
		case <-fc.commands:
		case <-time.After(time.Second):
			t.Fatalf("attempt %d not received", i+1)
		}
	}
}

// TestSendTCPMode TCP 通道（C20）：命令经 TCP 发送且带校验。
func TestSendTCPMode(t *testing.T) {
	fc := startFakeCamera(t)
	c, err := NewClient(TransportTCP, fc.addr(fc.udpPort), fc.addr(fc.tcpPort), 0, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	if err := c.Send(CmdPTZCenter); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-fc.commands:
		if got != Frame(CmdPTZCenter) {
			t.Fatalf("got %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("tcp command not received")
	}
}

func TestQueryTCPVersion(t *testing.T) {
	fc := startFakeCamera(t)
	c, err := NewClient(TransportUDP, fc.addr(fc.udpPort), fc.addr(fc.tcpPort), 0, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	resp, err := c.QueryTCP(Frame(CmdGetVersion), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp, "V2.1.7") {
		t.Fatalf("resp = %q", resp)
	}
}

func TestQueryTCPRefused(t *testing.T) {
	c, err := NewClient(TransportUDP, "127.0.0.1:19002", "127.0.0.1:19055", 0, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.QueryTCP(Frame(CmdGetVersion), 500*time.Millisecond); err == nil {
		t.Fatal("want error for refused tcp")
	}
}

func TestAngleToHex(t *testing.T) {
	cases := []struct {
		deg  float64
		want string
	}{
		{0, "0000"},
		{30, "0BB8"},  // 3000
		{60, "1770"},  // 6000
		{90, "2328"},  // 9000（SDK 钳位上限）
		{-90, "DCD8"}, // 16 位补码
		{-45.5, "EE3A"},
	}
	for _, c := range cases {
		if got := AngleToHex(c.deg); got != c.want {
			t.Fatalf("AngleToHex(%v) = %s, want %s", c.deg, got, c.want)
		}
	}
}

func TestHexToAngleRoundTrip(t *testing.T) {
	for _, deg := range []float64{0, 30, -30, 90, -90, 180, -45.5} {
		hex := AngleToHex(deg)
		back, err := HexToAngle(hex)
		if err != nil {
			t.Fatalf("HexToAngle(%s): %v", hex, err)
		}
		if diff := back - deg; diff > 0.01 || diff < -0.01 {
			t.Fatalf("round trip %v → %s → %v", deg, hex, back)
		}
	}
}

// TestSpeedSigned 与官方 demo 对齐：±100 刻度带符号补码。
func TestSpeedSigned(t *testing.T) {
	cases := []struct {
		v    float64
		want string
	}{
		{1, "64"},   // +100 → 0x64
		{-1, "9C"},  // -100 → 0x9C
		{0.3, "1E"}, // +30
		{-0.3, "E2"},
		{0.8, "50"}, // +80
		{-0.5, "CE"},
		{0, "00"},
		{2, "64"}, // 钳位
		{-2, "9C"},
	}
	for _, c := range cases {
		if got := SpeedSigned(c.v); got != c.want {
			t.Fatalf("SpeedSigned(%v) = %s, want %s", c.v, got, c.want)
		}
	}
}

// TestHandleAttitudeFrame 姿态帧解析（帧格式来自 SDK TopParser/GAC 逆向）。
func TestHandleAttitudeFrame(t *testing.T) {
	c := &Client{logger: quietLogger()}
	frame := "#tpUG2rGAC" + AngleToHex(12.5) + AngleToHex(-30.25) + AngleToHex(1.5) + "ZZ"
	c.handleFrame(frame)
	yaw, pitch, roll, fresh := c.Attitude(2 * time.Second)
	if !fresh || yaw != 12.5 || pitch != -30.25 || roll != 1.5 {
		t.Fatalf("attitude = %v/%v/%v fresh=%v", yaw, pitch, roll, fresh)
	}
	// 非法帧被忽略。
	c.handleFrame("garbage")
	c.handleFrame("#tpUG2rGACZZ")
	if _, _, _, fresh := c.Attitude(2 * time.Second); !fresh {
		t.Fatal("valid attitude lost")
	}
}

// TestAttitudeFromUDP 假相机回推姿态后客户端可读到真实角度。
func TestAttitudeFromUDP(t *testing.T) {
	fc := startFakeCamera(t)
	listen := freeTCPPortForUDP(t)
	c, err := NewClient(TransportUDP, fc.addr(fc.udpPort), fc.addr(fc.tcpPort), listen, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	c.SetRetryTimeout(time.Millisecond)

	if err := c.Send(CmdPTZStop); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(3 * time.Second)
	for {
		if _, _, _, fresh := c.Attitude(2 * time.Second); fresh {
			yaw, pitch, _, _ := c.Attitude(2 * time.Second)
			if yaw != 12.5 || pitch != -30 {
				t.Fatalf("yaw=%v pitch=%v", yaw, pitch)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("attitude not received")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func freeTCPPortForUDP(t *testing.T) int {
	t.Helper()
	ln, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	port := ln.LocalAddr().(*net.UDPAddr).Port
	_ = ln.Close()
	return port
}

// --- UDP 查询（issue #3：固件版本查询失败时回退 UDP 通道） ---

// realVERResponse 真机（UDP-only 机型）经 5000/UDP 查询固件版本的应答原文。
const realVERResponse = "#tpDU3rVER01050860"

// TestVERResponseChecksum 实测应答帧的校验结论：末 2 位 "60" 恰好等于去掉末 2 位
// 后整帧（含 '#'）全部 ASCII 字节的累加校验（Checksum("#tpDU3rVER010508") == "60"），
// 与 GAC 姿态上报帧（#tp*GAC...<crc2>）一致——即 VER 应答帧带 2 位尾校验。
// 因此解析时校验并剥离末 2 位再取版本号：真机应答的版本数据实为 "0508"
// （固件 05.08；把校验位 "60" 一并读成 "050860"/05.08.60 属于误读）。
func TestVERResponseChecksum(t *testing.T) {
	if cs := Checksum(realVERResponse[:len(realVERResponse)-2]); cs != realVERResponse[len(realVERResponse)-2:] {
		t.Fatalf("Checksum(%q) = %s, trailing = %s（不吻合：应答不带校验，应改为不剥离解析）",
			realVERResponse[:len(realVERResponse)-2], cs, realVERResponse[len(realVERResponse)-2:])
	}
	// 结论吻合：真机应答解析出的版本数据为 "0508"。
	if got := ParseVERResponse(realVERResponse); got != "0508" {
		t.Fatalf("ParseVERResponse(real) = %q, want 0508", got)
	}
}

// TestParseVERResponse 按「末 2 位校验吻合则剥离」规则解析各类应答帧。
func TestParseVERResponse(t *testing.T) {
	// 按校验结论构造的、版本数据为 "050860" 的正确帧（末 2 位为整帧累加校验）。
	base := "#tpDU3rVER01" + "050860"
	cases := []struct{ resp, want string }{
		{realVERResponse, "0508"},         // 真机原文：末 2 位 "60" 是校验
		{base + Checksum(base), "050860"}, // 版本 050860 + 正确校验
		{"#tpDU3rVER01ABCD", "ABCD"},      // 末 2 位非有效校验（Checksum(...)=59≠CD）：不剥离
		{"#tpDU3rVER01", ""},              // 无版本数据
		{"garbage", ""},                   // 非应答帧
		{"#tpUG2rGAC000000000000AA", ""},  // GAC 姿态帧不含版本
	}
	for _, c := range cases {
		if got := ParseVERResponse(c.resp); got != c.want {
			t.Fatalf("ParseVERResponse(%q) = %q, want %q", c.resp, got, c.want)
		}
	}
}

// fakeUDPCamera 最小假相机：把收到的包写入 commands，再按 replies 顺序回包
//（replies 为空即只收不应答）。
type fakeUDPCamera struct {
	addr     string
	commands chan string
}

func startFakeUDPCamera(t *testing.T, replies ...string) *fakeUDPCamera {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	fc := &fakeUDPCamera{
		addr:     conn.LocalAddr().String(),
		commands: make(chan string, 64),
	}
	go func() {
		buf := make([]byte, 256)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			select {
			case fc.commands <- string(buf[:n]):
			default:
			}
			for _, r := range replies {
				_, _ = conn.WriteToUDP([]byte(r), from)
			}
		}
	}()
	t.Cleanup(func() { _ = conn.Close() })
	return fc
}

// TestQueryUDPVersion 假相机对收到的包先回 GAC 姿态帧、再回带正确校验的 VER 应答：
// QueryUDP 应拿到版本应答（与 readLoop 共存、不抢包），解析出版本号 "050860"，
// 且 readLoop 的姿态解析不受影响。
func TestQueryUDPVersion(t *testing.T) {
	base := "#tpDU3rVER01" + "050860"
	verFrame := base + Checksum(base) // "#tpDU3rVER01050860C6"
	fc := startFakeUDPCamera(t,
		"#tpUG2rGAC"+AngleToHex(12.5)+AngleToHex(-30)+AngleToHex(0)+"AA",
		verFrame)
	c, err := NewClient(TransportUDP, fc.addr, "127.0.0.1:19055", 0, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	resp, err := c.QueryUDP(CmdGetVersion, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if resp != verFrame {
		t.Fatalf("resp = %q, want %q", resp, verFrame)
	}
	if got := ParseVERResponse(resp); got != "050860" {
		t.Fatalf("version = %q, want 050860", got)
	}
	// 命令必须自动追加校验后发送。
	select {
	case got := <-fc.commands:
		if got != Frame(CmdGetVersion) {
			t.Fatalf("cmd = %q, want %q", got, Frame(CmdGetVersion))
		}
	default:
		t.Fatal("no command received")
	}
	// 同一 socket 上 readLoop 的姿态解析照常工作（GAC 帧不满足 VER waiter）。
	deadline := time.After(3 * time.Second)
	for {
		if _, _, _, fresh := c.Attitude(2 * time.Second); fresh {
			return
		}
		select {
		case <-deadline:
			t.Fatal("attitude not received alongside udp query")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// TestQueryUDPTimeout 假相机只收不应答时，QueryUDP 按传入的小超时（按尝试次数
// 均分后逐次等待并重发）在时限内返回错误，无需额外可调参数。
func TestQueryUDPTimeout(t *testing.T) {
	fc := startFakeUDPCamera(t) // 不回任何包
	c, err := NewClient(TransportUDP, fc.addr, "127.0.0.1:19055", 0, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	start := time.Now()
	if _, err := c.QueryUDP(CmdGetVersion, 450*time.Millisecond); err == nil {
		t.Fatal("want error for udp query timeout")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("timeout took too long: %v", d)
	}
	// 超时窗口内应完成全部重发尝试（默认 3 次）。
	for i := 0; i < 3; i++ {
		select {
		case <-fc.commands:
		case <-time.After(time.Second):
			t.Fatalf("attempt %d not received", i+1)
		}
	}
}

// TestQueryUDPRequiresUDPTransport TCP 模式客户端没有 UDP 连接，QueryUDP 直接报错。
func TestQueryUDPRequiresUDPTransport(t *testing.T) {
	c, err := NewClient(TransportTCP, "127.0.0.1:19002", "127.0.0.1:19055", 0, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.QueryUDP(CmdGetVersion, time.Second); err == nil {
		t.Fatal("want error without udp transport")
	}
}
