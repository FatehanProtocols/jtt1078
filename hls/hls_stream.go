package hls

import (
	"fmt"
	"github.com/lkmio/avformat/libhls"
	"github.com/lkmio/avformat/libmpeg"
	"github.com/lkmio/avformat/utils"
	"github.com/FatehanProtocols/jtt1078/log"
	"github.com/FatehanProtocols/jtt1078/stream"
	"os"
	"path/filepath"
	"strconv"
)

type tsContext struct {
	segmentSeq      int    // 切片序号
	writeBuffer     []byte // ts流的缓冲区, 由TSMuxer使用. 减少用户态和内核态交互，以及磁盘IO频率
	writeBufferSize int    // 已缓存TS流大小

	url  string   // @See TransStream.tsUrl
	path string   // ts切片位于磁盘中的绝对路径
	file *os.File // ts切片文件句柄
}

type TransStream struct {
	stream.BaseTransStream
	muxer   libmpeg.TSMuxer
	context *tsContext

	M3U8Writer     libhls.M3U8Writer
	m3u8Name       string   // m3u8文件名
	m3u8File       *os.File // m3u8文件句柄
	dir            string   // m3u8文件父目录
	tsUrl          string   // m3u8中每个url的前缀, 默认为空, 为了支持绝对路径访问:http://xxx/xxx/xxx.ts
	tsFormat       string   // ts文件名格式
	duration       int      // 切片时长, 单位秒
	playlistLength int      // 最大切片文件个数

	m3u8Sinks      map[stream.SinkID]*M3U8Sink // 保存还未生成mu38文件就拉流的sink, 发送一次后就删除.
	PlaylistFormat *string                     // 位于内存中的播放列表，每个sink都引用指针地址.
}

func (t *TransStream) Input(packet utils.AVPacket) ([][]byte, int64, bool, error) {
	if packet.Index() >= t.muxer.TrackCount() {
		return nil, -1, false, fmt.Errorf("track not available")
	}

	// 创建一下个切片
	// 已缓存时长>=指定时长, 如果存在视频, 还需要等遇到关键帧才切片
	if (!t.ExistVideo || utils.AVMediaTypeVideo == packet.MediaType() && packet.KeyFrame()) && float32(t.muxer.Duration())/90000 >= float32(t.duration) {
		// 保存当前切片文件
		if t.context.file != nil {
			err := t.flushSegment(false)
			if err != nil {
				return nil, -1, false, err
			}
		}

		// 创建新的切片
		if err := t.createSegment(); err != nil {
			return nil, -1, false, err
		}
	}

	pts := packet.ConvertPts(90000)
	dts := packet.ConvertDts(90000)
	if utils.AVMediaTypeVideo == packet.MediaType() {
		t.muxer.Input(packet.Index(), packet.AnnexBPacketData(t.BaseTransStream.Tracks[packet.Index()].Stream), pts, dts, packet.KeyFrame())
	} else {
		t.muxer.Input(packet.Index(), packet.Data(), pts, dts, packet.KeyFrame())
	}

	return nil, -1, true, nil
}

func (t *TransStream) AddTrack(track *stream.Track) error {
	if err := t.BaseTransStream.AddTrack(track); err != nil {
		return err
	}

	var err error
	if utils.AVMediaTypeVideo == track.Stream.Type() {
		data := track.Stream.CodecParameters().AnnexBExtraData()
		_, err = t.muxer.AddTrack(track.Stream.Type(), track.Stream.CodecId(), data)
	} else {
		_, err = t.muxer.AddTrack(track.Stream.Type(), track.Stream.CodecId(), track.Stream.Extra())
	}
	return err
}

func (t *TransStream) WriteHeader() error {
	return t.createSegment()
}

func (t *TransStream) onTSWrite(data []byte) {
	t.context.writeBufferSize += len(data)
}

func (t *TransStream) onTSAlloc(size int) []byte {
	n := len(t.context.writeBuffer) - t.context.writeBufferSize
	if n < size {
		_, _ = t.context.file.Write(t.context.writeBuffer[:t.context.writeBufferSize])
		t.context.writeBufferSize = 0
	}

	return t.context.writeBuffer[t.context.writeBufferSize : t.context.writeBufferSize+size]
}

// 写入新的TS切片，更新M3U8
func (t *TransStream) flushSegment(end bool) error {
	// 写入剩余TS包
	if t.context.writeBufferSize > 0 {
		_, _ = t.context.file.Write(t.context.writeBuffer[:t.context.writeBufferSize])
		t.context.writeBufferSize = 0
	}

	if err := t.context.file.Close(); err != nil {
		return err
	}

	// 删除多余的ts切片文件
	if t.M3U8Writer.Size() >= t.playlistLength {
		_ = os.Remove(t.M3U8Writer.Get(0).Path)
	}

	// 更新m3u8列表
	duration := float32(t.muxer.Duration()) / 90000
	t.M3U8Writer.AddSegment(duration, t.context.url, t.context.segmentSeq, t.context.path)
	m3u8Txt := t.M3U8Writer.String()

	//if end {
	//	m3u8Txt += "#EXT-X-ENDLIST"
	//}

	*t.PlaylistFormat = m3u8Txt

	// 写入最新的m3u8到文件
	if t.m3u8File != nil {
		if _, err := t.m3u8File.Seek(0, 0); err != nil {
			return err
		} else if err = t.m3u8File.Truncate(0); err != nil {
			return err
		} else if _, err = t.m3u8File.Write([]byte(m3u8Txt)); err != nil {
			return err
		}
	}

	// 通知等待m3u8的sink
	// 缓存完第二个切片, 才响应发送m3u8文件. 如果一个切片就发, 播放器缓存少会卡顿.
	if len(t.m3u8Sinks) > 0 && t.M3U8Writer.Size() > 1 {
		for _, sink := range t.m3u8Sinks {
			sink.SendM3U8Data(t.PlaylistFormat)
		}

		t.m3u8Sinks = make(map[stream.SinkID]*M3U8Sink, 0)
	}

	return nil
}

// 创建一个新的ts切片
func (t *TransStream) createSegment() error {
	t.muxer.Reset()

	var tsFile *os.File
	startSeq := t.context.segmentSeq + 1
	for {
		tsName := fmt.Sprintf(t.tsFormat, startSeq)
		// ts文件
		t.context.path = fmt.Sprintf("%s/%s", t.dir, tsName)
		// m3u8列表中切片的url
		t.context.url = fmt.Sprintf("%s%s", t.tsUrl, tsName)

		file, err := os.OpenFile(t.context.path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
		if err == nil {
			tsFile = file
			break
		}

		log.Sugar.Errorf("创建ts切片文件失败 err:%s path:%s", err.Error(), t.context.path)
		if os.IsPermission(err) || os.IsTimeout(err) || os.IsNotExist(err) {
			return err
		}

		// 继续创建TS文件, 认为是文件名冲突, 并且文件已经被打开.
		startSeq++
	}

	t.context.segmentSeq = startSeq
	t.context.file = tsFile
	_ = t.muxer.WriteHeader()
	return nil
}

func (t *TransStream) Close() ([][]byte, int64, error) {
	var err error

	if t.context.file != nil {
		err = t.flushSegment(true)
		err = t.context.file.Close()
		t.context.file = nil
	}

	if t.muxer != nil {
		t.muxer.Close()
		t.muxer = nil
	}

	if t.m3u8File != nil {
		err = t.m3u8File.Close()
		t.m3u8File = nil
	}

	// 如果关闭HLS输出流时, 没有有效切片(推流数据过少), 通知等待的sink
	for _, sink := range t.m3u8Sinks {
		sink.cb(nil)
	}

	t.m3u8Sinks = nil

	return nil, 0, err
}

func DeleteOldSegments(id string) {
	var index int
	for ; ; index++ {
		path := stream.AppConfig.Hls.TSPath(id, strconv.Itoa(index))
		fileInfo, err := os.Stat(path)
		if err != nil && os.IsNotExist(err) {
			break
		} else if fileInfo.IsDir() {
			continue
		}

		_ = os.Remove(path)
	}
}

// NewTransStream 创建HLS传输流
// @Params dir			m3u8的文件夹目录
// @Params m3u8Name	m3u8文件名
// @Params tsFormat	ts文件格式, 例如: %d.ts
// @Params tsUrl   	m3u8中ts切片的url前缀
// @Params parentDir	保存切片的绝对路径. mu38和ts切片放在同一目录下, 目录地址使用parentDir+urlPrefix
// @Params segmentDuration 单个切片时长
// @Params playlistLength 缓存多少个切片
func NewTransStream(dir, m3u8Name, tsFormat, tsUrl string, segmentDuration, playlistLength int, seq int, playlistFormat *string, writer libhls.M3U8Writer) (stream.TransStream, error) {
	// 创建文件夹
	m3u8Path := fmt.Sprintf("%s/%s", dir, m3u8Name)
	if err := os.MkdirAll(filepath.Dir(m3u8Path), 0666); err != nil {
		log.Sugar.Errorf("创建HLS目录失败 err: %s path: %s", err.Error(), m3u8Path)
		return nil, err
	}

	// 创建m3u8文件
	file, err := os.OpenFile(m3u8Path, os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		log.Sugar.Errorf("创建m3u8文件失败 err: %s path: %s", err.Error(), m3u8Path)
		//return nil, err
	}

	transStream := &TransStream{
		m3u8Name:       m3u8Name,
		tsFormat:       tsFormat,
		tsUrl:          tsUrl,
		dir:            dir,
		duration:       segmentDuration,
		playlistLength: playlistLength,
	}

	if writer != nil {
		transStream.M3U8Writer = writer
	} else {
		transStream.M3U8Writer = libhls.NewM3U8Writer(playlistLength)
	}

	if playlistFormat != nil {
		transStream.PlaylistFormat = playlistFormat
	} else {
		transStream.PlaylistFormat = new(string)
	}

	// 创建TS封装器
	muxer := libmpeg.NewTSMuxer()
	muxer.SetWriteHandler(transStream.onTSWrite)
	muxer.SetAllocHandler(transStream.onTSAlloc)

	// ts封装上下文对象
	transStream.context = &tsContext{
		segmentSeq:      seq,
		writeBuffer:     make([]byte, 1024*1024),
		writeBufferSize: 0,
	}

	transStream.muxer = muxer
	transStream.m3u8File = file
	transStream.m3u8Sinks = make(map[stream.SinkID]*M3U8Sink, 24)
	return transStream, nil
}

func TransStreamFactory(source stream.Source, protocol stream.TransStreamProtocol, tracks []*stream.Track) (stream.TransStream, error) {
	id := source.GetID()

	var writer libhls.M3U8Writer
	var playlistFormat *string
	startSeq := -1

	endInfo := source.GetStreamEndInfo()
	if endInfo != nil && endInfo.M3U8Writer != nil {
		writer = endInfo.M3U8Writer
		playlistFormat = endInfo.PlaylistFormat
		startSeq = writer.Get(writer.Size() - 1).Sequence
	}

	// 删除旧的m3u8文件
	//_ = os.Remove(stream.AppConfig.Hls.M3U8Path(id))
	// 删除旧的切片文件
	//go DeleteOldSegments(id)

	return NewTransStream(stream.AppConfig.Hls.M3U8Dir(id), stream.AppConfig.Hls.M3U8Format(id), stream.AppConfig.Hls.TSFormat(id), "", stream.AppConfig.Hls.Duration, stream.AppConfig.Hls.PlaylistLength, startSeq, playlistFormat, writer)
}
