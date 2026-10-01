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
