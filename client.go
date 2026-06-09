package main

import(
	"fmt"
	"crypto/tls"
	"context"
	"net"
	"strings"
	"strconv"
	"os"

	"github.com/quic-go/quic-go"
	"github.com/mengelbart/roq"
	"github.com/go-gst/go-gst/gst"
	"github.com/go-gst/go-gst/gst/app"
	"github.com/go-gst/go-glib/glib"
)

var flowID uint64 = 1

type Client struct{
	ip 			   string
	port 		   uint64
	quicConnection quic.Connection
	receiveFlow    *roq.ReceiveFlow
	
	pipeline 	 *gst.Pipeline
	source 		 *gst.Element
	depayloader  *gst.Element
	parser 		 *gst.Element
	decoder 	 *gst.Element
	videoconvert *gst.Element
	sink 		 *gst.Element
}

func CreateClient(ip string, port uint64) *Client{
	return &Client{
		ip: 			ip,
		port: 			port,
		quicConnection: nil,
		receiveFlow: 	nil,
		pipeline: 		nil,
		source: 		nil,
		depayloader: 	nil,
		parser: 		nil,
		decoder: 		nil,
		videoconvert: 	nil,
		sink: 			nil,
	}
}

func (client *Client) InitializePipeline(){
	pipeline := fmt.Sprintf(
		`appsrc name=source format=time caps="application/x-rtp, media=(string)video, clock-rate=(int)90000, encoding-name=(string)H264, payload=(int)96" !` +
		`rtph264depay name=depayloader !` +
		`h264parse name=parser !` +
		`avdec_h264 name=decoder !` +
		`videoconvert !` +
		`ximagesink name=sink`,
	)

	client.pipeline, _ = gst.NewPipelineFromString(pipeline)

	client.source, _ 	  = client.pipeline.GetElementByName("source")
	client.depayloader, _ = client.pipeline.GetElementByName("depayloader")
	client.parser, _	  = client.pipeline.GetElementByName("parser")
	client.decoder, _	  = client.pipeline.GetElementByName("decoder")
	client.sink, _		  = client.pipeline.GetElementByName("sink")
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
	
	serverAddress, err := net.ResolveUDPAddr("udp", client.ip + ":" + strconv.FormatUint(uint64(client.port), 10))

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
	client.InitializePipeline()
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

func (client *Client) PlayStream() error{
	err := client.InitializeClient()
	
	if err != nil{
		return fmt.Errorf("[Error] Error initializing client: %v\n", err)
	}

	go client.ReceivePackets()
	client.pipeline.SetState(gst.StatePlaying)	
	
	mainLoop := glib.NewMainLoop(nil, true)
	mainLoop.Run()
	
	fmt.Printf("[Status] Streaming finished")
	
	return nil
}

func (client *Client) ReceivePackets(){
	buffer := make([]byte, 65536)
	
	appsource := app.SrcFromElement(client.source)
	
	for{
		size, err := client.receiveFlow.Read(buffer)
		
		if err != nil{
			break
		}
		
		if size == 0{
			continue
		}
		
		bytes := make([]byte, size)
		copy(bytes, buffer[:size])
		gstbuf := gst.NewBufferFromBytes(bytes)
		ret := appsource.PushBuffer(gstbuf)
			
		if ret != gst.FlowOK{
			break
		}
	}
}

func (client *Client) CloseClient(){
	client.receiveFlow.Close()
	client.quicConnection.CloseWithError(0, "QUIC connection closed.")
}

func main() {
	gst.Init(nil)
	addr := "127.0.0.1:5000" //default fallback string

	if len(os.Args) > 1 {
		addr = os.Args[1]
	}

	address := strings.Split(addr, ":")
	ip := address[0]
	port, _ := strconv.ParseUint(address[1], 10, 64)

	client := CreateClient(ip, port)
	fmt.Println("[Status] Client Created")

	_ = client.PlayStream()
	
	client.CloseClient()
}