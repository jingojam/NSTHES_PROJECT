package main

import(
	"fmt"
	"context"
	"crypto/tls"
	"net"
	//"io"
	"os"

	"github.com/quic-go/quic-go"
	"github.com/mengelbart/roq"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
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
	clientAddress string
	sendFlow *roq.SendFlow
	packetizer rtp.Packetizer
}

// Server constructor
func CreateServer(clientAddress string) *Server{
	return &Server{
		clientAddress: clientAddress,
		sendFlow: nil,
		packetizer: rtp.NewPacketizer(mtu, pt, ssrc, payloader, sequencer, clockRate),
	}
}

// pointer receiver for initializing QUIC connection
func (s *Server) CreateQUICConnection() (quic.Connection, error){
	udpAddress := &net.UDPAddr{IP: nil, Port: 0}
	
	//create a base UDP socket beneathe QUIC
	udpConnection, err := net.ListenUDP("udp", udpAddress)
	
	if err != nil{
		return nil, fmt.Errorf("Error establishing UDP connection: %w", err)
	}
	
	quicTransport := &quic.Transport{Conn: udpConnection}
	
	// set alpn for tls handshake (roq-10) is the standard for roq
	//  not sure if crucial but InsecureSkipVerify is true which means any tls cert is accepted
	tlsConfig := &tls.Config{NextProtos: []string{"roq-10"}, InsecureSkipVerify: true}
	quicConfig := &quic.Config{EnableDatagrams: true} // configure quic to use quic datagrams
	
	serverAddress, err := net.ResolveUDPAddr("udp", s.clientAddress)

	if err != nil{
		return nil, fmt.Errorf("Error resolving address: %w", err)
	}
	
	// dialearly is for establishing new connection to remote
	//  but uses 0-RTT otherwise if available (not sure how this is triggered for now)
	quicConnection, err := quicTransport.DialEarly(context.Background(), serverAddress, tlsConfig, quicConfig)
	
	if err != nil{
		return nil, fmt.Errorf("Error establishing QUIC connection: %w", err)
	}
	
	return quicConnection, err
}

// pointer receiver for iniializing server session and flow
func (s *Server) InitializeServer(){
	quicConnection, err := s.CreateQUICConnection()
	
	if err != nil{
		fmt.Printf("Error establishing QUIC connection: %w\n", err)
		return
	}
	
	// escalate the base quic connection into ROQ
	roqConnection := roq.NewQUICGoConnection(quicConnection)
	session, err := roq.NewSession(roqConnection, false, nil)
	
	if err != nil{
		quicConnection.CloseWithError(0, "QUIC connection closed.")
		fmt.Printf("Error establishing session: %w\n", err)
		return
	}
	
	// create a sender flow for QUIC packets
	s.sendFlow, err = session.NewSendFlow(flowID)
	
	if err != nil{
		quicConnection.CloseWithError(0, "QUIC connection closed.")
		fmt.Printf("Error establishing send flow: %w\n", err)
		return
	}
}

// pointer receiver for opening video file and streaming
func (s *Server) SendRTP(vfilepath string){
	// access the video file
	vfile, err := os.Open(vfilepath)
	
	if err != nil{
		fmt.Printf("Error opening video file: %w\n", err)
		return
	}
	
	fmt.Printf("Streaming video source %s\n", vfilepath)
	vfile.Close()
}

func main(){
	addr string = "10.0.0.2:5000"
	var vfilepath string
	
	if len(os.Args) > 1{
		addr = args[1]
		vfilepath = args[2]
	}
	
	server := CreateServer(addr)
	
	fmt.Println("Server Created")
	
	// sample file
	server.SendRTP(vfilepath)
}

