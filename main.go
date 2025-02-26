package main

import (
	"encoding/json"
	"github.com/lkmio/avformat/transport"
	"github.com/FatehanProtocols/jtt1078/flv"
	"github.com/FatehanProtocols/jtt1078/gb28181"
	"github.com/FatehanProtocols/jtt1078/hls"
	"github.com/FatehanProtocols/jtt1078/jt1078"
	"github.com/FatehanProtocols/jtt1078/log"
	"github.com/FatehanProtocols/jtt1078/record"
	"github.com/FatehanProtocols/jtt1078/rtc"
	"github.com/FatehanProtocols/jtt1078/rtmp"
	"github.com/FatehanProtocols/jtt1078/rtsp"
	"github.com/FatehanProtocols/jtt1078/stream"
	"go.uber.org/zap/zapcore"
	"net"
	"net/http"
	_ "net/http/pprof"
	"strconv"
)

func init() {
	stream.RegisterTransStreamFactory(stream.TransStreamRtmp, rtmp.TransStreamFactory)
	stream.RegisterTransStreamFactory(stream.TransStreamHls, hls.TransStreamFactory)
	stream.RegisterTransStreamFactory(stream.TransStreamFlv, flv.TransStreamFactory)
	stream.RegisterTransStreamFactory(stream.TransStreamRtsp, rtsp.TransStreamFactory)
	stream.RegisterTransStreamFactory(stream.TransStreamRtc, rtc.TransStreamFactory)
	stream.RegisterTransStreamFactory(stream.TransStreamGBStreamForward, gb28181.TransStreamFactory)
	stream.SetRecordStreamFactory(record.NewFLVFileSink)
	stream.StreamEndInfoBride = NewStreamEndInfo

	config, err := stream.LoadConfigFile("./config.json")
	if err != nil {
		panic(err)
	}

	stream.SetDefaultConfig(config)

	options := map[string]stream.EnableConfig{
		"rtmp":    &config.Rtmp,
		"rtsp":    &config.Rtsp,
		"hls":     &config.Hls,
		"webrtc":  &config.WebRtc,
		"gb28181": &config.GB28181,
		"jt1078":  &config.JT1078,
		"hooks":   &config.Hooks,
		"record":  &config.Record,
	}

	// 读取运行参数
	disableOptions, enableOptions := readRunArgs()
	mergeArgs(options, disableOptions, enableOptions)

	stream.AppConfig = *config

	if stream.AppConfig.Hooks.Enable {
		stream.InitHookUrls()
	}

	if stream.AppConfig.WebRtc.Enable {
		// 设置公网IP和端口
		rtc.InitConfig()
	}

	// 初始化日志
	log.InitLogger(config.Log.FileLogging, zapcore.Level(stream.AppConfig.Log.Level), stream.AppConfig.Log.Name, stream.AppConfig.Log.MaxSize, stream.AppConfig.Log.MaxBackup, stream.AppConfig.Log.MaxAge, stream.AppConfig.Log.Compress)

	if stream.AppConfig.GB28181.Enable && stream.AppConfig.GB28181.IsMultiPort() {
		gb28181.TransportManger = transport.NewTransportManager(uint16(stream.AppConfig.GB28181.Port[0]), uint16(stream.AppConfig.GB28181.Port[1]))
	}

	if stream.AppConfig.Rtsp.Enable && stream.AppConfig.Rtsp.IsMultiPort() {
		rtsp.TransportManger = transport.NewTransportManager(uint16(stream.AppConfig.Rtsp.Port[1]), uint16(stream.AppConfig.Rtsp.Port[2]))
	}

	// 打印配置信息
	indent, _ := json.MarshalIndent(stream.AppConfig, "", "\t")
	log.Sugar.Infof("server config:\r\n%s", indent)
}

func main() {
	if stream.AppConfig.Rtmp.Enable {
		rtmpAddr, err := net.ResolveTCPAddr("tcp", stream.ListenAddr(stream.AppConfig.Rtmp.Port))
		if err != nil {
			panic(err)
		}

		server := rtmp.NewServer()
		err = server.Start(rtmpAddr)
		if err != nil {
			panic(err)
		}

		log.Sugar.Info("启动rtmp服务成功 addr:", rtmpAddr.String())
	}

	if stream.AppConfig.Rtsp.Enable {
		rtspAddr, err := net.ResolveTCPAddr("tcp", stream.ListenAddr(stream.AppConfig.Rtsp.Port[0]))
		if err != nil {
			panic(rtspAddr)
		}

		server := rtsp.NewServer(stream.AppConfig.Rtsp.Password)
		err = server.Start(rtspAddr)
		if err != nil {
			panic(err)
		}

		log.Sugar.Info("启动rtsp服务成功 addr:", rtspAddr.String())
	}

	log.Sugar.Info("启动http服务 addr:", stream.ListenAddr(stream.AppConfig.Http.Port))
	go startApiServer(net.JoinHostPort(stream.AppConfig.ListenIP, strconv.Itoa(stream.AppConfig.Http.Port)))

	// 单端口模式下, 启动时就创建收流端口
	// 多端口模式下, 创建GBSource时才创建收流端口
	if stream.AppConfig.GB28181.Enable && !stream.AppConfig.GB28181.IsMultiPort() {
		if stream.AppConfig.GB28181.IsEnableUDP() {
			server, err := gb28181.NewUDPServer(gb28181.NewSSRCFilter(128))
			if err != nil {
				panic(err)
			}

			gb28181.SharedUDPServer = server
			log.Sugar.Info("启动GB28181 udp收流端口成功:" + stream.ListenAddr(stream.AppConfig.GB28181.Port[0]))
		}

		if stream.AppConfig.GB28181.IsEnableTCP() {
			server, err := gb28181.NewTCPServer(gb28181.NewSSRCFilter(128))
			if err != nil {
				panic(err)
			}

			gb28181.SharedTCPServer = server
			log.Sugar.Info("启动GB28181 tcp收流端口成功:" + stream.ListenAddr(stream.AppConfig.GB28181.Port[0]))
		}
	}

	if stream.AppConfig.JT1078.Enable {
		jtAddr, err := net.ResolveTCPAddr("tcp", stream.ListenAddr(stream.AppConfig.JT1078.Port))
		if err != nil {
			panic(err)
		}

		server := jt1078.NewServer()
		err = server.Start(jtAddr)
		if err != nil {
			panic(err)
		}

		log.Sugar.Info("启动jt1078服务成功 addr:", jtAddr.String())
	}

	if stream.AppConfig.Hooks.IsEnableOnStarted() {
		go func() {
			_, _ = stream.Hook(stream.HookEventStarted, "", nil)
		}()
	}

	// 开启pprof调试
	err := http.ListenAndServe(":19999", nil)
	if err != nil {
		println(err)
	}

	select {}
}
