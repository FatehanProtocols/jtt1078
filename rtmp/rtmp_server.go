package rtmp

import (
	"github.com/FatehanProtocols/jtt1078/log"
	"github.com/FatehanProtocols/jtt1078/stream"
	"net"
	"runtime"

	"github.com/lkmio/avformat/transport"
	"github.com/lkmio/avformat/utils"
)

type Server interface {
	Start(addr net.Addr) error

	Close()
}

type server struct {
	stream.StreamServer[*Session]

	tcp *transport.TCPServer
}

func (s *server) Start(addr net.Addr) error {
	utils.Assert(s.tcp == nil)

	tcp := &transport.TCPServer{
		ReuseServer: transport.ReuseServer{
			EnableReuse:      true,
			ConcurrentNumber: runtime.NumCPU(),
		},
	}

	if err := tcp.Bind(addr); err != nil {
		return err
	}

	tcp.SetHandler(s)
	tcp.Accept()
	s.tcp = tcp
	return nil
}

func (s *server) Close() {
	panic("implement me")
}

func (s *server) OnNewSession(conn net.Conn) *Session {
	return NewSession(conn)
}

func (s *server) OnCloseSession(session *Session) {
	session.Close()
}

func (s *server) OnPacket(conn net.Conn, data []byte) []byte {
	s.StreamServer.OnPacket(conn, data)

	session := conn.(*transport.Conn).Data.(*Session)
	err := session.Input(data)

	if err != nil {
		log.Sugar.Errorf("处理rtmp包失败 err:%s conn:%s", err.Error(), conn.RemoteAddr().String())
		_ = conn.Close()
	}

	if session.isPublisher {
		return session.receiveBuffer.GetBlock()
	}

	return nil
}

func NewServer() Server {
	s := &server{}
	s.StreamServer = stream.StreamServer[*Session]{
		SourceType: stream.SourceTypeRtmp,
		Handler:    s,
	}
	return s
}
