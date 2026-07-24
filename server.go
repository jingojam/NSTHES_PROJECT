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
	"encoding/binary"

	"github.com/go-gst/go-glib/glib"
	"github.com/go-gst/go-gst/gst"
	"github.com/go-gst/go-gst/gst/app"
	"github.com/mengelbart/roq"
	"github.com/quic-go/quic-go"
	"github.com/pion/rtcp"
	scream "github.com/mengelbart/scream-go"
)

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
	mark bool
	payload []byte
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
	paceSenderSignal chan struct{} // go channel for signalling
	
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
		paceSenderSignal:	make(chan struct{}, 1),
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
		`queue max-size-buffers=1000 max-size-bytes=0 max-size-time=0 ! `+
		`h264parse name=parser ! `+
		`avdec_h264 ! `+
		`videoscale ! `+
		`capsfilter name=filter caps=video/x-raw,width=%d,height=%d ! `+
		`x264enc name=encoder bitrate=%d tune=zerolatency speed-preset=ultrafast ! `+
		`rtph264pay name=payloader mtu=1200 config-interval=1 pt=96 ssrc=12345678 ! `+
		`appsink name=sink sync=true max-buffers=100 `, videoFile, quality[low].width, quality[low].height, uint(quality[low].targetbitrate/1000),
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
	
	fmt.Printf("[%s] [ABR] Adjusted quality to: %s\n\n", time.Now().Format("15:04:05.000"), quality[tier].quality)
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
	
	keylogWriter, err := os.OpenFile("secrets.txt", os.O_CREATE|os.O_WRONLY, 0600)
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
	
	server.screamCC.RegisterNewStream(server.screamQueue, 12345678, 1.0, quality[low].targetbitrate, quality[low].targetbitrate, quality[high].maxbitrate)
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
	go server.SendPace()
	
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
			sequenceNumber := binary.BigEndian.Uint16(data[2:4])
			size := len(data)
			mark := (data[1] & 0x80) != 0

			payload := make([]byte, size)
			copy(payload, data)
			
			now := time.Now()

			pkt := &Packet{
				size:           size,
				sequenceNumber: sequenceNumber,
				timestamp:      now,
				mark:			mark,
				payload:		payload,
			}
			
			server.screamQueue.Enqueue(pkt)
			server.screamCC.NewMediaFrame(now, 12345678, pkt.size, pkt.mark)
			fmt.Printf("<%s> [SCReAM Log] %s, queue length = %d, bytes in queue = %d b\n\n", now.Format("15:04:05.000"), server.screamCC.GetStatistics(time.Now()), server.screamQueue.SizeOfQueue(), server.screamQueue.BytesInQueue())
			server.SignalPaceSender() // signal pacing sender, since NewMediaFrame requires IsOkToTransmit to be executed after
			
			return gst.FlowOK
		},
	})
	
	server.pipeline.SetState(gst.StatePlaying)
	mainLoop.Run()
	return nil
}

func(server *Server) SignalPaceSender(){
	select{
		case server.paceSenderSignal <- struct{}{}: // send an empty struct to the signalling channel to trigger a signal
		default:
		// signal is discarded
	}
}

func(server *Server) SendPace(){
	timer := time.NewTimer(0)
	timer.Stop()
	defer timer.Stop()
	
	for{
		// SCReAM check to validate if it's ok to send packets to client
		now := time.Now()
		okToSend := server.screamCC.IsOkToTransmit(now, 12345678)
		
		if okToSend == 0.0{ 		// 0 = ok to transmit
			// dequeue packet from SCReAM queue and then send the rtp packet
			pkt, ok := server.screamQueue.Dequeue()
			
			if ok{
				err := server.sendFlow.WriteRTPBytes(pkt.payload)
				
				if err != nil {
					fmt.Printf("[Server Log] WriteRTPBytes failed: %v\n", err)
					return
				}
				
				// check when is the next time to call IsOkToTransmit again
				nextCall := server.screamCC.AddTransmitted(now, 12345678, pkt.size, pkt.sequenceNumber, pkt.mark)
				
				if nextCall > 0{
					// set a timer of the specified amount if tehre is
					timer.Reset(time.Duration(nextCall * float64(time.Second)))
					
				} else{
					// otherwise if the timer is 0 (now), go back to the start again to call IsOkToTransmit
					continue
				}
			} else{
				timer.Stop()
			}
		} else if okToSend > 0.0{ // >0 = time until IsOkToTransmit() should be called again
			// sleep for okToSend amount of time
			timer.Reset(time.Duration(okToSend * float64(time.Second)))
		} else{ // <0 = queue empty, or the scream congestion window is full
			// wait 
			timer.Stop()
		}
		
		// block the goroutine and wait for timer to end, or signal from other functions
		select{
			case <-timer.C: // if timer ends, jump back to the start of the for-loop
			case <-server.paceSenderSignal: // if signalled by another function, jump back to the start of the for-loop
				timer.Stop()
		}
	}
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
		
		if len(compoundPacket) < 12 {
			continue
		}
		
		feedbackLength := (int(binary.BigEndian.Uint16(compoundPacket[2:4])) + 1) * 4
		if len(compoundPacket) < feedbackLength {
			continue
		}
		
		// get the length fields from the rtcp header
		
		// since receiver reports is first, it is at bytes 2-3)
		rrLen := (int(binary.BigEndian.Uint16(compoundPacket[2:4])) + 1) * 4

		// start offset of SDES (next to RR prefix) is right after RR (last index = size of RR)
		sdesStart := rrLen
		// length field is just the base offset of SDES, bytes 2-3 as usual
		sdesLen := (int(binary.BigEndian.Uint16(compoundPacket[sdesStart+2 : sdesStart+4])) + 1) * 4

		// start offset of feedback packet is sum of sizes of the first 2 packets
		fbStart := sdesStart + sdesLen
		fbLen := (int(binary.BigEndian.Uint16(compoundPacket[fbStart+2 : fbStart+4])) + 1) * 4

		// so on
		appStart := fbStart + fbLen
		rawFeedback := compoundPacket[fbStart : fbStart+fbLen]
		rawAppPacket := compoundPacket[appStart:]
		
		//fmt.Printf("<%s> [ABR] RTCP Feedback=%d bytes, RTCP app=%d bytes\n\n", time.Now().Format("15:04:05.000"), len(feedbackPacket), len(appPacket))
		server.screamCC.IncomingStandardizedFeedback(time.Now(), rawFeedback)
		server.SignalPaceSender()
		
		var appPacket rtcp.ApplicationDefined
		err = appPacket.Unmarshal(rawAppPacket)
		
		if err != nil || len(rawAppPacket) < 8{
			continue
		}

		clientBuffer := binary.BigEndian.Uint64(appPacket.Data[:8])
		
		if len(appPacket.Data[:8]) > 0 {	
			server.ABR(uint32(clientBuffer))
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
		fmt.Printf("<%s> [ABR] Bcurrent = %d ms, Rcurrent = %s, Traw = %.2f bps, Tsafe = %.2f bps\n\n", time.Now().Format("15:04:05.000"), clientBuffer, quality[server.Rcurrent].quality, Traw, ComputeSafeThroughput(Traw))
		Tsafe := ComputeSafeThroughput(Traw)
		Rtarget := ComputeTier(Tsafe)

		// --------- SCENARIO A ---------
		if Rtarget > server.Rcurrent{
			if server.isStartup{
				fmt.Printf("<%s> [ABR] Fast-start phase: Upgrading bitrate tier to %s\n\n", time.Now().Format("15:04:05.000"), quality[Rtarget].quality)
				server.Rcurrent = Rtarget
				server.AdjustVideoQuality(Rtarget)
			} else{
				if server.upgradeTimer == nil{
					upgrade := func(){
						fmt.Printf("<%s> [ABR] Elapsed 5 seconds: Upgrading bitrate tier to %s\n\n", time.Now().Format("15:04:05.000"), quality[Rtarget].quality)
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
				fmt.Printf("<%s> [ABR] Maintaining %s bitrate tier\n\n", time.Now().Format("15:04:05.000"), quality[server.Rcurrent].quality)
			} else{		
				fmt.Printf("<%s> [ABR] Buffer depleted (%d ms). Downgrading bitrate tier to %s\n\n", time.Now().Format("15:04:05.000"), clientBuffer, quality[Rtarget].quality)
				server.Rcurrent = Rtarget
				server.AdjustVideoQuality(Rtarget)
				server.isStartup = false
			}
		} else{ // --------- SCENARIO C ---------
			if server.upgradeTimer != nil{
				fmt.Printf("<%s> [ABR] Upgrade timer reset\n\n", time.Now().Format("15:04:05.000"))
				server.upgradeTimer.Stop()
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
