package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"time" 
	"sync"
	"sync/atomic"
	"encoding/binary"
	"errors"

	"github.com/go-gst/go-glib/glib"
	"github.com/go-gst/go-gst/gst"
	"github.com/go-gst/go-gst/gst/app"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/logging"
	"github.com/pion/rtcp"
)

// Flow ID constant = 1
var flowID uint64 = 1

// Global logger: dual-writes to stdout AND a log file, so you get a
// persistent, greppable record instead of relying on a scrollback buffer
// that moves faster than you can read it.
var logger *log.Logger
var logFile *os.File

func InitLogger(logFileName string) {
	var err error
	logFile, err = os.OpenFile(logFileName, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		fmt.Printf("[Error] Failed to open log file %s: %v. Falling back to stdout only.\n", logFileName, err)
		logger = log.New(os.Stdout, "", log.LstdFlags|log.Lmicroseconds)
		return
	}
	multiWriter := io.MultiWriter(os.Stdout, logFile)
	logger = log.New(multiWriter, "", log.LstdFlags|log.Lmicroseconds)
	logger.Println("[Status] Logging module initialized successfully.")
}

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
 * RoQ Server struct
 */
type QUICMetrics struct{
	cwndBps	float64
	inFlightBps	float64
	smoothedRTT time.Duration
}

type Server struct {
	/*thread safety*/
	qPipeLock sync.RWMutex // read/write lock for modifying a pipeline element
	
	/*Network attributes*/
	ip   string
	port uint64
	
	/*ABR variables*/
	metrics atomic.Pointer[QUICMetrics]
	R_current int
	isStartup	bool
	stabilityTimer time.Time
	isStabilityTimerActive bool
	stableSince time.Time
	
	connectionStartTime time.Time
	/*Log throttling -- see StartStream's NewSampleFunc and ABR() below.*/
	lastQuicStatsLogAt time.Time
	lastAbrLogAt       time.Time

	/*QUIC transport attributes*/
	quicConnection *quic.Conn
	
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
		R_current:		low,
		isStartup:		true,
		isStabilityTimerActive:	false,
		quicConnection: nil,
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
		logger.Printf("[Error] Failed to create pipeline: %v\n", err)
	}
	
	server.source, err    = server.pipeline.GetElementByName("source")
	if err != nil{
		logger.Printf("[Error] Failed to initialize source element: %v\n", err)
	}
	
	server.demux, err     = server.pipeline.GetElementByName("demux")
	if err != nil{
		logger.Printf("[Error] Failed to initialize demux element: %v\n", err)
	}
	
	server.parser, err    = server.pipeline.GetElementByName("parser")
	if err != nil{
		logger.Printf("[Error] Failed to initialize parser element: %v\n", err)
	}
	
	server.capsfilter, err = server.pipeline.GetElementByName("filter")
	if err != nil{
		logger.Printf("[Error] Failed to initialize capsfilter element: %v\n", err)
	}
	
	server.encoder, err = server.pipeline.GetElementByName("encoder")
	if err != nil{
		logger.Printf("[Error] Failed to initialize encoder element: %v\n", err)
	}
	
	server.payloader, err = server.pipeline.GetElementByName("payloader")
	if err != nil{
		logger.Printf("[Error] Failed to initialize payloader element: %v\n", err)
	}
	
	server.sink, err      = server.pipeline.GetElementByName("sink")
	if err != nil{
		logger.Printf("[Error] Failed to initialize sink element: %v\n", err)
	}
}

/*
 * Function to modify Server pipeline's `capsfilter` element to adjust video resolution
 *  and `bitrate` property of the `encoder` element for adjusting video bitrate
 */
func (server *Server) AdjustVideoQuality(tier int){
	caps := gst.NewCapsFromString(fmt.Sprintf("video/x-raw, width=%d, height=%d", quality[tier].width, quality[tier].height))
	err := server.capsfilter.SetProperty("caps", caps)
	if err != nil{
		logger.Printf("[Error] Error adjusting video quality: %v\n", err)
		return
	}
	
	err = server.encoder.SetProperty("bitrate", uint(quality[tier].targetbitrate/1000))
	if err != nil{
		logger.Printf("[Error] Error adjusting encoder: %v\n", err)
		return
	}
	
	logger.Printf("[%s] [ABR] Adjusted quality to: %s\n\n", time.Now().Format("15:04:05.000"), quality[tier].quality)
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
	quicConfig := &quic.Config{
		EnableDatagrams: true,
		Tracer: func(ctx context.Context, perspective logging.Perspective, connID quic.ConnectionID) *logging.ConnectionTracer{
			return &logging.ConnectionTracer{
				UpdatedMetrics: func(rttStats *logging.RTTStats, cwnd, bytesInFlight logging.ByteCount, packetsInFlight int){
					srtt := rttStats.SmoothedRTT()
					if srtt <= 0 {
						return
					}

					values := &QUICMetrics{
						cwndBps:     float64(cwnd) * 8 / srtt.Seconds(),
						inFlightBps: float64(bytesInFlight) * 8 / srtt.Seconds(),
						smoothedRTT: srtt,
					}

					// atomic scope would rewrite the value on the pointer 
					server.metrics.Store(values)
				},
			}
		},
	}

	listener, err := quicTransport.Listen(tlsConfig, quicConfig)
	if err != nil {
		return fmt.Errorf("[Error] Error starting QUIC listener: %v", err)
	}
	
	server.quicConnection, err = listener.Accept(context.Background())
	if err != nil {
		server.quicConnection = nil
		return fmt.Errorf("[Error] Error accepting QUIC connection: %v", err)
	}

	logger.Printf("[Status] Accepted connection from %s\n", server.quicConnection.RemoteAddr())
	return nil
}

/*
 * Function to start Server pipelines
 */
func (server *Server) StartStream(mainLoop *glib.MainLoop) error {
	_ = server.CreateQUICConnection()

	// Marks t=0 for the Fast-Start phase (see ABR()). Set here rather than
	// in CreateServer() so it reflects when the connection actually came up,
	// not when the process started.
	server.connectionStartTime = time.Now()

	streamCtx, stopStream := context.WithCancel(context.Background())
	defer stopStream()
	
	go server.ReceiveFeedback(streamCtx)
	
	go func() {
		<-streamCtx.Done()
		glib.IdleAdd(func() bool{
			mainLoop.Quit()
			return false
		})
	}()
	
	appSink := app.SinkFromElement(server.sink)
	appSink.SetCallbacks(&app.SinkCallbacks{
		EOSFunc: func(sink *app.Sink) {
			logger.Println("[Server Log] End of video file reached (EOS). Stopping engine...")
			stopStream()
		},
		NewSampleFunc: func(sink *app.Sink) gst.FlowReturn {
			sample := sink.PullSample()
			if sample == nil {
				return gst.FlowEOS
			}
			
			buffer := sample.GetBuffer()
			if buffer == nil {
				return gst.FlowError
			}
			
			mapinfo := buffer.Map(gst.MapRead)
			defer buffer.Unmap()
			
			data:= mapinfo.Bytes()
			size := len(data)

			payload := make([]byte, size)
			copy(payload, data)
			
			stats := server.quicConnection.ConnectionStats()
			
			// Throttled: this callback fires once per RTP packet (dozens
			// to hundreds per second), but the underlying ConnectionStats
			// barely changes packet-to-packet -- printing it that often
			// just floods the log without adding information.
			if time.Since(server.lastQuicStatsLogAt) >= time.Second {
				logger.Printf("[QUIC Stats] MinRTT=%v LatestRTT=%v SmoothedRTT=%v MeanDeviation=%v | BytesSent=%d PacketsSent=%d BytesReceived=%d PacketsReceived=%d BytesLost=%d PacketsLost=%d\n",
					stats.MinRTT, stats.LatestRTT, stats.SmoothedRTT, stats.MeanDeviation,
					stats.BytesSent, stats.PacketsSent, stats.BytesReceived, stats.PacketsReceived,
					stats.BytesLost, stats.PacketsLost)
				server.lastQuicStatsLogAt = time.Now()
			}
				
			err := server.quicConnection.SendDatagram(payload)
			
			if err != nil{
				logger.Println("[Server Log] Failed to send Datagrams.")
				stopStream()
				return gst.FlowError
			}
			
			return gst.FlowOK
		},
	})
	
	server.pipeline.SetState(gst.StatePlaying)
	mainLoop.Run()
	
	server.CloseServer()
	
	return nil
}

/*
 * Function for receiving RTCP feedback from the Client
 * Retuned to isolate the trailing 16-byte tracking packet safely
 */
func (server *Server) ReceiveFeedback(ctx context.Context){
	for{
		rawAppPacket, err := server.quicConnection.ReceiveDatagram(ctx)
			
		if err != nil{
			if errors.Is(err, context.Canceled) {
				return 
			}
			logger.Printf("[Server Log] Error receiving RTCP feedback: %v\n", err)
			return
		}
			
		if len(rawAppPacket) < 8 {
			continue
		}
			
		var appPacket rtcp.ApplicationDefined
		err = appPacket.Unmarshal(rawAppPacket)
			
		if err != nil {
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


func (server *Server) ABR(B_current uint32){
	if time.Since(server.connectionStartTime) >= 10 * time.Second{
		server.isStartup = false
	}		
	
	quicMetrics := server.metrics.Load()
	T_raw := quicMetrics.cwndBps
	T_safe := ComputeSafeThroughput(T_raw)
	R_target := ComputeTier(T_safe)
	R_next := server.R_current
	
	if time.Since(server.lastQuicStatsLogAt) >= time.Second {
		logger.Printf("[QUIC Stats] Tsafe = %.2f, cwndBps = %.2f, inFlightBps = %.2f, smoothedRTT = %s\n", ComputeSafeThroughput(quicMetrics.cwndBps), quicMetrics.cwndBps, quicMetrics.inFlightBps, quicMetrics.smoothedRTT)
		server.lastQuicStatsLogAt = time.Now()
	}
	
	if R_target > server.R_current{
		server.stableSince = time.Time{}
		// SCENARIO A
		if server.isStartup{
			R_next = R_target
		} else{
			
			if !server.isStabilityTimerActive{
				// if no timer active, start one
				server.stabilityTimer = time.Now()
				server.isStabilityTimerActive = true
				R_next = server.R_current
			} else if time.Since(server.stabilityTimer) >= 5 * time.Second{
				// else if there's a timer and it's >= 5.0 seconds elapsed
				R_next = R_target
				server.isStabilityTimerActive = false
			} else{
				// timer is active, but has not reached ~5.0 seconds
				R_next = server.R_current
			}
		}
	} else if R_target < server.R_current{
		server.stableSince = time.Time{}
		// SCENARIO B
		if B_current > 1000{
			R_next = server.R_current
		} else{
			R_next = server.R_current - 1
			if R_next < low {
				R_next = low
			}
			server.isStartup = false
		}
	} else{
		// SCENARIO C
		if server.isStabilityTimerActive{
			server.isStabilityTimerActive = false
		} 
		
		R_next = server.R_current
		
		if server.stableSince.IsZero() {
			server.stableSince = time.Now()
		}
		isStable := time.Since(server.stableSince) >= 3*time.Second
		
		if server.R_current == high || isStable{
			server.isStartup = false
		}
	}
	
	logger.Printf("\n[ABR] Adjusted Video Quality to %s\n\n", quality[R_next].quality)
	server.AdjustVideoQuality(R_next)
	server.R_current = R_next 
}


/*
 * Function for cleaning the Server resources
 */
func (server *Server) CloseServer() {
	if server.pipeline != nil {
		server.pipeline.SetState(gst.StateNull)
	}
	
	if server.quicConnection != nil {
		server.quicConnection.CloseWithError(0, "[Status] QUIC connection closed.")
	}
	if logFile != nil {
		logFile.Close()
	}
}

func main() {
	gst.Init(nil)

	InitLogger("server_abr.log")
	
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
	logger.Println("[Status] Server Created")
	
	server.InitializePipeline(vfilepath)
	
	mainLoop := glib.NewMainLoop(nil, false)
	err := server.StartStream(mainLoop)
	
	if err != nil {
		logger.Printf("[Error] Error streaming video %v\n", err)
		server.CloseServer()
		os.Exit(1)
	}
	
	time.Sleep(2 * time.Second)
	server.CloseServer()
}
