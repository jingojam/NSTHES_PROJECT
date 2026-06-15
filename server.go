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
	"sync"

	"github.com/go-gst/go-glib/glib"
	"github.com/go-gst/go-gst/gst"
	"github.com/go-gst/go-gst/gst/app"
	"github.com/mengelbart/roq"
	"github.com/quic-go/quic-go"
	scream "github.com/mengelbart/scream-go"
)

var flowID uint64 = 1

/*video quality modifiers*/
type BitrateTier struct{
	quality	string
	targetbitrate float64
	maxbitrate	float64
	width	int
	height	int
}

/*
	bitrate tiers, and corresponding modifiers
	(as per the paper)
*/
var quality = []BitrateTier{
	{quality: "480p", targetbitrate: 2000000.0, maxbitrate: 2500000.0, width: 854, height: 480},
	{quality: "720p", targetbitrate: 4000000.0, maxbitrate: 5000000.0, width: 1280, height: 720},
	{quality: "1080p", targetbitrate: 8000000.0, maxbitrate: 10000000.0, width: 1920, height: 1080},
}


/*
	quality[low] = "low" quality
	quality[mid] = "mid" quality
	quality[high] = "high" quality
	
	these indexes are just helpers to avoid indexing confusion
*/
var low int = 0
var mid int = 1
var high int = 2

/*
	Packet implementation for SCReAM
*/
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
	/*thread safety*/
	lock sync.RWMutex
	
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
	capsfilter	  *gst.Element
	encoder	  *gst.Element
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
		capsfilter:		nil,
		encoder:		nil,
		demux:          nil,
		parser:         nil,
		payloader:      nil,
		sink:           nil,
	}
}

/*
	initialize server-side gstreamer pipeline and elements
*/
func (server *Server) InitializePipeline(videoFile string) {
	pipeline := fmt.Sprintf(
		`filesrc location=%s name=source ! `+
		`qtdemux name=demux demux.video_0 ! `+
		`queue ! `+
		`h264parse name=parser ! `+
		`avdec_h264 ! `+
		`videoscale ! `+
		`capsfilter name=filter caps=video/x-raw,width=%d,height=%d ! `+
		`x264enc name=encoder bitrate=%d tune=zerolatency speed-preset=ultrafast ! `+
		`rtph264pay name=payloader mtu=1200 config-interval=1 pt=96 ssrc=12345678 ! `+
		`appsink name=sink sync=true max-buffers=1 `, videoFile, quality[low].width, quality[low].height, uint32(quality[low].targetbitrate/1000), //video quality initially lowest
	)
	
	var err error
	
	server.pipeline, err = gst.NewPipelineFromString(pipeline)
	
	if err != nil{
		fmt.Printf("[Error] Failed to create pipeline: %v\n", err)
	}
	
	server.source, err    = server.pipeline.GetElementByName("source")
	
	if err != nil{
		fmt.Printf("[Error] Failed to initialize source element: %v\n", err)
	}
	
	server.demux, err     = server.pipeline.GetElementByName("demux")
	
	if err != nil{
		fmt.Printf("[Error] Failed to initialize demux element: %v\n", err)
	}
	
	server.parser, err    = server.pipeline.GetElementByName("parser")
	
	if err != nil{
		fmt.Printf("[Error] Failed to initialize parser element: %v\n", err)
	}
	
	server.capsfilter, err = server.pipeline.GetElementByName("filter")
	
	if err != nil{
		fmt.Printf("[Error] Failed to initialize capsfilter element: %v\n", err)
	}
	
	server.encoder, err = server.pipeline.GetElementByName("encoder")
	
	if err != nil{
		fmt.Printf("[Error] Failed to initialize encoder element: %v\n", err)
	}
	
	server.payloader, err = server.pipeline.GetElementByName("payloader")
	
	if err != nil{
		fmt.Printf("[Error] Failed to initialize payloader element: %v\n", err)
	}
	
	server.sink, err      = server.pipeline.GetElementByName("sink")
	
	if err != nil{
		fmt.Printf("[Error] Failed to initialize sink element: %v\n", err)
	}
}

/*
	updates server's capsfilter element to adjust video quality
*/
func (server *Server) AdjustVideoQuality(tier int){
	// obtain a lock in case to prevent race conditions
	server.lock.Lock()
	defer server.lock.Unlock()
	
	caps := gst.NewCapsFromString(fmt.Sprintf("video/x-raw, width=%d, height=%d", quality[tier].width, quality[tier].height))
	err := server.capsfilter.SetProperty("caps", caps)
	
	if err != nil{
		fmt.Printf("[Error] Error adjusting video quality: %v\n", err)
		return
	}
	
	err = server.encoder.SetProperty("bitrate", uint(quality[tier].targetbitrate/1000))
	
	if err != nil{
		fmt.Printf("[Error] Error adjusting encoder: %v\n", err)
		return
	}
	
	fmt.Printf("[ABR] Adjusted quality to: %s\n", quality[tier].quality)
}

/*
	creates a bease quic connection
*/
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

/*
	creates roq connection, session, sender flow 
	and SCReAM register
*/
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

/*
	launches server gstreamer pipeline and scream
	monitoring
*/
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

			pkt := &Packet{
				size:           size,
				sequenceNumber: sequenceNumber,
				timestamp:      time.Now(),
			}
			
			server.screamCC.NewMediaFrame(time.Now(), 12345678, size, mark)
			server.screamCC.IsOkToTransmit(time.Now(), 12345678)
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

/*
	helper function for computing safe throughput
*/
func ComputeSafeThroughput(Traw float64) float64{
	//alpha = 0.8
	return Traw * 0.8
}

/*
	helper function for deriving video quality tier
	from current throughput estimation

*/
func ComputeTier(Tsafe float64) int{
	if Tsafe >= quality[high].targetbitrate{
		return high
	}
	
	if Tsafe >= quality[mid].targetbitrate{
		return mid
	}

	return low
}

/*
	function for receiving RTCP feedback from the client
*/
func (server *Server) ReceiveFeedback(){
	for{
		//receive a QUIC datagram
		feedback, err := server.quicConnection.ReceiveDatagram(context.Background())
		
		if err != nil{
			fmt.Printf("[SCReAM Log] Error receiving RTCP feedback: %v\n", err)
			return
		}
		
		// parse feedback
		server.screamCC.IncomingStandardizedFeedback(time.Now(), feedback)
		
		fmt.Println(server.screamCC.GetStatistics(time.Now()))
		
		Traw := server.screamCC.GetTargetBitrate(time.Now(), 12345678)
		
		if Traw != -1.0{
			//Rcurrent := 0
			Tsafe := ComputeSafeThroughput(Traw)
			target := ComputeTier(Tsafe)
			fmt.Printf("[ABR] Traw = %.2f bps, Tsafe = %.2f bps, Tier = %s\n", Traw, ComputeSafeThroughput(Traw), quality[target].quality)
			server.AdjustVideoQuality(target)
			
			/*
			go func(server *Server, Traw float64, Rcurrent string){
				startupPhase := true
				Tsafe := ComputeSafeThroughput(Traw)
				Rtarget := ComputeTier(Tsafe)
				
				if Rtarget > Rcurrent{
					server.AdjustVideoQuality(quality[Rtarget].width, quality[Rtarget].height)
					Rcurrent = Rtarget
				}
				
			}(server, Traw, Rcurrent)
			*/
		}
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
