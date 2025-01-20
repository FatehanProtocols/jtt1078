package gb28181

import (
	"github.com/FatehanProtocols/jtt1078/stream"
	"github.com/pion/rtp"
)

// UDPSource 国标UDP推流源
type UDPSource struct {
	BaseGBSource

	jitterBuffer  *stream.JitterBuffer
	receiveBuffer *stream.ReceiveBuffer
}

func (u *UDPSource) SetupType() SetupType {
	return SetupUDP
}

// OnOrderedRtp 有序RTP包回调
func (u *UDPSource) OnOrderedRtp(packet interface{}) {
	// 此时还在网络收流携程, 交给Source的主协程处理
	u.PublishSource.Input(packet.(*rtp.Packet).Raw)
}

// InputRtpPacket 将RTP包排序后，交给Source的主协程处理
func (u *UDPSource) InputRtpPacket(pkt *rtp.Packet) error {
	block := u.receiveBuffer.GetBlock()
	copy(block, pkt.Raw)

	pkt.Raw = block[:len(pkt.Raw)]
	u.jitterBuffer.Push(pkt.SequenceNumber, pkt)
	return nil
}

func (u *UDPSource) Close() {
	// 清空剩余在缓冲区的包
	u.jitterBuffer.Flush()
	u.jitterBuffer.SetHandler(nil)

	u.BaseGBSource.Close()
}

func NewUDPSource() *UDPSource {
	u := &UDPSource{
		receiveBuffer: stream.NewReceiveBuffer(1500, stream.ReceiveBufferUdpBlockCount+50),
	}

	u.jitterBuffer = stream.NewJitterBuffer()
	u.jitterBuffer.SetHandler(u.OnOrderedRtp)
	return u
}
