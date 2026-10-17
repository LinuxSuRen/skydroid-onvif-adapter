package snapshot

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
)

// fakeFFmpegEnv 置 1 时，测试二进制以“伪 ffmpeg”模式运行（重入自身），
// 用于在无真实 ffmpeg/相机的环境下端到端验证 JPEG()。
const fakeFFmpegEnv = "GO_SNAPSHOT_FAKE_FFMPEG"

// fakeJPEG 伪 ffmpeg 输出的最小合法 JPEG（SOI + 假数据 + EOI）。
var fakeJPEG = []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'f', 'a', 'k', 'e', 0xFF, 0xD9}

func TestMain(m *testing.M) {
	if os.Getenv(fakeFFmpegEnv) == "1" {
		fakeFFmpeg(os.Args[1:])
		return
	}
	os.Exit(m.Run())
}

// fakeFFmpeg 充当 ffmpeg：先校验关键参数确实按约定传入，再按输入 URL 的
// 路径决定输出内容，覆盖成功、非 JPEG 输出、输出过短、进程失败四条分支。
func fakeFFmpeg(args []string) {
	// 关键参数逐项校验，任何缺失/错值都以非零退出并留 stderr 线索。
	for _, pair := range [][2]string{
		{"-rtsp_transport", "tcp"},
		{"-vf", `select=gte(n\,25)`},
		{"-frames:v", "1"},
		{"-f", "image2"},
	} {
		i := slices.Index(args, pair[0])
		if i < 0 || i+1 >= len(args) || args[i+1] != pair[1] {
			fmt.Fprintf(os.Stderr, "fake ffmpeg: bad args %q, want %s %s\n", args, pair[0], pair[1])
			os.Exit(1)
		}
	}
	if args[len(args)-1] != "pipe:1" {
		fmt.Fprintf(os.Stderr, "fake ffmpeg: last arg %q, want pipe:1\n", args[len(args)-1])
		os.Exit(1)
	}
	i := slices.Index(args, "-i")
	switch args[i+1] {
	case "rtsp://fake/ok": // 正常：输出一帧 JPEG
		_, _ = os.Stdout.Write(fakeJPEG)
	case "rtsp://fake/garbage": // 输出非 JPEG 数据
		_, _ = os.Stdout.Write([]byte("this is not a jpeg"))
	case "rtsp://fake/short": // 输出不足 4 字节
		_, _ = os.Stdout.Write([]byte{0xFF, 0xD8})
	case "rtsp://fake/fail": // 进程失败（模拟断流/鉴权失败）
		fmt.Fprintln(os.Stderr, "fake ffmpeg: boom")
		os.Exit(9)
	default:
		fmt.Fprintf(os.Stderr, "fake ffmpeg: unexpected url %s\n", args[i+1])
		os.Exit(1)
	}
}

// TestBuildArgs 表驱动验证 ffmpeg 参数构造：取值、位置与顺序。
func TestBuildArgs(t *testing.T) {
	const url = "rtsp://192.168.144.108:554/stream=1"
	args := buildArgs(url)
	cases := []struct {
		name string
		flag string
		want string
	}{
		{"TCP 拉流", "-rtsp_transport", "tcp"},
		{"输入地址", "-i", url},
		{"跳过开头占位帧", "-vf", `select=gte(n\,25)`},
		{"只输出一帧", "-frames:v", "1"},
		{"输出封装", "-f", "image2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			i := slices.Index(args, c.flag)
			if i < 0 {
				t.Fatalf("buildArgs 缺少参数 %s: %v", c.flag, args)
			}
			if i+1 >= len(args) || args[i+1] != c.want {
				t.Fatalf("buildArgs %s = %q, want %q", c.flag, args[i+1], c.want)
			}
		})
	}
	// 输出目标必须是 stdout 管道。
	if got := args[len(args)-1]; got != "pipe:1" {
		t.Fatalf("buildArgs 末参数 = %q, want pipe:1", got)
	}
	// URL 只出现一次，且位于输入侧参数之后、输出侧参数之前（顺序错误
	// 时 -vf/-frames:v 会被 ffmpeg 当成输入选项）。
	iURL, iIn, iVF := slices.Index(args, "-i"), slices.Index(args, "-rtsp_transport"), slices.Index(args, "-vf")
	if strings.Count(strings.Join(args, "\x00"), url) != 1 {
		t.Fatalf("buildArgs 中 URL 出现次数 != 1: %v", args)
	}
	if !(iIn < iURL && iURL < iVF) {
		t.Fatalf("buildArgs 参数顺序错误: -rtsp_transport=%d -i=%d -vf=%d", iIn, iURL, iVF)
	}
}

// TestJPEGWithFakeFFmpeg 用重入自身的伪 ffmpeg 端到端验证 JPEG()。
func TestJPEGWithFakeFFmpeg(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeFFmpegEnv, "1") // 子进程默认继承本进程环境变量
	g := New(bin, slog.New(slog.NewTextHandler(io.Discard, nil)))
	cases := []struct {
		name    string
		url     string
		wantErr string // 空串表示应成功
	}{
		{"正常输出 JPEG", "rtsp://fake/ok", ""},
		{"输出非 JPEG 数据", "rtsp://fake/garbage", "non-JPEG output"},
		{"输出不足 4 字节", "rtsp://fake/short", "non-JPEG output"},
		{"ffmpeg 进程失败", "rtsp://fake/fail", "ffmpeg capture failed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data, err := g.JPEG(c.url)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("JPEG(%s) 报错: %v", c.url, err)
				}
				if !slices.Equal(data, fakeJPEG) {
					t.Fatalf("JPEG(%s) 输出 %d 字节, 与伪 ffmpeg 写入不符", c.url, len(data))
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("JPEG(%s) err = %v, want 包含 %q", c.url, err, c.wantErr)
			}
		})
	}
}
