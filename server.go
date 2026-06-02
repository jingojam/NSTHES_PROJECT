package main

import(
	"fmt"
	"context"
	"crypto/tls"
	"net"
	//"io"
	"os"
	"time"

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
	sendFlow *roq.SendFlow
	packetizer rtp.Packetizer
}

// Server constructor
func CreateServer(address string) *Server{
	return &Server{
		address: address,
		sendFlow: nil,
		packetizer: rtp.NewPacketizer(mtu, pt, ssrc, payloader, sequencer, clockRate),
	}
}

// pointer receiver for initializing QUIC connection
func (server *Server) CreateQUICConnection() (quic.Connection, error){
	udpAddress, err := net.ResolveUDPAddr("udp", server.address)

	if err != nil{
		fmt.Printf("Error listening on UDP socket: %v\n", err)
		return nil, fmt.Errorf("Error listening on UDP socket: %v\n", err)
	}
	
	//create a base UDP socket beneathe QUIC
	udpConnection, err := net.ListenUDP("udp", udpAddress)
	
	if err != nil{
		return nil, fmt.Errorf("Error establishing UDP connection: %v", err)
	}
	
	quicTransport := &quic.Transport{Conn: udpConnection}
	
	// set alpn for tls handshake (roq-10) is the standard for roq
	//  not sure if crucial but InsecureSkipVerify is true which means any tls cert is accepted
	tlsConfig := &tls.Config{NextProtos: []string{"roq-10"}, InsecureSkipVerify: true}
	quicConfig := &quic.Config{EnableDatagrams: true} // configure quic to use quic datagrams

	listener, err := quicTransport.Listen(tlsConfig, quicConfig)
	
	if err != nil{
		fmt.Printf("Error starting QUIC listener: %v\n", err)
		return nil, fmt.Errorf("Error starting QUIC listener: %v\n", err)
	}
	
	quicConnection, err := listener.Accept(context.Background())
	
	if err != nil{
		fmt.Printf("Error accepting QUIC connection: %v\n", err)
		return nil, fmt.Errorf("Error accepting QUIC connection: %v\n", err)
	}

	fmt.Printf("Accepted connection from %s\n", quicConnection.RemoteAddr())
	
	return quicConnection, err
}

// pointer receiver for iniializing server session and flow
func (server *Server) InitializeServer() error{
	quicConnection, err := server.CreateQUICConnection()
	
	if err != nil{
		return fmt.Errorf("Error establishing QUIC connection: %w\n", err)
	}
	
	// escalate the base quic connection into ROQ
	roqConnection := roq.NewQUICGoConnection(quicConnection)
	session, err := roq.NewSession(roqConnection, false, nil)
	
	if err != nil{
		quicConnection.CloseWithError(0, "QUIC connection closed.")
		return fmt.Errorf("Error establishing session: %w\n", err)
	}
	
	// create a sender flow for QUIC packets
	server.sendFlow, err = session.NewSendFlow(flowID)
	
	if err != nil{
		quicConnection.CloseWithError(0, "QUIC connection closed.")
		return fmt.Errorf("Error establishing send flow: %w\n", err)
	}
	
	return nil
}

// pointer receiver for opening video file and streaming
func (server *Server) SendRTP(vfilepath string){
	// access the video file
	vfile, err := os.Open(vfilepath)
	
	if err != nil{
		fmt.Printf("Error opening video file: %v\n", err)
		return
	}
	
	fmt.Printf("Streaming video source %s\n", vfilepath)
	
	// h264 reader 
	reader, _ := h264reader.NewReader(vfile)
	
	ticker := time.NewTicker(33 * time.Millisecond)
	var samples uint32 = clockRate / 30 
	
	//   loop thru all nal units from the reader
	for{
		<-ticker.C
		
		nal, err := reader.NextNAL()
		
		if err != nil{
			fmt.Printf("Error extracting NAL unit: %v\n", err)
			return
		}
		
		if nal == nil{
			break
		}
		
		//skip empty nal units
		if len(nal.Data) == 0{
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
	
	vfile.Close()
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
	fmt.Println("Server Created")

	if err := server.InitializeServer(); err != nil{
		fmt.Printf("Initialization failed: %v\n", err)
		os.Exit(1)
	}

	server.SendRTP(vfilepath)
}
