package transport

import (
	"fmt"
	"net"

	"github.com/assemblaj/ggpo/internal/messages"
	"github.com/assemblaj/ggpo/internal/util"
)

const (
	MaxUDPEndpoints  = 16
	MaxUDPPacketSize = 4096
)

type Udp struct {
	Stats UdpStats // may not need this, may just be a service used by others

	socket         net.Conn
	messageHandler MessageHandler
	listener       *net.UDPConn
	localPort      int
	ipAddress      string
}

type UdpStats struct {
	BytesSent   int
	PacketsSent int
	KbpsSent    float64
}

func getPeerAddress(address net.Addr) peerAddress {
	switch addr := address.(type) {
	case *net.UDPAddr:
		return peerAddress{
			Ip:   addr.IP.String(),
			Port: addr.Port,
		}
	}
	return peerAddress{}
}

func (u Udp) Close() {
	if u.listener != nil {
		u.listener.Close()
	}
}

func NewUdp(messageHandler MessageHandler, localPort int) Udp {
	u := Udp{}
	u.messageHandler = messageHandler

	u.localPort = localPort
	util.Log.Printf("binding udp socket to port %d.\n", localPort)
	udpAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf(":%d", localPort))
	if err != nil {
		panic(err) // TODO Handle error4
	}
	u.listener, err = net.ListenUDP("udp", udpAddr)
	if err != nil {
		panic(err) // TODO Handle error
	}
	return u
}

// dst should be sockaddr
// maybe create Gob encoder and decoder members
// instead of creating them on each message send
func (u Udp) SendTo(msg messages.UDPMessage, remoteIp string, remotePort int) {
	if msg == nil || remoteIp == "" {
		return
	}

	remote, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", remoteIp, remotePort))
	if err != nil {
		panic(err) // TODO Handle error
	}
	buf := msg.ToBytes()
	_, err = u.listener.WriteToUDP(buf, remote)
	if err != nil {
		panic(err) // TODO Handle error
	}
}

func (u Udp) Read(messageChan chan MessageChannelItem) {
	defer u.listener.Close()
	recvBuf := make([]byte, MaxUDPPacketSize*2)
	for {
		l, addr, err := u.listener.ReadFromUDP(recvBuf)
		if err != nil {
			util.Log.Printf("conn.Read error returned: %s\n", err)
			break
		} else if l <= 0 {
			util.Log.Printf("no data recieved\n")
		} else if l > 0 {
			util.Log.Printf("recvfrom returned (len:%d  from:%s).\n", l, addr.String())
			peer := getPeerAddress(addr)

			msg, err := messages.DecodeMessageBinary(recvBuf)
			if err != nil {
				util.Log.Printf("Error decoding message: %s", err)
				continue
			}
			messageChan <- MessageChannelItem{Peer: peer, Message: msg, Length: l}
		}

	}
}

func (u Udp) IsInitialized() bool {
	return u.listener != nil
}
