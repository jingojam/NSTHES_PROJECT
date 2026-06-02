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
	"github.com/pion/rtp/codecs"
)

const flowID uint64 = 1
var annexBCode = []byte{0x00, 0x00, 0x00, 0x01}

type Client struct{
	address string
	quicConnection quic.Connection
	receiveFlow *roq.ReceiveFlow
	depacketizer rtp.Depacketizer
}

func CreateClient(address string) *Client{
	return &Client{
		address: address,
		quicConnection: nil,
		receiveFlow: nil,
		depacketizer: &codecs.H264Packet{},
	}
}

// pointer receiver for initializing QUIC connection
func (client *Client) CreateQUICConnection() error{
	udpAddress := &net.UDPAddr{IP: nil, Port: 0}
	
	//create a base UDP socket beneathe QUIC
	udpConnection, err := net.ListenUDP("udp", udpAddress)
	
	if err != nil{
		return fmt.Errorf("[Error] Error establishing UDP connection: %v", err)
	}
	
	quicTransport := &quic.Transport{Conn: udpConnection}
	
	// set alpn for tls handshake (roq-10) is the standard for roq
	//  not sure if crucial but InsecureSkipVerify is true which means any tls cert is accepted
	tlsConfig := &tls.Config{NextProtos: []string{"roq-10"}, InsecureSkipVerify: true}
	quicConfig := &quic.Config{EnableDatagrams: true} // configure quic to use quic datagrams
	
	serverAddress, err := net.ResolveUDPAddr("udp", client.address)

	if err != nil{
		return fmt.Errorf("[Error] Error resolving address: %v", err)
	}
	
	// dialearly is for establishing new connection to peer
	//  but uses 0-RTT otherwise if available (not sure how this is triggered for now)
	client.quicConnection, err = quicTransport.DialEarly(context.Background(), serverAddress, tlsConfig, quicConfig)
	
	if err != nil{
		client.quicConnection = nil
		return fmt.Errorf("[Error] Error establishing QUIC connection: %v", err)
	}
	
	return nil
}


func (client *Client) InitializeClient() error{
	err := client.CreateQUICConnection()
	
	if err != nil{
		return fmt.Errorf("[Error] Error establishing QUIC connection: %w\n", err)
	}
	
	// escalate the base quic connection into ROQ
	roqConnection := roq.NewQUICGoConnection(client.quicConnection)
	session, err := roq.NewSession(roqConnection, false, nil)
	
	if err != nil{
		client.quicConnection.CloseWithError(0, "[Error] QUIC connection closed.")
		return fmt.Errorf("[Error] Error establishing session: %w\n", err)
	}
	
	// create a sender flow for QUIC packets
	client.receiveFlow, err = session.NewReceiveFlow(flowID)
	
	if err != nil{
		client.quicConnection.CloseWithError(0, "[Error] QUIC connection closed.")
		return fmt.Errorf("[Error] Error establishing send flow: %w\n", err)
	}
	
	fmt.Printf("[Status] Listening...\n")
	
	return nil
}

func (client *Client) ReceiveRTP(){
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
			fmt.Printf("[Error] Error unmarshaling RTP: %v\n", err)
			continue //skip the packet and look for the next one
		}
			
		nal, err := client.depacketizer.Unmarshal(packet.Payload)
			
		if err != nil || len(nal) ==0{
			continue
		}

		fmt.Printf("[RTP] {pt: %d | seq: %d | ssrc: 0x%X | timestamp: %d | sizeof payload: %d bytes}\n", packet.PayloadType, packet.SequenceNumber, packet.SSRC, packet.Timestamp, len(packet.Payload))
	}
	
	fmt.Printf("[Status] Streaming finished")
}

func (client *Client) CloseClient(){
	client.receiveFlow.Close()
	client.quicConnection.CloseWithError(0, "QUIC connection closed.")
}

func main() {
	addr := "127.0.0.1:5000" //default fallback string

	if len(os.Args) > 1 {
		addr = os.Args[1]
	}

	client := CreateClient(addr)
	fmt.Println("[Status] Client Created")
	
	if err := client.InitializeClient(); err != nil{
		fmt.Printf("[Error] Client initialization failed: %v\n", err)
		os.Exit(1)
	}

	client.ReceiveRTP()
	client.CloseClient()
}