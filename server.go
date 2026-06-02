package main

import(
	"fmt"
	"context"
	"crypto/tls"
	"net"
	"io"
	"os"
	"time"
	"errors"

	"github.com/quic-go/quic-go"
	"github.com/mengelbart/roq"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v3/pkg/media/h264reader"
	//"github.com/pion/rtp/codecs/h264"
)

// reference for these values: https://www.iana.org/assignments/rtp-parameters/rtp-parameters.xhtml
var(
	payloader = &codecs.H264Payloader{}
	sequencer = rtp.NewRandomSequencer()
	mtu uint16 = 1200
	pt uint8 = 96
	ssrc uint32 = 0xC0FFEE 
	clockRate uint32 = 90000
	flowID uint64 = 1
)

/*
	RTP/QUIC Server
*/
type Server struct{
	address string
	quicConnection quic.Connection
	sendFlow *roq.SendFlow
	packetizer rtp.Packetizer
}

// Server constructor
func CreateServer(address string) *Server{
	return &Server{
		address: address,
		quicConnection: nil,
		sendFlow: nil,
		packetizer: rtp.NewPacketizer(mtu, pt, ssrc, payloader, sequencer, clockRate),
	}
}

// pointer receiver for initializing QUIC connection
func (server *Server) CreateQUICConnection() error{
	//read self-signed CA cert
	cert, err := tls.LoadX509KeyPair("server.crt", "server.key")
	
	if err != nil{
		return fmt.Errorf("[Error] Error loading self-signed certificate: %v\n", err)
	}
	
	udpAddress, err := net.ResolveUDPAddr("udp", server.address)

	if err != nil{
		return fmt.Errorf("[Error] Error resoving address: %v\n", err)
	}
	
	//create a base UDP socket beneathe QUIC
	udpConnection, err := net.ListenUDP("udp", udpAddress)
	
	if err != nil{
		return fmt.Errorf("[Error] Error listening on UDP socket: %v\n", err)
	}
	
	quicTransport := &quic.Transport{Conn: udpConnection}
	
	// set alpn for tls handshake (roq-10) is the standard for roq
	// set the  tls certificate to the loaded CA cert as well
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert}, 
		NextProtos: []string{"roq-10"}, 
		
		// accept client regardless of client cert (for now)
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error){
			return &tls.Config{
				Certificates: []tls.Certificate{cert}, 
				NextProtos: []string{"roq-10"},
			}, nil
		},
	}
	quicConfig := &quic.Config{EnableDatagrams: true} // configure quic to use quic datagrams

	listener, err := quicTransport.Listen(tlsConfig, quicConfig)
	
	if err != nil{
		return fmt.Errorf("[Error] Error starting QUIC listener: %v\n", err)
	}
	
	server.quicConnection, err = listener.Accept(context.Background())
	
	if err != nil{
		server.quicConnection = nil
		return fmt.Errorf("[Error] Error accepting QUIC connection: %v\n", err)
	}

	fmt.Printf("[Status] Accepted connection from %s\n", server.quicConnection.RemoteAddr())
	return nil
}

// pointer receiver for iniializing server session and flow
func (server *Server) InitializeServer() error{
	err := server.CreateQUICConnection()
	
	if err != nil{
		return fmt.Errorf("[Error] Error establishing QUIC connection: %w\n", err)
	}
	
	// escalate the base quic connection into ROQ
	roqConnection := roq.NewQUICGoConnection(server.quicConnection)
	session, err := roq.NewSession(roqConnection, false, nil)
	
	if err != nil{
		server.quicConnection.CloseWithError(0, "[Error] QUIC connection closed.")
		return fmt.Errorf("[Error] Error establishing session: %w\n", err)
	}
	
	// create a sender flow for QUIC packets
	server.sendFlow, err = session.NewSendFlow(flowID)
	
	if err != nil{
		server.quicConnection.CloseWithError(0, "[Error] QUIC connection closed.")
		return fmt.Errorf("[Error] Error establishing send flow: %w\n", err)
	}
	
	fmt.Println("[Status] Server initialized")
	return nil
}

// pointer receiver for opening video file and streaming
func (server *Server) SendRTP(vfilepath string) error{
	// access the video file
	vfile, err := os.Open(vfilepath)
	
	if err != nil{
		return fmt.Errorf("[Error] Error opening video file: %v\n", err)
	}
	
	fmt.Printf("[Status] Streaming video source %s\n", vfilepath)
	
	// h264 reader 
	reader, _ := h264reader.NewReader(vfile)
	
	ticker := time.NewTicker(33 * time.Millisecond)
	var samples uint32 = clockRate / 30 
	
	//   loop thru all nal units from the reader
	for{
		<-ticker.C
		
		nal, err := reader.NextNAL()
		
		if err != nil{
			if errors.Is(err, io.EOF){
				fmt.Println("[Status ] Video ended")
				break
			}
			return fmt.Errorf("[Error] Error extracting NAL unit: %v\n", err)
		}
		
		//skip empty nal units
		if nal == nil || len(nal.Data) == 0{
			continue
		}

		//packetize nal unit
		packets := server.packetizer.Packetize(nal.Data, samples)
		
		// send packets to roq buffwer
		for _, packet := range packets{
			rawb, err := packet.Marshal()
			_ = err
			err = server.sendFlow.WriteRTPBytes(rawb)
		}
	}
	
	fmt.Printf("[Status] Streaming finished.")
	vfile.Close()
	
	return nil
}

func (server *Server) CloseServer(){
	server.sendFlow.Close()
	server.quicConnection.CloseWithError(0, "[Status] QUIC connection closed.")
}

func main(){
	addr := "127.0.0.1:5000"
	var vfilepath string

	if len(os.Args) > 2 {
		addr = os.Args[1]
		vfilepath = os.Args[2]
	} else if len(os.Args) > 1 {
		vfilepath = os.Args[1]
	}

	server := CreateServer(addr)
	fmt.Println("[Status] Server Created")

	if err := server.InitializeServer(); err != nil{
		fmt.Printf("[Error] Server initialization failed: %v\n", err)
		server.CloseServer()
		os.Exit(1)
	}

	err := server.SendRTP(vfilepath)
	
	if err != nil{
		fmt.Printf("[Error] Error sending RTP: %v\n", err)
		server.CloseServer()
		os.Exit(1)
	}
	
	server.CloseServer()
}
