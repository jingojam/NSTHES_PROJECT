package main

import(
	"fmt"
	"crypto/tls"
	"context"
	"net"
	//"io"
	"os"

	"github.com/quic-go/quic-go"
	"github.com/mengelbart/roq"
	"github.com/pion/rtp"
	//"github.com/pion/rtp/codecs/h264"
)

const flowID uint64 = 1

type Client struct{
	address string
	receiveFlow *roq.ReceiveFlow
}

func CreateClient(address string) *Client{
	return &Client{receiveFlow: nil, address: address}
}

// pointer receiver for initializing QUIC connection
func (client *Client) CreateQUICConnection() (quic.Connection, error){
	udpAddress := &net.UDPAddr{IP: nil, Port: 0}
	
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
	
	serverAddress, err := net.ResolveUDPAddr("udp", client.address)

	if err != nil{
		return nil, fmt.Errorf("Error resolving address: %v", err)
	}
	
	// dialearly is for establishing new connection to peer
	//  but uses 0-RTT otherwise if available (not sure how this is triggered for now)
	quicConnection, err := quicTransport.DialEarly(context.Background(), serverAddress, tlsConfig, quicConfig)
	
	if err != nil{
		return nil, fmt.Errorf("Error establishing QUIC connection: %v", err)
	}
	
	return quicConnection, err
}


func (client *Client) InitializeClient() error{
	quicConnection, err := client.CreateQUICConnection()
	
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
	client.receiveFlow, err = session.NewReceiveFlow(flowID)
	
	if err != nil{
		quicConnection.CloseWithError(0, "QUIC connection closed.")
		return fmt.Errorf("Error establishing send flow: %w\n", err)
	}
	
	fmt.Printf("Listening...\n")
	
	return nil
}

func (client *Client) ReceiveRTP() error{
	buffer := make([]byte, 1500)
	
	for{
		size, err := client.receiveFlow.Read(buffer)
		
		if err != nil{
			continue
		}
		
		if size == 0{
			continue
		}
		
		packet := &rtp.Packet{}
		
		//unmarshal only the actual bytes (0 to size)
		if err := packet.Unmarshal(buffer[:size]); err != nil{
			fmt.Printf("Error unmarshaling RTP: %v\n", err)
			continue //skip the packet and look for the next one
		}

		fmt.Printf("RTP: {pt: %d | seq: %d | ssrc: 0x%X | timestamp: %d | sizeof payload: %d bytes}\n", packet.PayloadType, packet.SequenceNumber, packet.SSRC, packet.Timestamp, len(packet.Payload))
	}
}

func main() {
	addr := "127.0.0.1:5000" //default fallback string

	if len(os.Args) > 1 {
		addr = os.Args[1]
	}

	client := CreateClient(addr)
	fmt.Println("Client Created")
	
	if err := client.InitializeClient(); err != nil{
		fmt.Printf("Initialization failed: %v\n", err)
		os.Exit(1)
	}

	client.ReceiveRTP()
}