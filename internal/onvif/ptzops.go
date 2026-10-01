package onvif

import (
	"encoding/xml"
	"fmt"
	"strconv"
	"time"

	"github.com/linuxsuren/skydroid-onvif-adapter/internal/ptz"
)

// ONVIF 标准 PTZ 空间。
const (
	absPanTiltSpace = "http://www.onvif.org/ver10/tptz/PanTiltSpaces/PositionGenericSpace"
	absZoomSpace    = "http://www.onvif.org/ver10/tptz/ZoomSpaces/PositionGenericSpace"
	velPanTiltSpace = "http://www.onvif.org/ver10/tptz/PanTiltSpaces/VelocityGenericSpace"
	velZoomSpace    = "http://www.onvif.org/ver10/tptz/ZoomSpaces/VelocityGenericSpace"
)

// 估算坐标范围（与驱动一致）。
const (
	panMin, panMax   = ptz.PanMin, ptz.PanMax
	tiltMin, tiltMax = ptz.TiltMin, ptz.TiltMax
)

func registerPTZOps() {
	reg(nsPTZ, "GetNodes", opGetNodes)
	reg(nsPTZ, "GetNode", opGetNode)
	reg(nsPTZ, "GetConfigurations", opGetConfigs)
	reg(nsPTZ, "GetConfiguration", opGetConfig)
	reg(nsPTZ, "GetConfigurationOptions", opGetConfigOptions)
	reg(nsPTZ, "GetStatus", opGetStatus)
	reg(nsPTZ, "ContinuousMove", opContinuousMove)
	reg(nsPTZ, "RelativeMove", opRelativeMove)
	reg(nsPTZ, "AbsoluteMove", opAbsoluteMove)
	reg(nsPTZ, "Stop", opStop)
	reg(nsPTZ, "GotoHomePosition", opGotoHome)
	reg(nsPTZ, "SetHomePosition", opSetHome)
	reg(nsPTZ, "GetPresets", opGetPresets)
	reg(nsPTZ, "SetPreset", opSetPreset)
	reg(nsPTZ, "GotoPreset", opGotoPreset)
	reg(nsPTZ, "RemovePreset", opRemovePreset)
	reg(nsPTZ, "GetServiceCapabilities", func(s *Service, _ []byte) (string, error) {
		return `<tptz:GetServiceCapabilitiesResponse>` +
			`<tptz:Capabilities EFlip="false" Reverse="false" ContinuousMove="true" RelativeMove="true" AbsoluteMove="true" Presets="true"/>` +
			`</tptz:GetServiceCapabilitiesResponse>`, nil
	})
}

// ptzReq 覆盖全部 PTZ 请求的可选字段（按子元素解析，local name 匹配）。
type ptzReq struct {
	ProfileToken string    `xml:"ProfileToken"`
	Position     ptzVector `xml:"Position"`
	Translation  ptzVector `xml:"Translation"`
	Velocity     ptzVector `xml:"Velocity"`
	Speed        ptzVector `xml:"Speed"`
	PanTiltStop  bool      `xml:"PanTilt"`
	ZoomStop     bool      `xml:"Zoom"`
	PresetToken  string    `xml:"PresetToken"`
	PresetName   string    `xml:"PresetName"`
	Timeout      string    `xml:"Timeout"`
}

type ptzVector struct {
	PanTilt struct {
		X float64 `xml:"x,attr"`
		Y float64 `xml:"y,attr"`
	} `xml:"PanTilt"`
	Zoom struct {
		X float64 `xml:"x,attr"`
	} `xml:"Zoom"`
}

func (v ptzVector) pan() float64  { return v.PanTilt.X }
func (v ptzVector) tilt() float64 { return v.PanTilt.Y }
func (v ptzVector) zoom() float64 { return v.Zoom.X }

func opGetNodes(_ *Service, _ []byte) (string, error) {
	return `<tptz:GetNodesResponse>` + ptzNodeXML() + `</tptz:GetNodesResponse>`, nil
}

func opGetNode(_ *Service, _ []byte) (string, error) {
	return `<tptz:GetNodeResponse>` + ptzNodeXML() + `</tptz:GetNodeResponse>`, nil
}

func ptzNodeXML() string {
	return `<tptz:PTZNode token="ptznode_main" FixedHomePosition="false" HomeSupported="true">` +
		`<tt:Name>Skydroid Gimbal</tt:Name>` +
		`<tt:SupportedPTZSpaces>` +
		`<tt:AbsolutePanTiltPositionSpace><tt:XRange><tt:Min>-90</tt:Min><tt:Max>90</tt:Max></tt:XRange>` +
		`<tt:YRange><tt:Min>-90</tt:Min><tt:Max>90</tt:Max></tt:YRange>` +
		`<tt:URI>` + absPanTiltSpace + `</tt:URI></tt:AbsolutePanTiltPositionSpace>` +
		`<tt:AbsoluteZoomPositionSpace><tt:XRange><tt:Min>0</tt:Min><tt:Max>1</tt:Max></tt:XRange>` +
		`<tt:URI>` + absZoomSpace + `</tt:URI></tt:AbsoluteZoomPositionSpace>` +
		`<tt:ContinuousPanTiltVelocitySpace><tt:XRange><tt:Min>-1</tt:Min><tt:Max>1</tt:Max></tt:XRange>` +
		`<tt:YRange><tt:Min>-1</tt:Min><tt:Max>1</tt:Max></tt:YRange>` +
		`<tt:URI>` + velPanTiltSpace + `</tt:URI></tt:ContinuousPanTiltVelocitySpace>` +
		`<tt:ContinuousZoomVelocitySpace><tt:XRange><tt:Min>-1</tt:Min><tt:Max>1</tt:Max></tt:XRange>` +
		`<tt:URI>` + velZoomSpace + `</tt:URI></tt:ContinuousZoomVelocitySpace>` +
		`</tt:SupportedPTZSpaces>` +
		`<tt:MaximumNumberOfPresets>16</tt:MaximumNumberOfPresets>` +
		`<tt:HomeSupported>true</tt:HomeSupported>` +
		`</tptz:PTZNode>`
}

func ptzCfgXML() string {
	return `<tptz:PTZConfiguration token="ptzcfg_main">` +
		`<tt:Name>Skydroid</tt:Name><tt:UseCount>1</tt:UseCount>` +
		`<tt:NodeToken>ptznode_main</tt:NodeToken>` +
		`<tt:DefaultAbsolutePantTiltVelocitySpace>` + absPanTiltSpace + `</tt:DefaultAbsolutePantTiltVelocitySpace>` +
		`<tt:DefaultAbsoluteZoomVelocitySpace>` + absZoomSpace + `</tt:DefaultAbsoluteZoomVelocitySpace>` +
		`<tt:DefaultContinuousPanTiltVelocitySpace>` + velPanTiltSpace + `</tt:DefaultContinuousPanTiltVelocitySpace>` +
		`<tt:DefaultContinuousZoomVelocitySpace>` + velZoomSpace + `</tt:DefaultContinuousZoomVelocitySpace>` +
		`<tt:DefaultPTZSpeed><tt:PanTilt x="0.5" y="0.5" space="` + velPanTiltSpace + `"/>` +
		`<tt:Zoom x="0.5" space="` + velZoomSpace + `"/></tt:DefaultPTZSpeed>` +
		`<tt:DefaultPTZTimeout>PT5S</tt:DefaultPTZTimeout>` +
		`</tptz:PTZConfiguration>`
}

func opGetConfigs(_ *Service, _ []byte) (string, error) {
	return `<tptz:GetConfigurationsResponse>` + ptzCfgXML() + `</tptz:GetConfigurationsResponse>`, nil
}

func opGetConfig(_ *Service, _ []byte) (string, error) {
	return `<tptz:GetConfigurationResponse>` + ptzCfgXML() + `</tptz:GetConfigurationResponse>`, nil
}

func opGetConfigOptions(_ *Service, _ []byte) (string, error) {
	return `<tptz:GetConfigurationOptionsResponse><tptz:PTZOptions>` +
		`<tt:Spaces>` +
		`<tt:AbsolutePanTiltPositionSpace><tt:XRange><tt:Min>-90</tt:Min><tt:Max>90</tt:Max></tt:XRange>` +
		`<tt:YRange><tt:Min>-90</tt:Min><tt:Max>90</tt:Max></tt:YRange><tt:URI>` + absPanTiltSpace + `</tt:URI></tt:AbsolutePanTiltPositionSpace>` +
		`<tt:AbsoluteZoomPositionSpace><tt:XRange><tt:Min>0</tt:Min><tt:Max>1</tt:Max></tt:XRange>` +
		`<tt:URI>` + absZoomSpace + `</tt:URI></tt:AbsoluteZoomPositionSpace>` +
		`<tt:ContinuousPanTiltVelocitySpace><tt:XRange><tt:Min>-1</tt:Min><tt:Max>1</tt:Max></tt:XRange>` +
		`<tt:YRange><tt:Min>-1</tt:Min><tt:Max>1</tt:Max></tt:YRange><tt:URI>` + velPanTiltSpace + `</tt:URI></tt:ContinuousPanTiltVelocitySpace>` +
		`<tt:ContinuousZoomVelocitySpace><tt:XRange><tt:Min>-1</tt:Min><tt:Max>1</tt:Max></tt:XRange>` +
		`<tt:URI>` + velZoomSpace + `</tt:URI></tt:ContinuousZoomVelocitySpace>` +
		`</tt:Spaces>` +
		`<tt:PTZTimeout><tt:Min>PT1S</tt:Min><tt:Max>PT60S</tt:Max></tt:PTZTimeout>` +
		`</tptz:PTZOptions></tptz:GetConfigurationOptionsResponse>`, nil
}

func fmtF(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func opGetStatus(s *Service, _ []byte) (string, error) {
	pan, tilt, zoom, moving := s.driver.Status()
	moveStatus := "IDLE"
	if moving {
		moveStatus = "MOVING"
	}
	return `<tptz:GetStatusResponse><tptz:PTZStatus>` +
		fmt.Sprintf(`<tt:Position><tt:PanTilt x="%s" y="%s" space="%s"/><tt:Zoom x="%s" space="%s"/></tt:Position>`,
			fmtF(pan), fmtF(tilt), absPanTiltSpace, fmtF(zoom), absZoomSpace) +
		`<tt:MoveStatus><tt:PanTilt>` + moveStatus + `</tt:PanTilt><tt:Zoom>` + moveStatus + `</tt:Zoom></tt:MoveStatus>` +
		`<tt:Error>No error</tt:Error>` +
		dateTimeXML(time.Now().UTC()) +
		`</tptz:PTZStatus></tptz:GetStatusResponse>`, nil
}

func parsePTZReq(inner []byte) (ptzReq, error) {
	var req ptzReq
	if err := xml.Unmarshal(inner, &req); err != nil {
		return req, fmt.Errorf("parse request: %w", err)
	}
	return req, nil
}

func opContinuousMove(s *Service, inner []byte) (string, error) {
	req, err := parsePTZReq(inner)
	if err != nil {
		return "", err
	}
	if err := s.driver.ContinuousMove(req.Velocity.pan(), req.Velocity.tilt(), req.Velocity.zoom()); err != nil {
		return "", err
	}
	return `<tptz:ContinuousMoveResponse/>`, nil
}

func opRelativeMove(s *Service, inner []byte) (string, error) {
	req, err := parsePTZReq(inner)
	if err != nil {
		return "", err
	}
	if err := s.driver.RelativeMove(req.Translation.pan(), req.Translation.tilt(), req.Translation.zoom()); err != nil {
		return "", err
	}
	return `<tptz:RelativeMoveResponse/>`, nil
}

func opAbsoluteMove(s *Service, inner []byte) (string, error) {
	req, err := parsePTZReq(inner)
	if err != nil {
		return "", err
	}
	if err := s.driver.AbsoluteMove(req.Position.pan(), req.Position.tilt(), req.Position.zoom()); err != nil {
		return "", err
	}
	return `<tptz:AbsoluteMoveResponse/>`, nil
}

func opStop(s *Service, inner []byte) (string, error) {
	req, err := parsePTZReq(inner)
	if err != nil {
		return "", err
	}
	panTilt, zoom := true, true
	if req.PanTiltStop || req.ZoomStop {
		panTilt, zoom = req.PanTiltStop, req.ZoomStop
	}
	if err := s.driver.Stop(panTilt, zoom); err != nil {
		return "", err
	}
	return `<tptz:StopResponse/>`, nil
}

func opGotoHome(s *Service, _ []byte) (string, error) {
	if err := s.driver.GotoHomePosition(); err != nil {
		return "", err
	}
	return `<tptz:GotoHomePositionResponse/>`, nil
}

func opSetHome(s *Service, _ []byte) (string, error) {
	s.driver.SetHomePosition()
	return `<tptz:SetHomePositionResponse/>`, nil
}

func opGetPresets(s *Service, _ []byte) (string, error) {
	body := `<tptz:GetPresetsResponse>`
	for _, p := range s.driver.Presets() {
		body += `<tptz:Preset token="` + xmlEsc(p.Token) + `">` +
			`<tt:Name>` + xmlEsc(p.Name) + `</tt:Name>` +
			fmt.Sprintf(`<tt:PTZPosition><tt:PanTilt x="%s" y="%s" space="%s"/><tt:Zoom x="0" space="%s"/></tt:PTZPosition>`,
				fmtF(p.Pan), fmtF(p.Tilt), absPanTiltSpace, absZoomSpace) +
			`</tptz:Preset>`
	}
	return body + `</tptz:GetPresetsResponse>`, nil
}

func opSetPreset(s *Service, inner []byte) (string, error) {
	req, err := parsePTZReq(inner)
	if err != nil {
		return "", err
	}
	name := req.PresetName
	if name == "" {
		name = req.PresetToken
	}
	token := s.driver.SetPreset(name)
	return `<tptz:SetPresetResponse><tptz:PresetToken>` + xmlEsc(token) + `</tptz:PresetToken></tptz:SetPresetResponse>`, nil
}

func opGotoPreset(s *Service, inner []byte) (string, error) {
	req, err := parsePTZReq(inner)
	if err != nil {
		return "", err
	}
	if err := s.driver.GotoPreset(req.PresetToken); err != nil {
		return "", err
	}
	return `<tptz:GotoPresetResponse/>`, nil
}

func opRemovePreset(s *Service, inner []byte) (string, error) {
	req, err := parsePTZReq(inner)
	if err != nil {
		return "", err
	}
	s.driver.RemovePreset(req.PresetToken)
	return `<tptz:RemovePresetResponse/>`, nil
}

func init() {
	registerDeviceOps()
	registerMediaOps()
	registerPTZOps()
}
