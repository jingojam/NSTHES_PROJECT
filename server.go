package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time" // Added to handle the 2-second teardown delay

	"github.com/go-gst/go-glib/glib"
	"github.com/go-gst/go-gst/gst"
	"github.com/go-gst/go-gst/gst/app"
	"github.com/mengelbart/roq"
	"github.com/quic-go/quic-go"
	scream "github.com/mengelbart/scream-go"
)

var flowID uint64 = 1

type Packet struct{
	size int
	sequenceNumber uint16
	timestamp time.Time
}

func (packet* Packet) Size() int{
	return packet.size
}

func (packet* Packet) SequenceNumber() uint16{
	return packet.sequenceNumber
}

func (packet* Packet) Timestamp() time.Time{
	return packet.timestamp
}

/*
	RTP/QUIC Server
*/
type Server struct {
	/*Network attributes*/
	ip   string
	port uint64
	
	/*SCReAM congestion controller*/
	screamCC *scream.Tx
	screamQueue *scream.Queue[*Packet]

	/*QUIC transport attributes*/
	quicConnection quic.Connection
	sendFlow       *roq.SendFlow
	
	/*GStreamer pipeline elements*/
	pipeline  *gst.Pipeline
	source    *gst.Element
	parser    *gst.Element
	demux     *gst.Element
	payloader *gst.Element
	sink      *gst.Element
}

func CreateServer(ip string, port uint64) *Server {
	return &Server {
		ip:             ip,
		port:           port,
		screamCC:       scream.NewTx(),
		screamQueue:	scream.NewQueue[*Packet](),
		quicConnection: nil,
		sendFlow:       nil,
		pipeline:       nil,
		source:         nil,
		demux:          nil,
		parser:         nil,
		payloader:      nil,
		sink:           nil,
	}
}

func (server *Server) InitializePipeline(videoFile string) {
	pipeline := fmt.Sprintf(
		`filesrc location=%s name=source !`+
		`qtdemux name=demux demux.video_0 !`+
		`queue !`+
		`h264parse name=parser !`+
		`rtph264pay name=payloader mtu=1200 config-interval=1 pt=96 ssrc=12345678 !`+
		`appsink name=sink sync=true max-buffers=1`, videoFile,
	)
	
	server.pipeline, _ = gst.NewPipelineFromString(pipeline)
	
	server.source, _    = server.pipeline.GetElementByName("source")
	server.demux, _     = server.pipeline.GetElementByName("demux")
	server.parser, _    = server.pipeline.GetElementByName("parser")
	server.payloader, _ = server.pipeline.GetElementByName("payloader")
	server.sink, _      = server.pipeline.GetElementByName("sink")
}

func (server *Server) CreateQUICConnection() error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("[Error] Unable to fetch current working directory: %v", err)
	}
	
	certPath := cwd + "/server.crt"
	keyPath  := cwd + "/server.key"
	
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return fmt.Errorf("[Error] Error loading self-signed certificate: %v", err)
	}
	
	udpAddress, err := net.ResolveUDPAddr("udp", server.ip+":"+strconv.FormatUint(uint64(server.port), 10))
	if err != nil {
		return fmt.Errorf("[Error] Error resolving address: %v", err)
	}
	
	udpConnection, err := net.ListenUDP("udp", udpAddress)
	if err != nil {
		return fmt.Errorf("[Error] Error listening on UDP socket: %v", err)
	}
	
	keylogWriter, err := os.OpenFile("secrets.txt", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	quicTransport := &quic.Transport{Conn: udpConnection}
	
	tlsConfig := &tls.Config {
		Certificates: []tls.Certificate{cert}, 
		NextProtos:   []string{"roq-10"}, 
		KeyLogWriter: keylogWriter,
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			return &tls.Config {
				Certificates: []tls.Certificate{cert}, 
				NextProtos:   []string{"roq-10"},
				KeyLogWriter: keylogWriter,
			}, nil
		},
	}
	quicConfig := &quic.Config{EnableDatagrams: true}

	listener, err := quicTransport.Listen(tlsConfig, quicConfig)
	if err != nil {
		return fmt.Errorf("[Error] Error starting QUIC listener: %v", err)
	}
	
	server.quicConnection, err = listener.Accept(context.Background())
	if err != nil {
		server.quicConnection = nil
		return fmt.Errorf("[Error] Error accepting QUIC connection: %v", err)
	}

	fmt.Printf("[Status] Accepted connection from %s\n", server.quicConnection.RemoteAddr())
	return nil
}

func (server *Server) InitializeServer() error {
	err := server.CreateQUICConnection()
	if err != nil {
		return fmt.Errorf("[Error] Error establishing QUIC connection: %w", err)
	}
	
	roqConnection := roq.NewQUICGoConnection(server.quicConnection)
	
	session, err := roq.NewSession(roqConnection, true, nil)
	if err != nil {
		server.quicConnection.CloseWithError(0, "[Error] QUIC connection closed.")
		return fmt.Errorf("[Error] Error establishing session: %w", err)
	}
	
	server.sendFlow, err = session.NewSendFlow(flowID)
	if err != nil {
		server.quicConnection.CloseWithError(0, "[Error] QUIC connection closed.")
		return fmt.Errorf("[Error] Error establishing send flow: %w", err)
	}
	
	server.screamCC.RegisterNewStream(server.screamQueue, 12345678, 1.0, 100000, 50000000, 50000000)
	
	fmt.Println("[Status] Server initialized")
	
	return nil
}

func (server *Server) StartStream() error {
	err := server.InitializeServer()
	if err != nil {
		return fmt.Errorf("[Error] Failed to initialize server: %v", err)
	}
	
	//create a background thread for listening to feedback from the client
	go server.ReceiveFeedback()
	
	// Create MainLoop prior to callbacks so closure context captures it smoothly
	mainLoop := glib.NewMainLoop(nil, true)

	appSink := app.SinkFromElement(server.sink)
	appSink.SetCallbacks(&app.SinkCallbacks{
		NewSampleFunc: func(sink *app.Sink) gst.FlowReturn {
			sample := sink.PullSample()
			if sample == nil {
				fmt.Println("[Server Log] End of video file reached (EOS). Stopping engine...")
				mainLoop.Quit() 
				return gst.FlowEOS
			}
			
			buffer := sample.GetBuffer()
			if buffer == nil {
				return gst.FlowError
			}
			
			mapinfo := buffer.Map(gst.MapRead)
			defer buffer.Unmap()
			
			data:= mapinfo.Bytes()
			sequenceNumber := uint16(data[2])<<8 | uint16(data[3])
			size := len(data)
			mark := (data[1] & 0x80) != 0 // RTP marker bit
			//now := time.Now()
			//ssrc := uint32(data[8])<<24 | uint32(data[9])<<16 | uint32(data[10])<<8 | uint32(data[11])			
			
			pkt := &Packet{
				size:           size,
				sequenceNumber: sequenceNumber,
				timestamp:      time.Now(),
			}
			
			if mark{
				server.screamCC.NewMediaFrame(time.Now(), 12345678, size, mark)
			}
			//fmt.Printf("[Server Log] Pushing RTP packet to RoQ flow, size: %d bytes\n", len(data))
			
			server.screamQueue.Enqueue(pkt)
			
			err := server.sendFlow.WriteRTPBytes(data)
			if err != nil {
				fmt.Printf("[Server Log] WriteRTPBytes failed: %v\n", err)
				return gst.FlowError
			}
		
			server.screamQueue.Dequeue()
			server.screamCC.AddTransmitted(time.Now(), 12345678, size, sequenceNumber, mark)
			
			return gst.FlowOK
		},
	})
	
	server.pipeline.SetState(gst.StatePlaying)
	mainLoop.Run()
	
	return nil
}

func (server *Server) ReceiveFeedback(){
	for{
		feedback, err := server.quicConnection.ReceiveDatagram(context.Background())
		
		if err != nil{
			fmt.Printf("[SCReAM Log] Error receiving RTCP feedback: %v\n", err)
			return
		}
		
		//now := time.Now()
		
		server.screamCC.IncomingStandardizedFeedback(time.Now(), feedback)
		
		fmt.Println(server.screamCC.GetStatistics(time.Now()))
		
		bitrate := server.screamCC.GetTargetBitrate(time.Now(), 12345678)
        fmt.Printf("[Debug] target bitrate after feedback = %f\n", bitrate)
	}
}

func (server *Server) CloseServer() {
	if server.pipeline != nil {
		server.pipeline.SetState(gst.StateNull)
	}
	if server.sendFlow != nil {
		server.sendFlow.Close()
	}
	if server.quicConnection != nil {
		server.quicConnection.CloseWithError(0, "[Status] QUIC connection closed.")
	}
}

func main() {
	gst.Init(nil)
	
	addr := "127.0.0.1:5000"
	var vfilepath string

	if len(os.Args) > 2 {
		addr = os.Args[1]
		vfilepath = os.Args[2]
	} else if len(os.Args) > 1 {
		vfilepath = os.Args[1]
	}
	
	address := strings.Split(addr, ":")
	ip := address[0]
	port, _ := strconv.ParseUint(address[1], 10, 64)

	server := CreateServer(ip, port)
	fmt.Println("[Status] Server Created")

	server.InitializePipeline(vfilepath)
	err := server.StartStream()
	if err != nil {
		fmt.Printf("[Error] Error streaming video %v\n", err)
		server.CloseServer()
		os.Exit(1)
	}
	
	// FIX APPLIED HERE:
	// Gives the QUIC stack and Mininet link 2 seconds to flush the final trailing frames
	// before tearing down the connection.
	time.Sleep(2 * time.Second)
	
	server.CloseServer()
}
