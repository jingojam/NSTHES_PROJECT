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
	"sync"
	"time"

	"github.com/go-gst/go-glib/glib"
	"github.com/go-gst/go-gst/gst"
	"github.com/go-gst/go-gst/gst/app"
	"github.com/mengelbart/roq"
	scream "github.com/mengelbart/scream-go"
	"github.com/quic-go/quic-go"
)

/*
 * RTCP Packet RFCs:
 * - RTCP APP packet: https://datatracker.ietf.org/doc/html/rfc3550#section-6.7
 * - RTCP Compound packet: https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-rtpme/e2e2deb3-2f87-4932-a9c5-1dbde47fb12d
 * - RTCP CC Feedback packet: https://datatracker.ietf.org/doc/html/rfc8888#section-3.1
 */

// Flow ID constant = 1
var flowID uint64 = 1

// Global logger instance
var logger *log.Logger
var logFile *os.File

/*
 * Video quality struct with matching bitrate floors
 */
type BitrateTier struct {
	quality       string
	targetbitrate float64
	minEncoderKbps uint
	width         int
	height        int
}

/*
 * Array of `BitrateTier` structs initialized with H.264 standard bitrates
 */
var quality = []BitrateTier{
	{quality: "480p", targetbitrate: 800000.0, minEncoderKbps: 800, width: 854, height: 480},      // 800 kbps
	{quality: "720p", targetbitrate: 2500000.0, minEncoderKbps: 2500, width: 1280, height: 720},   // 2.5 Mbps
	{quality: "1080p", targetbitrate: 5000000.0, minEncoderKbps: 5000, width: 1920, height: 1080}, // 5.0 Mbps
}

/*
 * Index constants for indexing `quality` Array
 */
var low int = 0  // quality[0] -> 480p
var mid int = 1  // quality[1] -> 720p
var high int = 2 // quality[2] -> 1080p

/*
 * Packet struct for implementing `Packet` interface
 * from the SCReAM Go wrapper
 */
type Packet struct {
	size           int
	sequenceNumber uint16
	timestamp      time.Time
	payload        []byte // Holds actual RTP bytes for the decoupling queue
}

func (packet *Packet) Size() int {
	return packet.size
}

func (packet *Packet) SequenceNumber() uint16 {
	return packet.sequenceNumber
}

func (packet *Packet) Timestamp() time.Time {
	return packet.timestamp
}

/*
 * RoQ Server struct
 */
type Server struct {
	/*thread safety*/
	qPipeLock sync.RWMutex // read/write lock for modifying pipeline element

	/*Network attributes*/
	ip   string
	port uint64

	/*SCReAM congestion controller*/
	screamCC    *scream.Tx
	screamQueue *scream.Queue[*Packet]
	packetChan  chan *Packet // Decoupling queue

	/*ABR variables*/
	isStartup    bool
	Rcurrent     int
	lastSwitch   time.Time // Track pipeline stabilization time
	upgradeTimer *time.Timer
	tsafeHistory []float64 // Rolling window for Moving Average smoothing

	/*QUIC transport attributes*/
	quicConnection quic.Connection
	sendFlow       *roq.SendFlow

	/*GStreamer pipeline elements*/
	pipeline   *gst.Pipeline
	source     *gst.Element
	capsfilter *gst.Element
	encoder    *gst.Element
	parser     *gst.Element
	demux      *gst.Element
	payloader  *gst.Element
	sink       *gst.Element
}

/*
 * Initialize dual-logging to both stdout and file with instant flushing
 */
func InitLogger(logFileName string) {
	var err error
	logFile, err = os.OpenFile(logFileName, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		fmt.Printf("[Error] Failed to open log file %s: %v. Falling back to stdout.\n", logFileName, err)
		logger = log.New(os.Stdout, "", log.LstdFlags|log.Lmicroseconds)
		return
	}

	multiWriter := io.MultiWriter(os.Stdout, logFile)
	logger = log.New(multiWriter, "", log.LstdFlags|log.Lmicroseconds)
	logger.Println("[Status] Logging module initialized successfully.")
}

/*
 * Function for initializing a Server/sender
 */
func CreateServer(ip string, port uint64) *Server {
	return &Server{
		ip:             ip,
		port:           port,
		screamCC:       scream.NewTx(),
		screamQueue:    scream.NewQueue[*Packet](),
		packetChan:     make(chan *Packet, 1000),
		isStartup:      true,
		Rcurrent:       low,
		lastSwitch:     time.Now(),
		tsafeHistory:   make([]float64, 0, 10), // Store up to 10 historical Tsafe samples
		quicConnection: nil,
		sendFlow:       nil,
		pipeline:       nil,
		source:         nil,
		capsfilter:     nil,
		encoder:        nil,
		demux:          nil,
		parser:         nil,
		payloader:      nil,
		sink:           nil,
	}
}

/*
 * Function for initializing GStreamer pipeline elements
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
			`appsink name=sink sync=true max-buffers=1 `, videoFile, quality[low].width, quality[low].height, quality[low].minEncoderKbps,
	)

	var err error

	server.pipeline, err = gst.NewPipelineFromString(pipeline)
	if err != nil {
		logger.Printf("[Error] Failed to create pipeline: %v\n", err)
	}

	server.source, err = server.pipeline.GetElementByName("source")
	if err != nil {
		logger.Printf("[Error] Failed to initialize source element: %v\n", err)
	}

	server.demux, err = server.pipeline.GetElementByName("demux")
	if err != nil {
		logger.Printf("[Error] Failed to initialize demux element: %v\n", err)
	}

	server.parser, err = server.pipeline.GetElementByName("parser")
	if err != nil {
		logger.Printf("[Error] Failed to initialize parser element: %v\n", err)
	}

	server.capsfilter, err = server.pipeline.GetElementByName("filter")
	if err != nil {
		logger.Printf("[Error] Failed to initialize capsfilter element: %v\n", err)
	}

	server.encoder, err = server.pipeline.GetElementByName("encoder")
	if err != nil {
		logger.Printf("[Error] Failed to initialize encoder element: %v\n", err)
	}

	server.payloader, err = server.pipeline.GetElementByName("payloader")
	if err != nil {
		logger.Printf("[Error] Failed to initialize payloader element: %v\n", err)
	}

	server.sink, err = server.pipeline.GetElementByName("sink")
	if err != nil {
		logger.Printf("[Error] Failed to initialize sink element: %v\n", err)
	}
}

/*
 * Function to modify Server pipeline's `capsfilter` element to adjust video resolution
 */
func (server *Server) AdjustVideoQuality(tier int) {
	server.qPipeLock.Lock()
	defer server.qPipeLock.Unlock()

	server.lastSwitch = time.Now()

	caps := gst.NewCapsFromString(fmt.Sprintf("video/x-raw, width=%d, height=%d", quality[tier].width, quality[tier].height))
	err := server.capsfilter.SetProperty("caps", caps)
	if err != nil {
		logger.Printf("[Error] Error adjusting video quality: %v\n", err)
		return
	}

	logger.Printf("[GStreamer] Resolution caps adjusted to: %s (%dx%d)\n",
		quality[tier].quality, quality[tier].width, quality[tier].height)

	if logFile != nil {
		logFile.Sync()
	}
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
	keyPath := cwd + "/server.key"

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

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"roq-10"},
		KeyLogWriter: keylogWriter,
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			return &tls.Config{
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

	logger.Printf("[Status] Accepted connection from %s\n", server.quicConnection.RemoteAddr())
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

	// Initialize SCReAM (Min: 300kbps, Start: 2.5Mbps, Max: 12Mbps)
	server.screamCC.RegisterNewStream(server.screamQueue, 12345678, 1.0, 300000, 2500000, 12000000)
	logger.Println("[Status] Server initialized successfully")
	return nil
}

/*
 * Function for asynchronously draining the packet queue and transmitting over QUIC
 */
func (server *Server) DrainQueue() {
	for pkt := range server.packetChan {
		server.screamCC.IsOkToTransmit(time.Now(), 12345678)

		err := server.sendFlow.WriteRTPBytes(pkt.payload)
		if err != nil {
			logger.Printf("[Server Log] WriteRTPBytes failed: %v\n", err)
			continue
		}

		server.screamQueue.Dequeue()
		mark := (pkt.payload[1] & 0x80) != 0
		server.screamCC.AddTransmitted(time.Now(), 12345678, pkt.size, pkt.sequenceNumber, mark)
	}
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
	go server.DrainQueue()

	mainLoop := glib.NewMainLoop(nil, true)
	appSink := app.SinkFromElement(server.sink)
	appSink.SetCallbacks(&app.SinkCallbacks{
		NewSampleFunc: func(sink *app.Sink) gst.FlowReturn {
			sample := sink.PullSample()
			if sample == nil {
				logger.Println("[Server Log] End of video file reached (EOS). Stopping engine...")
				mainLoop.Quit()
				return gst.FlowEOS
			}

			buffer := sample.GetBuffer()
			if buffer == nil {
				return gst.FlowError
			}

			mapinfo := buffer.Map(gst.MapRead)
			defer buffer.Unmap()

			data := mapinfo.Bytes()
			sequenceNumber := uint16(data[2])<<8 | uint16(data[3])
			size := len(data)
			mark := (data[1] & 0x80) != 0

			pkt := &Packet{
				size:           size,
				sequenceNumber: sequenceNumber,
				timestamp:      time.Now(),
				payload:        append([]byte(nil), data...),
			}

			server.screamCC.NewMediaFrame(time.Now(), 12345678, size, mark)
			server.screamQueue.Enqueue(pkt)

			select {
			case server.packetChan <- pkt:
			default:
				logger.Println("[Server Log] Network queue full, dropping packet to prevent pipeline stall")
				server.screamQueue.Dequeue()
			}

			return gst.FlowOK
		},
	})

	server.pipeline.SetState(gst.StatePlaying)
	mainLoop.Run()
	return nil
}

/*
 * Function for receiving RTCP feedback from the Client
 */
func (server *Server) ReceiveFeedback() {
	for {
		compoundPacket, err := server.quicConnection.ReceiveDatagram(context.Background())
		if err != nil {
			logger.Printf("[SCReAM Log] Error receiving RTCP feedback: %v\n", err)
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

		for i := 0; i < len(rawData)/4; i++ {
			index := i * 4
			clientBuffer[i] = uint32(rawData[index])<<24 |
				uint32(rawData[index+1])<<16 |
				uint32(rawData[index+2])<<8 |
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
func ComputeSafeThroughput(Traw float64) float64 {
	return Traw * 0.8
}

/*
 * Calculates a rolling average over the last 10 Tsafe samples (~1 second of telemetry)
 */
func (server *Server) getSmoothedTsafe(tsafe float64) float64 {
	server.tsafeHistory = append(server.tsafeHistory, tsafe)
	if len(server.tsafeHistory) > 10 {
		server.tsafeHistory = server.tsafeHistory[1:]
	}

	var sum float64
	for _, val := range server.tsafeHistory {
		sum += val
	}
	return sum / float64(len(server.tsafeHistory))
}

/*
 * Practical tier selection thresholds based on safe throughput (Tsafe)
 */
func ComputeTier(q float64) int {
	if q >= 3500000.0 {
		return high // 1080p requires 3.5 Mbps safe throughput
	}
	if q >= 1500000.0 {
		return mid // 720p requires 1.5 Mbps safe throughput
	}
	return low // 480p fallback
}

/*
 * Function for the main ABR Algorithm loop with dynamic encoder bitrate setting
 */
func (server *Server) ABR(clientBuffer uint32) {
	Traw := server.screamCC.GetTargetBitrate(time.Now(), 12345678)

	if Traw != -1.0 {
		Tsafe := ComputeSafeThroughput(Traw)

		// 1. Smooth out instantaneous Tsafe using Moving Average
		smoothedTsafe := server.getSmoothedTsafe(Tsafe)
		Rtarget := ComputeTier(smoothedTsafe)

		// 2. Probing Headroom (1.30x)
		encBitrateKbps := uint((Traw * 1.30) / 1000)

		// 3. ENFORCE TIER MINIMUM BITRATE FLOOR
		// Prevents spatial quality degradation (starvation) at higher resolutions
		minKbpsForCurrentTier := quality[server.Rcurrent].minEncoderKbps
		if encBitrateKbps < minKbpsForCurrentTier {
			encBitrateKbps = minKbpsForCurrentTier
		}

		if encBitrateKbps > 10000 {
			encBitrateKbps = 10000
		}

		server.qPipeLock.Lock()
		if server.encoder != nil {
			_ = server.encoder.SetProperty("bitrate", encBitrateKbps)
		}
		server.qPipeLock.Unlock()

		logger.Printf("[ABR] Bcurrent = %d ms, Rcurrent = %s, Traw = %.2f bps, Tsafe = %.2f bps (Smoothed = %.2f bps) (Encoder @ %d kbps)\n",
			clientBuffer, quality[server.Rcurrent].quality, Traw, Tsafe, smoothedTsafe, encBitrateKbps)

		// Cooldown period after resolution switches (3 seconds)
		isCoolingDown := time.Since(server.lastSwitch) < (3 * time.Second)

		// --------- FAST-START STARTUP PHASE LOGIC ---------
		if server.isStartup {
			if server.Rcurrent == high {
				logger.Println("[ABR] Startup phase concluded: Reached maximum resolution (1080p).")
				server.isStartup = false
			} else if !isCoolingDown {
				if clientBuffer < 300 && time.Since(server.lastSwitch) > 2500*time.Millisecond {
					logger.Printf("[ABR] Startup phase concluded: Low client buffer (%d ms).\n", clientBuffer)
					server.isStartup = false
				} else {
					nextTier := server.Rcurrent + 1
					if Rtarget > nextTier {
						nextTier = Rtarget
					}
					logger.Printf("[ABR] Fast-start optimistic probe: Upgrading resolution tier to %s\n", quality[nextTier].quality)
					server.Rcurrent = nextTier
					server.AdjustVideoQuality(nextTier)
				}
			}
			if logFile != nil {
				logFile.Sync()
			}
			return
		}

		// --------- STEADY-STATE ABR LOGIC ---------
		// SCENARIO A: Upgrade
		if Rtarget > server.Rcurrent {
			if server.upgradeTimer == nil {
				targetTier := Rtarget
				upgrade := func() {
					logger.Printf("[ABR] Elapsed 2.5 seconds: Upgrading resolution tier to %s\n", quality[targetTier].quality)
					server.Rcurrent = targetTier
					server.AdjustVideoQuality(targetTier)
					server.upgradeTimer = nil
				}
				server.upgradeTimer = time.AfterFunc(2500*time.Millisecond, upgrade)
			}
		} else if Rtarget < server.Rcurrent { // SCENARIO B: Downgrade
			if server.upgradeTimer != nil {
				server.upgradeTimer.Stop()
				server.upgradeTimer = nil
			}

			if clientBuffer > 1000 {
				logger.Printf("[ABR] Maintaining %s resolution tier\n", quality[server.Rcurrent].quality)
			} else {
				logger.Printf("[ABR] Buffer depleted (%d ms). Downgrading resolution tier to %s\n", clientBuffer, quality[Rtarget].quality)
				server.Rcurrent = Rtarget
				server.AdjustVideoQuality(Rtarget)
			}
		}

		if logFile != nil {
			logFile.Sync()
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
	err := server.StartStream()
	if err != nil {
		logger.Printf("[Error] Error streaming video: %v\n", err)
		server.CloseServer()
		os.Exit(1)
	}

	time.Sleep(2 * time.Second)
	server.CloseServer()
}
