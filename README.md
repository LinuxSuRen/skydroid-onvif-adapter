# skydroid-onvif-adapter

把 Skydroid（云卓）云台相机映射为标准 ONVIF 设备：ONVIF 客户端（NVR、
device-camera-onvif、ONVIF Device Manager 等）通过标准 PTZ/抓拍/取流接口控制相机，
适配器把云台命令翻译为 Skydroid TP 协议。

- **PTZ 控制**：ONVIF ContinuousMove/AbsoluteMove/预置位 → TP 协议（UDP 9002）
- **视频**：`GetStreamUri` 直接返回相机 RTSP 地址（客户端直连，零转码零转发）
- **快照**：`GetSnapshotUri` 由适配器从相机 RTSP 抓帧输出 JPEG（需 ffmpeg）
- **发现**：WS-Discovery（UDP 3702）自动发现
- 纯 Go 单二进制，无 CGO

协议依据（Go 直接实现线上协议，不依赖 Android SDK/AAR）：

1. **官方 [rcsdk-demo](https://gitee.com/skydroid/rcsdk-demo)** 源码 —— 命令样例、端口、通道
2. **官方 rcsdk-v1.9.2.aar 字节码反汇编**（javap）—— 校验算法（`genSendControlCmd`）、
   角度编码（`String2ByteArrayUtils.short2Hex`：16 位补码 %04X，范围 ±90°）、
   速度语义（GSY/GSP 带符号 ±100）、姿态回报帧（`#tpUG2rGAC<yaw><pitch><roll>`，
   TopParser 帧结构）
3. 逆向分析（[SkydroidC20](https://github.com/BadministratorSrcn/SkydroidC20)）——
   命令集清单互证

部分命令未经真机验证，首次使用请先用 `--probe` 联调。

## 快速开始

```bash
make build
# 1. 真机联调：验证与相机的 UDP/TCP/RTSP 连通性
./bin/skydroid-onvif-adapter --probe --camera-ip 192.168.144.108

# 2. 启动适配器
./bin/skydroid-onvif-adapter --camera-ip 192.168.144.108
```

## 架构与协议映射

```
ONVIF 客户端 ──SOAP──▶ 适配器(:8080) ──TP/UDP 9002──▶ Skydroid 相机
    │                     │
    │                     ├─ GetStreamUri ──返回──▶ 相机 RTSP :554（客户端直连）
    │                     └─ GetSnapshotUri ──ffmpeg 抓帧──▶ 相机 RTSP 子码流
    └── WS-Discovery UDP 3702 自动发现
```

### 命令校验与通道（官方 rcsdk-demo 对齐）

- **校验**：每条命令末尾追加 2 位十六进制累加和（整条命令含 `#` 与参数的全部
  ASCII 字节求和取低 8 位），如 `#TPUD2wCAP01` → `#TPUD2wCAP013E`
- **通道与端口**：C10Pro 走 UDP（相机 5000）；C20 相机走 TCP 8100、C20 云台走
  TCP 5000 —— 通过 `--camera-transport`/`--camera-udp-port`/`--camera-tcp-port` 选择

### PTZ 映射表

| ONVIF 操作 | TP 命令 | 说明 |
|---|---|---|
| ContinuousMove(pan/tilt) | `GSY`+pan速度 + `GSP`+tilt速度 | 两轴独立带符号速度（±100 刻度补码，正=右/上）；每条命令带累加校验 |
| ContinuousMove(zoom) | `PTZ0A` / `PTZ0B` | 变倍瞬时，松开依赖 Stop |
| Stop | `GSY00` + `GSP00` + `PTZ00` | 速度归零 + 停止兜底（旧固件兼容） |
| AbsoluteMove | `GAY` / `GAP` + 6 字节角度 | 角度×100，负数 24 位补码；yaw ±180°，pitch −90°~+30°（协议上限） |
| RelativeMove | 先读估算位置再按绝对转角下发 | |
| 预置位（自定义） | 保存本地估算角度，回放即 `GAY`+`GAP` | |
| 预置位 token 1/2/3 | `PTZ09`/`PTZ10`/`PTZ11` | 内建：回中 / 垂直向下 / 水平朝前 |
| 预置位 token 4/5/6 | `PTZ12`/`PTZ13`/`PTZ14` | 内建：Follow / Lock / FPV 云台模式 |
| GotoHomePosition | `PTZ09`（未设置 Home 时） | |
| GetStatus | **真实姿态优先** | 云台主动上报 `GAC` 姿态帧（本地 UDP 端口，默认 5000）时返回真实 yaw/pitch；无上报回退估算（绝对命令记录 + 虚拟积分） |

已知限制：

- TP 协议单时刻只表达一个运动方向：pan/tilt 优先于 zoom
- UDP 命令无 ACK，适配器按 3 次重发（间隔 30ms）提高可靠性
- 俯仰上限 30°、下限 −90° 为协议约束
- 部分命令来自 APK 逆向，未经全部真机验证

## 运行参数

| 参数 | 环境变量 | 默认值 | 说明 |
|---|---|---|---|
| `--camera-ip` | `CAMERA_IP` | `192.168.144.108` | 相机 IP |
| `--camera-transport` | `CAMERA_TRANSPORT` | `udp` | 命令通道：udp（C10Pro）/ tcp（C20） |
| `--camera-udp-port` | `CAMERA_UDP_PORT` | `5000` | TP 命令 UDP 端口（官方 C10Pro 默认） |
| `--camera-tcp-port` | `CAMERA_TCP_PORT` | `8100` | TCP 端口（官方 C20 相机默认 8100，云台 5000） |
| `--rtsp-base` | `CAMERA_RTSP_BASE` | `<camera-ip>:554` | RTSP 基址 |
| `--http-addr` | `HTTP_ADDR` | `:8080` | HTTP 监听（端口被占自动向后漂移） |
| `--advertise-ip` | `ADVERTISE_IP` | 自动探测 | 对外宣告 IP |
| `--model` | `CAMERA_MODEL` | `C20` | ONVIF Model |
| `--serial` | `CAMERA_SERIAL` | 按 IP 生成 | ONVIF SerialNumber |
| `--ffmpeg-bin` | `FFMPEG_BIN` | `ffmpeg` | 快照抓帧用 |
| `--discovery` | `DISCOVERY` | `true` | WS-Discovery |
| `--probe` | - | - | 探测相机连通性后退出 |

## 真机验证清单

1. `ping 192.168.144.108` —— 网络可达
2. `./skydroid-onvif-adapter --probe` —— TCP 查询返回固件号、UDP 发送成功
3. `ffplay rtsp://192.168.144.108:554/stream=0` —— 视频链路
4. 启动适配器后用 ONVIF Device Manager（或任意 ONVIF 客户端）发现/手动添加，
   云台方向键、预置位（token 1-6 为内建位）、抓拍逐一验证
5. `curl http://<ip>:8080/onvif/snapshot/profile_sub` —— 快照出图

若 2 的 TCP 查询无响应但 ping 通，可能为固件差异（查询端口/命令不同），
抓包官方 APP 对照后再适配。

## 目录结构

```
├── cmd/skydroid-onvif-adapter/   # 入口（flag 装配、快照路由、健康检查）
├── internal/
│   ├── skydroid/                 # TP 协议客户端（UDP 重发、TCP 查询、角度编码）
│   ├── ptz/                      # ONVIF PTZ → TP 映射驱动 + 位置估算器 + 预置位
│   ├── onvif/                    # ONVIF SOAP 服务（Device/Media/PTZ）
│   ├── discovery/                # WS-Discovery 应答器
│   └── snapshot/                 # RTSP 抓帧（ffmpeg）
├── Makefile
└── README.md
```

## 测试

```bash
make test   # go test -race，含端到端用例：
            # 假相机记录 UDP 命令 → 断言 ONVIF 操作被逐字节翻译为 TP 命令
```

端到端覆盖：ContinuousMove 的速度字节与方向量化、AbsoluteMove 的角度补码编码、
Stop、预置位生命周期、内建模式位、Home、未知操作 Fault、设备信息/流地址/快照地址。

## 依赖

- Go 1.24+
- ffmpeg（仅快照功能；无 ffmpeg 时其余功能不受影响）

## 发布与 CI

- push master / PR 自动触发 `build` workflow（go vet + go test -race）
- 发布：打 tag 并创建 GitHub Release，`release` workflow 自动构建六平台
  （linux/darwin/windows × amd64/arm64）tar.gz/zip + checksums 并上传
- 本地验证发布产物：`make snapshot`（需要 [goreleaser](https://goreleaser.com)）
