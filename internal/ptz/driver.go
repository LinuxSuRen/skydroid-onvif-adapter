// Package ptz 提供 ONVIF PTZ 到 Skydroid 云台的映射驱动。
//
// 由于 TP 协议没有云台角度回读命令，位置由本地估算：绝对转角命令下发时
// 记录目标值，连续移动按速度虚拟积分（满速 90°/s 偏航、45°/s 俯仰）。
package ptz

import (
	"fmt"
	"sync"
	"time"

	"github.com/linuxsuren/skydroid-onvif-adapter/internal/skydroid"
)

// Preset 预置位。
type Preset struct {
	Token string  `json:"token"`
	Name  string  `json:"name"`
	Pan   float64 `json:"pan"`  // 估算 yaw 角
	Tilt  float64 `json:"tilt"` // 估算 pitch 角
}

// 坐标范围（SDK gotoYaw/gotoPitch 钳位：偏航/俯仰均 ±90°）。
const (
	PanMin  = -90.0
	PanMax  = 90.0
	TiltMin = -90.0
	TiltMax = 90.0
	ZoomMin = 0.0
	ZoomMax = 1.0
)

// estimator 云台位置估算器。
type estimator struct {
	pan, tilt, zoom          float64
	panVel, tiltVel, zoomVel float64
	last                     time.Time
}

func (e *estimator) advance() {
	now := time.Now()
	el := now.Sub(e.last)
	e.last = now
	if el <= 0 || (e.panVel == 0 && e.tiltVel == 0 && e.zoomVel == 0) {
		return
	}
	e.pan = clamp(e.pan+e.panVel*90*el.Seconds(), PanMin, PanMax)
	e.tilt = clamp(e.tilt+e.tiltVel*45*el.Seconds(), TiltMin, TiltMax)
	e.zoom = clamp(e.zoom+e.zoomVel*0.5*el.Seconds(), ZoomMin, ZoomMax)
}

func clamp(v, min, max float64) float64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// SkydroidDriver 把 ONVIF PTZ 语义翻译为 TP 协议命令。
type SkydroidDriver struct {
	client *skydroid.Client
	logger interface {
		Warn(msg string, args ...any)
		Debug(msg string, args ...any)
	}

	mu       sync.Mutex
	est      estimator
	presets  map[string]Preset
	order    []string
	nextID   int
	homeSet  bool
	homePan  float64
	homeTilt float64
}

// NewSkydroidDriver 创建驱动。
func NewSkydroidDriver(client *skydroid.Client) *SkydroidDriver {
	d := &SkydroidDriver{
		client:  client,
		est:     estimator{last: time.Now()},
		presets: map[string]Preset{},
		nextID:  10,
	}
	return d
}

// SetLogger 注入日志器（记录命令发送失败）。
func (d *SkydroidDriver) SetLogger(l interface {
	Warn(msg string, args ...any)
	Debug(msg string, args ...any)
}) {
	d.logger = l
}

// send 发送 TP 命令（自动追加校验），失败仅记录（ONVIF 侧不因云台瞬时故障报错）。
func (d *SkydroidDriver) send(cmd string) {
	if err := d.client.Send(cmd); err != nil {
		if d.logger != nil {
			d.logger.Warn("skydroid cmd failed", "cmd", cmd, "err", err.Error())
		}
	}
}

// ContinuousMove 连续移动：两轴独立带符号速度（官方 rcsdk-demo 语义：
// GSY/GSP 为 ±100 刻度补码，正=右/上、负=左/下），pan/tilt 优先于 zoom。
func (d *SkydroidDriver) ContinuousMove(pan, tilt, zoom float64) error {
	d.mu.Lock()
	d.est.advance()
	if pan != 0 || tilt != 0 {
		d.est.panVel, d.est.tiltVel = pan, tilt
	}
	if zoom != 0 {
		d.est.zoomVel = zoom
	}
	d.mu.Unlock()

	if pan != 0 || tilt != 0 {
		d.send(skydroid.CmdSetYawSpeedPrefix + skydroid.SpeedSigned(pan))
		d.send(skydroid.CmdSetPitchSpeedPrefix + skydroid.SpeedSigned(tilt))
		return nil
	}
	if zoom > 0 {
		d.send(skydroid.CmdPTZZoomIn)
	} else if zoom < 0 {
		d.send(skydroid.CmdPTZZoomOut)
	}
	return nil
}

// Stop 停止运动：两轴速度归零 + PTZ00 兜底（旧固件兼容）。
func (d *SkydroidDriver) Stop(panTilt, zoom bool) error {
	d.mu.Lock()
	d.est.advance()
	if panTilt {
		d.est.panVel, d.est.tiltVel = 0, 0
	}
	if zoom {
		d.est.zoomVel = 0
	}
	d.mu.Unlock()
	if panTilt {
		d.send(skydroid.CmdSetYawSpeedPrefix + "00")
		d.send(skydroid.CmdSetPitchSpeedPrefix + "00")
	}
	d.send(skydroid.CmdPTZStop)
	return nil
}

// AbsoluteMove 转到绝对角度：yaw/pitch 分别下发 GAY/GAP。
func (d *SkydroidDriver) AbsoluteMove(pan, tilt, zoom float64) error {
	pan = clamp(pan, PanMin, PanMax)
	tilt = clamp(tilt, TiltMin, TiltMax)
	d.mu.Lock()
	d.est.advance()
	d.est.panVel, d.est.tiltVel, d.est.zoomVel = 0, 0, 0
	d.est.pan, d.est.tilt = pan, tilt
	d.mu.Unlock()

	d.send(skydroid.CmdGotoYawPrefix + skydroid.AngleToHex(pan))
	d.send(skydroid.CmdGotoPitchPrefix + skydroid.AngleToHex(tilt))
	return nil
}

// RelativeMove 在当前真实位置（姿态新鲜时）或估算位置上叠加位移后按绝对转角下发。
func (d *SkydroidDriver) RelativeMove(dPan, dTilt, dZoom float64) error {
	d.mu.Lock()
	d.est.advance()
	if p, t, ok := d.attitudeFresh(); ok {
		d.est.pan, d.est.tilt = p, t
	}
	pan, tilt := d.est.pan, d.est.tilt
	d.mu.Unlock()
	// zoom 不支持绝对值，映射为瞬时方向。
	if dZoom > 0 {
		d.send(skydroid.CmdPTZZoomIn)
	} else if dZoom < 0 {
		d.send(skydroid.CmdPTZZoomOut)
	}
	return d.AbsoluteMove(pan+dPan, tilt+dTilt, 0)
}

// attitudeFresh 同步并查询云台真实姿态；上报新鲜（3 秒内）时把估算器对齐
// 到真实值并返回真值。调用方需持有 d.mu。
func (d *SkydroidDriver) attitudeFresh() (pan, tilt float64, ok bool) {
	yaw, pitch, _, fresh := d.client.Attitude(3 * time.Second)
	if !fresh {
		return 0, 0, false
	}
	d.est.pan, d.est.tilt = yaw, pitch
	return yaw, pitch, true
}

// Status 返回位置与运动状态：云台姿态上报新鲜时返回真实 yaw/pitch，
// 否则返回估算位置。
func (d *SkydroidDriver) Status() (pan, tilt, zoom float64, moving bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.est.advance()
	if p, t, ok := d.attitudeFresh(); ok {
		pan, tilt = p, t
	} else {
		pan, tilt = d.est.pan, d.est.tilt
	}
	zoom = d.est.zoom
	moving = d.est.panVel != 0 || d.est.tiltVel != 0 || d.est.zoomVel != 0
	return pan, tilt, zoom, moving
}

// SetHomePosition 记录当前估算位置为 Home。
func (d *SkydroidDriver) SetHomePosition() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.est.advance()
	d.homeSet, d.homePan, d.homeTilt = true, d.est.pan, d.est.tilt
}

// GotoHomePosition 回到 Home；未设置过时使用硬件回中命令。
func (d *SkydroidDriver) GotoHomePosition() error {
	d.mu.Lock()
	homeSet, hp, ht := d.homeSet, d.homePan, d.homeTilt
	d.mu.Unlock()
	if !homeSet {
		d.send(skydroid.CmdPTZCenter)
		return nil
	}
	return d.AbsoluteMove(hp, ht, 0)
}

// 内建预置位 token：硬件能力位与云台模式位。
const (
	presetCenter   = "1" // 回中
	presetLookDown = "2" // 垂直向下
	presetLookFwd  = "3" // 水平朝前
	presetFollow   = "4" // Follow 模式
	presetLock     = "5" // Lock 模式
	presetFPV      = "6" // FPV 模式
)

var builtinPresets = []Preset{
	{Token: presetCenter, Name: "回中"},
	{Token: presetLookDown, Name: "垂直向下"},
	{Token: presetLookFwd, Name: "水平朝前"},
	{Token: presetFollow, Name: "Follow 模式"},
	{Token: presetLock, Name: "Lock 模式"},
	{Token: presetFPV, Name: "FPV 模式"},
}

// Presets 返回全部预置位（内建 + 用户自定义）。
func (d *SkydroidDriver) Presets() []Preset {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.est.advance()
	out := append([]Preset(nil), builtinPresets...)
	for _, token := range d.order {
		out = append(out, d.presets[token])
	}
	return out
}

// SetPreset 保存当前估算位姿为自定义预置位，返回 token。
func (d *SkydroidDriver) SetPreset(name string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.est.advance()
	for _, token := range d.order {
		if d.presets[token].Name == name {
			p := d.presets[token]
			p.Pan, p.Tilt = d.est.pan, d.est.tilt
			d.presets[token] = p
			return token
		}
	}
	token := fmt.Sprintf("%d", d.nextID)
	d.nextID++
	d.presets[token] = Preset{Token: token, Name: name, Pan: d.est.pan, Tilt: d.est.tilt}
	d.order = append(d.order, token)
	return token
}

// GotoPreset 调用预置位：内建 token 直接发硬件命令，自定义位按角度回放。
func (d *SkydroidDriver) GotoPreset(token string) error {
	switch token {
	case presetCenter:
		d.send(skydroid.CmdPTZCenter)
		return d.AbsoluteMove(0, 0, 0)
	case presetLookDown:
		d.send(skydroid.CmdPTZLookDown)
		return d.AbsoluteMove(0, TiltMin, 0)
	case presetLookFwd:
		d.send(skydroid.CmdPTZLookFwd)
		return d.AbsoluteMove(0, 0, 0)
	case presetFollow:
		d.send(skydroid.CmdPTZFollow)
		return nil
	case presetLock:
		d.send(skydroid.CmdPTZLock)
		return nil
	case presetFPV:
		d.send(skydroid.CmdPTZFPV)
		return nil
	}
	d.mu.Lock()
	p, ok := d.presets[token]
	d.mu.Unlock()
	if !ok {
		return fmt.Errorf("preset %q not found", token)
	}
	return d.AbsoluteMove(p.Pan, p.Tilt, 0)
}

// RemovePreset 删除自定义预置位（内建位不可删）。
func (d *SkydroidDriver) RemovePreset(token string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.presets[token]; !ok {
		return false
	}
	delete(d.presets, token)
	for i, t := range d.order {
		if t == token {
			d.order = append(d.order[:i], d.order[i+1:]...)
			break
		}
	}
	return true
}
