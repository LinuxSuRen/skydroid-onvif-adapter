// Package snapshot 从相机 RTSP 流抓取单帧 JPEG（子码流，快照更快）。
package snapshot

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"time"
)

// skipFrames 跳过流开头的帧数（上游 issue #2）。
// Skydroid 双光云台（TP 固件 05.08.60，RTSP 服务 rtsp_demo）的流开头约
// 25 帧是灰底占位帧（1280x720 纯灰，灰度 mean≈128 std≈0.2），真实画面
// 在其后；实测跳过前 25 帧再取帧可得正常画面。
const skipFrames = 25

// timeout 抓帧整体超时。跳过 skipFrames 帧按 25fps 计约多花 1 秒
// （低帧率子码流按 10fps 计也约 2.5 秒），原 10 秒预算仍充足，维持不变。
const timeout = 10 * time.Second

// Generator 抓帧生成器。
type Generator struct {
	bin    string
	logger *slog.Logger
}

// New 创建抓帧生成器。bin 为 ffmpeg 可执行文件路径。
func New(bin string, logger *slog.Logger) *Generator {
	if logger == nil {
		logger = slog.Default()
	}
	return &Generator{bin: bin, logger: logger}
}

// buildArgs 构造 ffmpeg 抓帧参数：TCP 拉 RTSP，丢弃流开头 skipFrames 帧
// 后输出一帧 JPEG 到标准输出。
// exec 直接传参数数组、不经过 shell，故无需 shell 引号；但 ffmpeg 滤镜图
// 语法里逗号是滤镜链分隔符，select 表达式内的逗号仍需转义为 "\,"，跨
// Windows/Linux 行为一致。
func buildArgs(rtspURL string) []string {
	return []string{"-hide_banner", "-loglevel", "error", "-nostdin",
		"-rtsp_transport", "tcp",
		"-i", rtspURL,
		"-vf", fmt.Sprintf("select=gte(n\\,%d)", skipFrames),
		"-frames:v", "1", "-q:v", "3", "-f", "image2", "pipe:1"}
}

// JPEG 从 rtspURL 拉取一帧 JPEG（跳过流开头的占位帧）。
func (g *Generator) JPEG(rtspURL string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	args := buildArgs(rtspURL)
	g.logger.Debug("snapshot capturing", "url", rtspURL)
	cmd := exec.CommandContext(ctx, g.bin, args...) //nolint:gosec // 路径来自受控配置
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		tail := errBuf.String()
		if len(tail) > 300 {
			tail = tail[len(tail)-300:]
		}
		return nil, fmt.Errorf("ffmpeg capture failed: %w; %s", err, tail)
	}
	data := out.Bytes()
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return nil, fmt.Errorf("ffmpeg capture produced non-JPEG output (%d bytes)", len(data))
	}
	return data, nil
}
