// skydroid-onvif-adapter 把 Skydroid（云卓）云台相机映射为标准 ONVIF 设备：
// PTZ 控制翻译为 TP 协议（UDP 9002），视频/快照直连相机 RTSP。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/linuxsuren/skydroid-onvif-adapter/internal/discovery"
	"github.com/linuxsuren/skydroid-onvif-adapter/internal/onvif"
	"github.com/linuxsuren/skydroid-onvif-adapter/internal/ptz"
	"github.com/linuxsuren/skydroid-onvif-adapter/internal/skydroid"
	"github.com/linuxsuren/skydroid-onvif-adapter/internal/snapshot"
)

var (
	version   = "dev"
	commit    = "none"
	buildDate = "unknown"
)

const maxPortDrift = 20

func main() {
	var (
		httpAddr        = flag.String("http-addr", envOr("HTTP_ADDR", ":8080"), "HTTP 监听地址（SOAP/快照/健康检查）")
		advertiseIP     = flag.String("advertise-ip", envOr("ADVERTISE_IP", ""), "对外宣告 IP，留空自动探测")
		cameraIP        = flag.String("camera-ip", envOr("CAMERA_IP", "192.168.144.108"), "Skydroid 相机 IP")
		cameraTransport = flag.String("camera-transport", envOr("CAMERA_TRANSPORT", "udp"), "命令通道：udp（C10Pro，默认 5000）| tcp（C20 相机 8100 / 云台 5000）")
		cameraUDPPort   = flag.Int("camera-udp-port", envInt("CAMERA_UDP_PORT", 5000), "相机 TP 命令 UDP 端口（官方 C10Pro 默认 5000）")
		listenUDPPort   = flag.Int("listen-udp-port", envInt("LISTEN_UDP_PORT", 5000), "本地 UDP 监听端口（接收云台姿态上报，官方 SDK 默认 5000；被占则降级无姿态）")
		cameraTCPPort   = flag.Int("camera-tcp-port", envInt("CAMERA_TCP_PORT", 8100), "相机 TCP 端口（官方 C20 相机默认 8100，云台 5000）")
		cameraName      = flag.String("camera-name", envOr("CAMERA_NAME", "Skydroid 云台相机"), "设备名称（scope 展示）")
		manufacturer    = flag.String("manufacturer", "Skydroid", "ONVIF Manufacturer 字段")
		model           = flag.String("model", envOr("CAMERA_MODEL", "C20"), "ONVIF Model 字段")
		serial          = flag.String("serial", envOr("CAMERA_SERIAL", ""), "ONVIF SerialNumber 字段，默认自动生成")
		rtspBase        = flag.String("rtsp-base", envOr("CAMERA_RTSP_BASE", ""), "相机 RTSP 基址（ip:port），默认 <camera-ip>:554")
		ffmpegBin       = flag.String("ffmpeg-bin", envOr("FFMPEG_BIN", "ffmpeg"), "ffmpeg 路径（快照抓帧用）")
		discoveryOn     = flag.Bool("discovery", envBool("DISCOVERY", true), "是否开启 WS-Discovery")
		logLevel        = flag.String("log-level", envOr("LOG_LEVEL", "info"), "日志级别 debug|info|warn|error")
		probe           = flag.Bool("probe", false, "探测相机连通性并打印结果后退出")
		showVersion     = flag.Bool("version", false, "打印版本并退出")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("skydroid-onvif-adapter %s (commit %s, built %s)\n", version, commit, buildDate)
		return
	}

	logger := newLogger(*logLevel)
	slog.SetDefault(logger)

	udpAddr := net.JoinHostPort(*cameraIP, strconv.Itoa(*cameraUDPPort))
	tcpAddr := net.JoinHostPort(*cameraIP, strconv.Itoa(*cameraTCPPort))
	client, err := skydroid.NewClient(skydroid.Transport(*cameraTransport), udpAddr, tcpAddr, *listenUDPPort, logger)
	if err != nil {
		logger.Error("create skydroid client failed", "err", err.Error())
		os.Exit(1)
	}
	defer func() { _ = client.Close() }()

	// 探测模式：真机联调用。
	if *probe {
		runProbe(client, *cameraIP)
		return
	}

	firmware := queryFirmware(client, logger)
	if *serial == "" {
		*serial = "SKYDROID-" + strings.ReplaceAll(*cameraIP, ".", "-")
	}
	if *rtspBase == "" {
		*rtspBase = net.JoinHostPort(*cameraIP, "554")
	}

	ip := *advertiseIP
	if ip == "" {
		ip = detectAdvertiseIP()
	}
	ln, httpPort, drifted, err := listenWithDrift(*httpAddr, maxPortDrift, logger)
	if err != nil {
		logger.Error("http listen failed", "err", err.Error())
		os.Exit(1)
	}
	if drifted {
		logger.Warn("http port drifted", "from", *httpAddr, "to", ln.Addr().String())
	}

	cfg := onvif.Config{
		Manufacturer:   *manufacturer,
		Model:          *model,
		Firmware:       firmware,
		Serial:         *serial,
		AdvertiseIP:    ip,
		HTTPPort:       httpPort,
		CameraRTSPBase: *rtspBase,
		Profiles: []onvif.Profile{
			{Token: "profile_main", Name: *cameraName + " 主码流", StreamIndex: 0, Width: 1920, Height: 1080, Framerate: 30},
			{Token: "profile_sub", Name: *cameraName + " 子码流", StreamIndex: 1, Width: 640, Height: 360, Framerate: 15},
		},
	}

	driver := ptz.NewSkydroidDriver(client)
	driverLogger := slog.New(logger.Handler())
	driver.SetLogger(driverLogger)
	svc := onvif.NewService(cfg, driver, logger)
	snaps := snapshot.New(*ffmpegBin, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mux := http.NewServeMux()
	mux.Handle("/onvif/", svc.Handler())
	mux.HandleFunc("GET /onvif/snapshot/{token}", func(w http.ResponseWriter, r *http.Request) {
		p, ok := svc.ProfileByToken(r.PathValue("token"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		data, err := snaps.JPEG(fmt.Sprintf("rtsp://%s/stream=%d", cfg.CameraRTSPBase, p.StreamIndex))
		if err != nil {
			logger.Warn("snapshot failed", "err", err.Error())
			http.Error(w, "snapshot failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = w.Write(data)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	// 注意：不能写成 "GET /"，会与 "/onvif/"（全方法、更窄路径）构成注册冲突。
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"service":        "skydroid-onvif-adapter",
			"version":        version,
			"camera_ip":      *cameraIP,
			"onvif_endpoint": cfg.XAddr(),
			"stream_uri":     fmt.Sprintf("rtsp://%s/stream=0", cfg.CameraRTSPBase),
			"firmware":       firmware,
		})
	})

	httpServer := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	if *discoveryOn {
		scopes := []string{
			"onvif://www.onvif.org/type/NetworkVideoTransmitter",
			"onvif.org/type/ptz",
			"onvif://www.onvif.org/Profile/Streaming",
			"onvif://www.onvif.org/name/" + *cameraName,
		}
		responder := discovery.New("urn:uuid:skydroid-"+cfg.Serial, cfg.XAddr(), scopes, logger)
		go responder.Run(ctx)
	} else {
		logger.Info("ws-discovery disabled")
	}

	go func() {
		logger.Info("skydroid onvif adapter starting",
			"addr", ln.Addr().String(),
			"onvif", cfg.XAddr(),
			"camera", udpAddr,
			"stream", fmt.Sprintf("rtsp://%s/stream=0", cfg.CameraRTSPBase),
			"firmware", firmware)
		if err := httpServer.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err.Error())
			cancel()
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	logger.Info("shutting down")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer shutdownCancel()
	_ = httpServer.Shutdown(shutdownCtx)
	_ = client.Close()
	logger.Info("stopped")
}

// runProbe 真机探测：TCP 查版本 + UDP 停止命令（安全），打印结果。
func runProbe(client *skydroid.Client, cameraIP string) {
	fmt.Printf("探测相机 %s ...\n", cameraIP)
	resp, err := client.QueryTCP(skydroid.CmdGetVersion, 3*time.Second)
	if err != nil {
		fmt.Printf("  TCP 查询失败: %v\n", err)
	} else {
		fmt.Printf("  TCP 响应: %s\n", strings.TrimSpace(resp))
	}
	if err := client.Send(skydroid.CmdPTZStop); err != nil {
		fmt.Printf("  UDP 发送失败: %v\n", err)
	} else {
		fmt.Println("  UDP 已发送停止命令（PTZ00）×3")
	}
	fmt.Println("提示：连上相机 RTSP（rtsp://" + cameraIP + ":554/stream=0）可验证视频链路。")
}

// queryFirmware 启动时查询固件版本（失败不阻塞启动）。
func queryFirmware(client *skydroid.Client, logger *slog.Logger) string {
	resp, err := client.QueryTCP(skydroid.CmdGetVersion, 2*time.Second)
	if err != nil {
		logger.Warn("query camera firmware failed, using unknown", "err", err.Error())
		return "unknown"
	}
	fw := strings.TrimSpace(resp)
	fw = strings.TrimPrefix(fw, skydroid.CmdGetVersion)
	fw = strings.TrimPrefix(fw, "=")
	if fw == "" {
		fw = "unknown"
	}
	if len(fw) > 64 {
		fw = fw[:64]
	}
	logger.Info("camera firmware", "version", fw)
	return fw
}

// listenWithDrift 端口被占用时向后漂移（EADDRINUSE）。
func listenWithDrift(addr string, maxAttempts int, logger *slog.Logger) (net.Listener, int, bool, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, 0, false, fmt.Errorf("invalid http addr %q: %w", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 {
		return nil, 0, false, fmt.Errorf("invalid http port %q", portStr)
	}
	for i := 0; i < maxAttempts; i++ {
		candidate := port + i
		if candidate > 65535 {
			break
		}
		laddr := net.JoinHostPort(host, strconv.Itoa(candidate))
		ln, err := net.Listen("tcp", laddr)
		if err == nil {
			return ln, candidate, i > 0, nil
		}
		if !errors.Is(err, syscall.EADDRINUSE) {
			return nil, 0, false, fmt.Errorf("listen %s: %w", laddr, err)
		}
		logger.Warn("http port busy, drifting to next", "busy", laddr)
	}
	return nil, 0, false, fmt.Errorf("ports %d-%d all busy", port, port+maxAttempts-1)
}

func detectAdvertiseIP() string {
	conn, err := net.Dial("udp4", "8.8.8.8:80")
	if err == nil {
		defer func() { _ = conn.Close() }()
		if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok && addr.IP != nil && !addr.IP.IsLoopback() {
			return addr.IP.String()
		}
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "127.0.0.1"
	}
	for _, a := range addrs {
		if ipNet, ok := a.(*net.IPNet); ok && ipNet.IP.To4() != nil && !ipNet.IP.IsLoopback() {
			return ipNet.IP.String()
		}
	}
	return "127.0.0.1"
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	switch level {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: l}))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		return v == "1" || v == "true" || v == "yes"
	}
	return def
}
