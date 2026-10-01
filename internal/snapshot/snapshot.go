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

// JPEG 从 rtspURL 拉取一帧 JPEG。
func (g *Generator) JPEG(rtspURL string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin",
		"-rtsp_transport", "tcp",
		"-i", rtspURL, "-frames:v", "1", "-q:v", "3", "-f", "image2", "pipe:1"}
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
