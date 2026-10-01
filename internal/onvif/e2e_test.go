package onvif

import (
	"encoding/xml"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/linuxsuren/skydroid-onvif-adapter/internal/ptz"
	"github.com/linuxsuren/skydroid-onvif-adapter/internal/skydroid"
)

// ---- 测试脚手架：假 Skydroid 相机 + 适配器 ----

type fakeCam struct {
	udpPort  int
	tcpPort  int
	commands chan string
}

func startFakeCam(t *testing.T) *fakeCam {
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
	fc := &fakeCam{
		udpPort:  uconn.LocalAddr().(*net.UDPAddr).Port,
		tcpPort:  tln.Addr().(*net.TCPAddr).Port,
		commands: make(chan string, 256),
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
			// 模拟云台姿态上报。
			att := "#tpUG2rGAC" + skydroid.AngleToHex(12.5) + skydroid.AngleToHex(-30) + skydroid.AngleToHex(0) + "AA"
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
				if strings.HasPrefix(string(buf[:n]), skydroid.CmdGetVersion) {
					_, _ = c.Write([]byte(skydroid.CmdGetVersion + "=V2.1.7"))
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

func (fc *fakeCam) addr(port int) string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
}

// awaitCmd 等待假相机收到指定命令。
func (fc *fakeCam) awaitCmd(t *testing.T, want string) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	seen := map[string]int{}
	for {
		select {
		case cmd := <-fc.commands:
			seen[cmd]++
			if cmd == want {
				return
			}
		case <-deadline:
			t.Fatalf("camera did not receive %q; saw: %v", want, seen)
		}
	}
}

type testRig struct {
	svc *Service
	cam *fakeCam
	ts  *httptest.Server
}

func newRig(t *testing.T) *testRig {
	t.Helper()
	cam := startFakeCam(t)
	client, err := skydroid.NewClient(skydroid.TransportUDP, cam.addr(cam.udpPort), cam.addr(cam.tcpPort), 0,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	client.SetRetryTimeout(time.Millisecond)
	t.Cleanup(func() { _ = client.Close() })

	driver := ptz.NewSkydroidDriver(client)
	cfg := Config{
		Manufacturer:   "Skydroid",
		Model:          "C20",
		Firmware:       "V2.1.7",
		Serial:         "SKYDROID-TEST",
		AdvertiseIP:    "192.0.2.10",
		HTTPPort:       8080,
		CameraRTSPBase: "192.168.144.108:554",
	}
	svc := NewService(cfg, driver, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(svc.Handler())
	t.Cleanup(ts.Close)
	return &testRig{svc: svc, cam: cam, ts: ts}
}

// soap 发送 SOAP 请求并返回响应 Body innerxml。
func soap(t *testing.T, rig *testRig, inner string) string {
	t.Helper()
	body := `<?xml version="1.0"?>` +
		`<Envelope xmlns="http://www.w3.org/2003/05/soap-envelope"><Body>` + inner + `</Body></Envelope>`
	resp, err := rig.ts.Client().Post(rig.ts.URL+"/onvif/device_service",
		"application/soap+xml; charset=utf-8", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	var env struct {
		Body struct {
			Inner []byte `xml:",innerxml"`
		} `xml:"Body"`
	}
	if err := xml.Unmarshal(data, &env); err != nil {
		t.Fatalf("bad envelope: %v\n%s", err, data)
	}
	return string(env.Body.Inner)
}

func op(t *testing.T, rig *testRig, prefix, op string, ns, children string) string {
	t.Helper()
	return soap(t, rig, `<`+prefix+`:`+op+` xmlns:`+prefix+`="`+ns+`">`+children+`</`+prefix+`:`+op+`>`)
}

// ---- 用例 ----

func TestE2EDeviceInfoAndStreams(t *testing.T) {
	rig := newRig(t)
	var dev struct {
		XMLName      xml.Name `xml:"GetDeviceInformationResponse"`
		Manufacturer string   `xml:"Manufacturer"`
		Model        string   `xml:"Model"`
		Firmware     string   `xml:"FirmwareVersion"`
		Serial       string   `xml:"SerialNumber"`
	}
	body := op(t, rig, "tds", "GetDeviceInformation", nsDevice, "")
	if err := xml.Unmarshal([]byte(body), &dev); err != nil {
		t.Fatalf("parse: %v\n%s", err, body)
	}
	if dev.Manufacturer != "Skydroid" || dev.Model != "C20" || dev.Firmware != "V2.1.7" {
		t.Fatalf("dev info: %+v", dev)
	}

	var prof struct {
		Profiles []struct {
			Token string `xml:"token,attr"`
			Name  string `xml:"Name"`
		} `xml:"Profiles"`
	}
	body = op(t, rig, "trt", "GetProfiles", nsMedia, "")
	if err := xml.Unmarshal([]byte(body), &prof); err != nil || len(prof.Profiles) != 2 {
		t.Fatalf("profiles: %v %s", err, body)
	}

	type mediaURIResp struct {
		MediaURI struct {
			URI string `xml:"Uri"`
		} `xml:"MediaUri"`
	}
	var stream mediaURIResp
	body = op(t, rig, "trt", "GetStreamUri", nsMedia,
		`<trt:ProfileToken>profile_main</trt:ProfileToken>`)
	if err := xml.Unmarshal([]byte(body), &stream); err != nil || stream.MediaURI.URI != "rtsp://192.168.144.108:554/stream=0" {
		t.Fatalf("stream uri: %v %s", err, body)
	}
	var snap mediaURIResp
	body = op(t, rig, "trt", "GetSnapshotUri", nsMedia,
		`<trt:ProfileToken>profile_main</trt:ProfileToken>`)
	if err := xml.Unmarshal([]byte(body), &snap); err != nil || snap.MediaURI.URI != "http://192.0.2.10:8080/onvif/snapshot/profile_main" {
		t.Fatalf("snapshot uri: %v %s", err, body)
	}
}

// TestE2EContinuousMoveToCamera 断言 ONVIF ContinuousMove 被翻译为
// 速度设置 + 八方向恒动命令，Stop 被翻译为 PTZ00。
func TestE2EContinuousMoveToCamera(t *testing.T) {
	rig := newRig(t)

	body := op(t, rig, "tptz", "ContinuousMove", nsPTZ,
		`<tptz:ProfileToken>profile_main</tptz:ProfileToken>`+
			`<tptz:Velocity><tt:PanTilt x="0.8" y="0" xmlns:tt="http://www.onvif.org/ver10/schema"/>`+
			`<tt:Zoom x="0" xmlns:tt="http://www.onvif.org/ver10/schema"/></tptz:Velocity>`)
	if !strings.Contains(body, "ContinuousMoveResponse") {
		t.Fatalf("resp: %s", body)
	}
	// 两轴独立带符号速度：pan 0.8 → +80 → 0x50；tilt 0 → 0x00（均带校验）。
	rig.cam.awaitCmd(t, skydroid.Frame("#TPUG2wGSY50"))
	rig.cam.awaitCmd(t, skydroid.Frame("#TPUG2wGSP00"))

	// Zoom 单独。
	op(t, rig, "tptz", "ContinuousMove", nsPTZ,
		`<tptz:ProfileToken>profile_main</tptz:ProfileToken>`+
			`<tptz:Velocity><tt:PanTilt x="0" y="0" xmlns:tt="http://www.onvif.org/ver10/schema"/>`+
			`<tt:Zoom x="0.6" xmlns:tt="http://www.onvif.org/ver10/schema"/></tptz:Velocity>`)
	rig.cam.awaitCmd(t, skydroid.Frame(skydroid.CmdPTZZoomIn))

	op(t, rig, "tptz", "Stop", nsPTZ, `<tptz:ProfileToken>profile_main</tptz:ProfileToken>`)
	rig.cam.awaitCmd(t, skydroid.Frame(skydroid.CmdPTZStop))
}

// TestE2EAbsoluteMoveToCamera 断言绝对转角命令的字节级编码正确下发。
func TestE2EAbsoluteMoveToCamera(t *testing.T) {
	rig := newRig(t)
	op(t, rig, "tptz", "AbsoluteMove", nsPTZ,
		`<tptz:ProfileToken>profile_main</tptz:ProfileToken>`+
			`<tptz:Position><tt:PanTilt x="30" y="-45.5" xmlns:tt="http://www.onvif.org/ver10/schema"/>`+
			`<tt:Zoom x="0" xmlns:tt="http://www.onvif.org/ver10/schema"/></tptz:Position>`)
	rig.cam.awaitCmd(t, skydroid.Frame("#TPUG6wGAY0BB8")) // 30°
	rig.cam.awaitCmd(t, skydroid.Frame("#TPUG6wGAPEE3A")) // -45.5°

	// GetStatus 返回估算位置。
	type statusResp struct {
		Status struct {
			Position struct {
				PanTilt struct {
					X float64 `xml:"x,attr"`
					Y float64 `xml:"y,attr"`
				} `xml:"PanTilt"`
			} `xml:"Position"`
			MoveStatus struct {
				PanTilt string `xml:"PanTilt"`
			} `xml:"MoveStatus"`
		} `xml:"PTZStatus"`
	}
	var st statusResp
	body := op(t, rig, "tptz", "GetStatus", nsPTZ, `<tptz:ProfileToken>profile_main</tptz:ProfileToken>`)
	if err := xml.Unmarshal([]byte(body), &st); err != nil {
		t.Fatalf("parse status: %v\n%s", err, body)
	}
	// 云台姿态上报新鲜时 GetStatus 返回真实角度（假相机回推 12.5/-30），
	// 而非刚下发的目标值——与真机行为一致（云台尚在飞向目标途中）。
	if st.Status.Position.PanTilt.X != 12.5 || st.Status.Position.PanTilt.Y != -30 {
		t.Fatalf("status should report real attitude: %+v", st.Status.Position)
	}
	if st.Status.MoveStatus.PanTilt != "IDLE" {
		t.Fatalf("move status: %+v", st.Status.MoveStatus)
	}
}

// TestE2EPresets 覆盖预置位全生命周期与内建模式位。
func TestE2EPresets(t *testing.T) {
	rig := newRig(t)

	// 先绝对定位再存预置位。
	op(t, rig, "tptz", "AbsoluteMove", nsPTZ,
		`<tptz:ProfileToken>profile_main</tptz:ProfileToken>`+
			`<tptz:Position><tt:PanTilt x="60" y="-10" xmlns:tt="http://www.onvif.org/ver10/schema"/>`+
			`<tt:Zoom x="0" xmlns:tt="http://www.onvif.org/ver10/schema"/></tptz:Position>`)
	rig.cam.awaitCmd(t, skydroid.Frame("#TPUG6wGAY1770")) // 60° → 6000 = 0x1770

	var setResp struct {
		Token string `xml:"PresetToken"`
	}
	body := op(t, rig, "tptz", "SetPreset", nsPTZ,
		`<tptz:ProfileToken>profile_main</tptz:ProfileToken><tptz:PresetName>门口</tptz:PresetName>`)
	if err := xml.Unmarshal([]byte(body), &setResp); err != nil || setResp.Token == "" {
		t.Fatalf("set preset: %v %s", err, body)
	}

	// 调用自定义预置位 → 角度回放。
	op(t, rig, "tptz", "GotoPreset", nsPTZ,
		`<tptz:ProfileToken>profile_main</tptz:ProfileToken><tptz:PresetToken>`+setResp.Token+`</tptz:PresetToken>`)
	rig.cam.awaitCmd(t, skydroid.Frame("#TPUG6wGAY1770"))

	// 内建 Follow 模式位。
	op(t, rig, "tptz", "GotoPreset", nsPTZ,
		`<tptz:ProfileToken>profile_main</tptz:ProfileToken><tptz:PresetToken>4</tptz:PresetToken>`)
	rig.cam.awaitCmd(t, skydroid.Frame(skydroid.CmdPTZFollow))

	// Home 未设置 → 硬件回中。
	op(t, rig, "tptz", "GotoHomePosition", nsPTZ, `<tptz:ProfileToken>profile_main</tptz:ProfileToken>`)
	rig.cam.awaitCmd(t, skydroid.Frame(skydroid.CmdPTZCenter))
}

// TestE2EGetStatusRealAttitude 云台姿态上报新鲜时 GetStatus 返回真实角度。
func TestE2EGetStatusRealAttitude(t *testing.T) {
	rig := newRig(t)
	// 触发一次命令使假相机回推姿态。
	op(t, rig, "tptz", "Stop", nsPTZ, `<tptz:ProfileToken>profile_main</tptz:ProfileToken>`)
	time.Sleep(300 * time.Millisecond)

	type statusResp struct {
		Status struct {
			Position struct {
				PanTilt struct {
					X float64 `xml:"x,attr"`
					Y float64 `xml:"y,attr"`
				} `xml:"PanTilt"`
			} `xml:"Position"`
		} `xml:"PTZStatus"`
	}
	var st statusResp
	body := op(t, rig, "tptz", "GetStatus", nsPTZ, `<tptz:ProfileToken>profile_main</tptz:ProfileToken>`)
	if err := xml.Unmarshal([]byte(body), &st); err != nil {
		t.Fatalf("parse: %v\n%s", err, body)
	}
	if st.Status.Position.PanTilt.X != 12.5 || st.Status.Position.PanTilt.Y != -30 {
		t.Fatalf("want real attitude 12.5/-30, got %v/%v",
			st.Status.Position.PanTilt.X, st.Status.Position.PanTilt.Y)
	}
}

func TestE2EUnknownOpFault(t *testing.T) {
	rig := newRig(t)
	body := soap(t, rig, `<tds:NotExist xmlns:tds="`+nsDevice+`"/>`)
	if !strings.Contains(body, "ActionNotSupported") {
		t.Fatalf("fault: %s", body)
	}
}
