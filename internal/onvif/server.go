// Package onvif 实现把 Skydroid 云卓云台相机映射为 ONVIF 设备端 SOAP 服务：
// Device/Media/PTZ 三服务共用单一 HTTP 端点，PTZ 操作翻译为 TP 协议命令。
package onvif

import (
	"fmt"
	"log/slog"
	"net/http"

	"github.com/linuxsuren/skydroid-onvif-adapter/internal/ptz"
)

// Profile 描述一路视频 profile（对应相机的一路 RTSP 流）。
type Profile struct {
	Token       string `json:"token"`        // 如 profile_main
	Name        string `json:"name"`         // 如 主码流
	StreamIndex int    `json:"stream_index"` // rtsp://<cam>/stream=N
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Framerate   int    `json:"framerate"`
}

// Config 服务配置。
type Config struct {
	Manufacturer string
	Model        string
	Firmware     string
	Serial       string

	AdvertiseIP    string
	HTTPPort       int
	CameraRTSPBase string // 形如 192.168.144.108:554

	Profiles []Profile
}

// XAddr 返回 ONVIF Device Service 地址。
func (c Config) XAddr() string {
	return fmt.Sprintf("http://%s:%d/onvif/device_service", c.AdvertiseIP, c.HTTPPort)
}

// StreamURI 返回指定 profile 对应的相机 RTSP 地址（客户端直连相机拉流）。
func (c Config) StreamURI(p Profile) string {
	return fmt.Sprintf("rtsp://%s/stream=%d", c.CameraRTSPBase, p.StreamIndex)
}

// SnapshotURI 返回快照 JPEG 地址（由本适配器提供）。
func (c Config) SnapshotURI(p Profile) string {
	return fmt.Sprintf("http://%s:%d/onvif/snapshot/%s", c.AdvertiseIP, c.HTTPPort, p.Token)
}

// Service ONVIF SOAP 服务。
type Service struct {
	cfg    Config
	driver *ptz.SkydroidDriver
	logger *slog.Logger
}

// NewService 创建服务。
func NewService(cfg Config, driver *ptz.SkydroidDriver, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	if len(cfg.Profiles) == 0 {
		cfg.Profiles = []Profile{
			{Token: "profile_main", Name: "主码流", StreamIndex: 0, Width: 1920, Height: 1080, Framerate: 30},
			{Token: "profile_sub", Name: "子码流", StreamIndex: 1, Width: 640, Height: 360, Framerate: 15},
		}
	}
	return &Service{cfg: cfg, driver: driver, logger: logger}
}

func (s *Service) profile(token string) (Profile, bool) {
	for _, p := range s.cfg.Profiles {
		if p.Token == token {
			return p, true
		}
	}
	return Profile{}, false
}

// ProfileByToken 按 token 查询 profile（供快照路由使用）。
func (s *Service) ProfileByToken(token string) (Profile, bool) {
	return s.profile(token)
}

type opHandler func(s *Service, inner []byte) (string, error)

// dispatch 按命名空间精确匹配操作，回退按操作名匹配（与 local-onvif-adapter 相同策略）。
var (
	dispatchNS    = map[string]opHandler{}
	dispatchLocal = map[string]opHandler{}
)

func reg(ns, op string, h opHandler) {
	dispatchNS[ns+"|"+op] = h
	if _, ok := dispatchLocal[op]; !ok {
		dispatchLocal[op] = h
	}
}

const (
	nsDevice = "http://www.onvif.org/ver10/device/wsdl"
	nsMedia  = "http://www.onvif.org/ver10/media/wsdl"
	nsPTZ    = "http://www.onvif.org/ver20/ptz/wsdl"
)

// Handler 返回 ONVIF SOAP HTTP 处理器。鉴权：不校验任何凭据。
func (s *Service) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "ONVIF service accepts POST only", http.StatusMethodNotAllowed)
			return
		}
		op, ns, inner, err := parseSOAPRequest(r)
		if err != nil {
			s.logger.Warn("onvif request parse failed", "err", err.Error(), "remote", r.RemoteAddr)
			writeSOAP(w, soapFault("ter:InvalidArgVal", "malformed SOAP request: "+err.Error()))
			return
		}
		handler, ok := dispatchNS[ns+"|"+op]
		if !ok {
			handler, ok = dispatchLocal[op]
		}
		if !ok {
			s.logger.Info("onvif op not supported", "op", op, "ns", ns, "remote", r.RemoteAddr)
			writeSOAP(w, soapFault("ter:ActionNotSupported", "operation "+op+" is not supported"))
			return
		}
		body, err := handler(s, inner)
		if err != nil {
			s.logger.Warn("onvif op failed", "op", op, "err", err.Error())
			writeSOAP(w, soapFault("ter:Action", "operation "+op+" failed: "+err.Error()))
			return
		}
		s.logger.Debug("onvif op handled", "op", op, "remote", r.RemoteAddr)
		writeSOAP(w, soapEnvelope(body))
	})
}

func writeSOAP(w http.ResponseWriter, data []byte) {
	w.Header().Set("Content-Type", "application/soap+xml; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
