package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time" 
	"sync"

	"github.com/go-gst/go-glib/glib"
	"github.com/go-gst/go-gst/gst"
	"github.com/go-gst/go-gst/gst/app"
	"github.com/mengelbart/roq"
	"github.com/quic-go/quic-go"
	scream "github.com/mengelbart/scream-go"
)

/*
 * RTCP Packet RFCs:
 * - RTCP APP packet: https://datatracker.ietf.org/doc/html/rfc3550#section-6.7
 * - RTCP Compound packet: https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-rtpme/e2e2deb3-2f87-4932-a9c5-1dbde47fb12d
 * - RTCP CC Feedback packet: https://datatracker.ietf.org/doc/html/rfc8888#section-3.1
 */

// Flow ID constant = 1
var flowID uint64 = 1

/*
 * Video quality struct
 */
type BitrateTier struct{
	quality	string
	targetbitrate float64
	maxbitrate	float64
	width	int
	height	int
}

/*
 * Array of `BitrateTier` structs, each initialized
 * with corresponding video quality attributes
 */
var quality = []BitrateTier{
	{quality: "480p", targetbitrate: 2000000.0, maxbitrate: 2500000.0, width: 854, height: 480},
	{quality: "720p", targetbitrate: 4000000.0, maxbitrate: 5000000.0, width: 1280, height: 720},
	{quality: "1080p", targetbitrate: 8000000.0, maxbitrate: 10000000.0, width: 1920, height: 1080},
}

/*
 * Index constants for indexing `quality` Array
 */
var low int = 0   // quality[0] -> 480p
var mid int = 1   // quality[1] -> 720p
var high int = 2  // quality[2] -> 1080p

/*
 * Packet struct for implementing `Packet` interface
 * from the SCReAM Go wrapper
 */
type Packet struct{
	size int
	sequenceNumber uint16
	timestamp time.Time
}

/*
 * Function for obtaining packet size field
 * Returns size field in int
 */
func (packet* Packet) Size() int{
	return packet.size
}

/*
 * Function for obtaining packet sequence number
 * Returns sequence number in uint16
 */
func (packet* Packet) SequenceNumber() uint16{
	return packet.sequenceNumber
}

/*
 * Function for obtaining packet timestamp
 * Returns timestamp as a Time struct
 */
func (packet* Packet) Timestamp() time.Time{
	return packet.timestamp
}

/*
 * RoQ Server struct
 */
type Server struct {
	/*thread safety*/
	qPipeLock sync.RWMutex // read/write lock for modifying a pipeline element
	
	/*Network attributes*/
	ip   string
	port uint64
	
	/*SCReAM congestion controller*/
	screamCC *scream.Tx
	screamQueue *scream.Queue[*Packet]
	
	/*ABR variables*/
	isStartup	  bool
	Rcurrent	  int
	upgradeTimer *time.Timer

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

/*
 * Function for initializing a Server/sender
 *
 * Parameters:
 *	- ip address string
 *	- port value
 *
 * Returns a `Server` struct
 */
func CreateServer(ip string, port uint64) *Server {
	return &Server {
		ip:             ip,
		port:           port,
		screamCC:       scream.NewTx(),
		screamQueue:	scream.NewQueue[*Packet](),
		isStartup:		true,
		Rcurrent:		low,
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
 * Function for initializing Gstreamer pipeline elements
 *
 * Parameters:
 *	- videoFile string of source video file
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
		`appsink name=sink sync=true max-buffers=1 `, videoFile, quality[low].width, quality[low].height, uint(quality[low].targetbitrate/1000),
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
 * Function to modify Server pipeline's `capsfilter` element to adjust video resolution
 *  and `bitrate` property of the `encoder` element for adjusting video bitrate
 */
func (server *Server) AdjustVideoQuality(tier int){
	server.qPipeLock.Lock()
	defer server.qPipeLock.Unlock()
	
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
	
	fmt.Printf("[%s] [ABR] Adjusted quality to: %s\n", time.Now(), quality[tier].quality)
}

/*
 * Function to initialize a QUIC Connection
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
 * Function for creating RTP over QUIC connection, QUIC session, and sender Flow
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
 * Function to start Server pipelines
 */
func (server *Server) StartStream() error {
	err := server.InitializeServer()
	if err != nil {
		return fmt.Errorf("[Error] Failed to initialize server: %v", err)
	}
	
	go server.ReceiveFeedback()
	
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
			mark := (data[1] & 0x80) != 0

			pkt := &Packet{
				size:           size,
				sequenceNumber: sequenceNumber,
				timestamp:      time.Now(),
			}
			
			server.screamCC.NewMediaFrame(time.Now(), 12345678, size, mark)
			server.screamCC.IsOkToTransmit(time.Now(), 12345678)
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
 * Function for receiving RTCP feedback from the Client
 * Retuned to isolate the trailing 16-byte tracking packet safely
 */
func (server *Server) ReceiveFeedback(){
	for{
		compoundPacket, err := server.quicConnection.ReceiveDatagram(context.Background())
		if err != nil{
			fmt.Printf("[SCReAM Log] Error receiving RTCP feedback: %v\n", err)
			return
		}
		
		if len(compoundPacket) < 16 {
			continue
		}
		
		feedbackSize := len(compoundPacket) - 16
		feedbackPacket := compoundPacket[:feedbackSize]
		appPacket := compoundPacket[feedbackSize:]
		
		server.screamCC.IncomingStandardizedFeedback(time.Now(), feedbackPacket)
		
		rawData := appPacket[12:]
		clientBuffer := make([]uint32, (len(rawData))/4)
		
		for i := 0; i < len(rawData)/4; i++{
			index := i * 4 
			// FIXED: Cast to uint32 BEFORE shifting to prevent 8-bit byte overflow truncation
			clientBuffer[i] = uint32(rawData[index]) << 24 | 
							  uint32(rawData[index+1]) << 16 | 
							  uint32(rawData[index+2]) << 8 | 
							  uint32(rawData[index+3])
		}
		
		if len(clientBuffer) > 0 {
			server.ABR(clientBuffer[0])
		}
	}
}

/*
 * Function for computing safe throughput given SCReAM target bitrate (80% safety multiplier)
 */
func ComputeSafeThroughput(Traw float64) float64{
	return Traw * 0.8
}

/*
 * Helper function for deriving video quality tier
 */
func ComputeTier(q float64) int{
	if q >= quality[high].targetbitrate{
		return high
	}
	if q >= quality[mid].targetbitrate{
		return mid
	}
	return low
}

/*
 * Function for the main ABR Algorithm loop
 */
func (server *Server) ABR(clientBuffer uint32){
	Traw := server.screamCC.GetTargetBitrate(time.Now(), 12345678)
	
	if Traw != -1.0{ 
		fmt.Printf("[%s] [ABR] Bcurrent = %d ms, Rcurrent = %s, Traw = %.2f bps, Tsafe = %.2f bps\n", time.Now(), clientBuffer, quality[server.Rcurrent].quality, Traw, ComputeSafeThroughput(Traw))
		Tsafe := ComputeSafeThroughput(Traw)
		Rtarget := ComputeTier(Tsafe)

		// --------- SCENARIO A ---------
		if Rtarget > server.Rcurrent{
			if server.isStartup{
				fmt.Printf("[%s] [ABR] Fast-start phase: Upgrading bitrate tier to %s\n", time.Now(), quality[Rtarget].quality)
				server.Rcurrent = Rtarget
				server.AdjustVideoQuality(Rtarget)
			} else{
				if server.upgradeTimer == nil{
					upgrade := func(){
						fmt.Printf("[%s] [ABR] Elapsed 5 seconds: Upgrading bitrate tier to %s\n", time.Now(), quality[Rtarget].quality)
						server.Rcurrent = Rtarget
						server.AdjustVideoQuality(Rtarget)
						server.upgradeTimer = nil
					}
					server.upgradeTimer = time.AfterFunc(5 * time.Second, upgrade)
				}
			}
		} else if Rtarget < server.Rcurrent{ // --------- SCENARIO B ---------
			// FIXED: Now safely restored to your 1-second (1000ms) panic threshold
			if clientBuffer > 1000{
				fmt.Printf("[%s] [ABR] Maintaining %s bitrate tier\n", time.Now(), quality[server.Rcurrent].quality)
			} else{		
				fmt.Printf("[%s] [ABR] Buffer depleted (%d ms). Downgrading bitrate tier to %s\n", time.Now(), clientBuffer, quality[Rtarget].quality)
				server.Rcurrent = Rtarget
				server.AdjustVideoQuality(Rtarget)
				server.isStartup = false
			}
		} else{ // --------- SCENARIO C ---------
			if server.upgradeTimer != nil{
				fmt.Printf("[%s] [ABR] Upgrade timer reset \n", time.Now())
				server.upgradeTimer = nil
			}
			if server.Rcurrent == high{
				server.isStartup = false
			}
		}
	}
}

/*
 * Function for cleaning the Server resources
 */
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
	
	time.Sleep(2 * time.Second)
	server.CloseServer()
}
