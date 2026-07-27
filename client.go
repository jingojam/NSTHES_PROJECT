package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"encoding/binary"
	"errors"

	"github.com/go-gst/go-glib/glib"
	"github.com/go-gst/go-gst/gst"
	"github.com/go-gst/go-gst/gst/app"
	"github.com/quic-go/quic-go"
	"github.com/pion/rtcp"
)

// Flow ID constant = 1
var flowID uint64 = 1

// feedbackInterval controls how often SendFeedback actually fires. This
// replaces the previous "one goroutine + one datagram per received RTP
// packet" behavior, which sent feedback (and drove the server's ABR())
// dozens of times a second with no interval control at all -- flooding
// both sides' logs and the network with far more feedback traffic than
// useful. 50ms matches the cadence the server side already expects.
const feedbackInterval = 500 * time.Millisecond

// Global logger: dual-writes to stdout AND a log file, mirroring
// server.go's InitLogger, so client-side behavior is just as reviewable
// after the fact instead of only living in scrollback.
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
 * RoQ Client struct
 */
type Client struct {
	/*Network attributes*/
	ip             string
	port           uint64
	
	quicConnection *quic.Conn

	/*Feedback loop state -- written by ReceivePackets (every incoming
	  packet), read by feedbackLoop's ticker. atomic since they run on
	  different goroutines and this is just a single uint32 + a flag.*/
	latestSSRC atomic.Uint32
	haveSSRC   atomic.Bool

	/*Log throttling for the buffer-level print -- see GetClientBuffer.*/
	lastBufferLogAt time.Time

	/*Gstreamer elements*/
	pipeline     *gst.Pipeline
	source       *gst.Element
	buffer 		 *gst.Element
	depayloader  *gst.Element
	parser       *gst.Element
	decoder      *gst.Element
	videoconvert *gst.Element
	sink         *gst.Element
}

/*
 * Function for initializing an RoQ client/receiver
 *
 * Parameters:
 * 	- ip address string
 *  - port number
 *
 * Returns a `Client` struct
 */
func CreateClient(ip string, port uint64) *Client {
	return &Client{
		ip:             ip,
		port:           port,
		quicConnection: nil,
		pipeline:       nil,
		source:         nil,
		buffer:			nil,
		depayloader:    nil,
		parser:         nil,
		decoder:        nil,
		videoconvert:   nil,
		sink:           nil,
	}
}

/*
 * Function for initializing the Gstreamer pipeline
 *
 * Parameters:
 *  - outputFile string for filesink
 */
func (client *Client) InitializePipeline(outputFile string) {
	// FIX: Aligned with thesis document to support a 3-second (3000ms) maximum buffer cushion.
	// ts-offset=3000000000 delays rendering by exactly 3 seconds, storing all frames in our tracker queue.
	pipelineStr := fmt.Sprintf(
		"appsrc name=src is-live=true do-timestamp=false format=time caps=\"application/x-rtp, media=video, clock-rate=90000, encoding-name=H264\" ! "+
			"rtph264depay ! h264parse ! mpegtsmux ! "+
			"tee name=t "+
			"t. ! queue ! filesink location=\"%s\" "+
			"t. ! queue name=buffer max-size-time=10000000000 max-size-buffers=0 max-size-bytes=0 ! fakesink sync=true ts-offset=3000000000", outputFile,
	)

	pipeline, err := gst.NewPipelineFromString(pipelineStr)
	if err != nil {
		logger.Printf("[Error] Failed to parse pipeline launch string: %v\n", err)
		return
	}

	client.pipeline = pipeline

	srcElement, err := client.pipeline.GetElementByName("src")
	if err != nil {
		logger.Printf("[Error] Failed to find src element: %v\n", err)
		return
	}
	client.source = srcElement
	
	// Targets the branch buffer right before the fakesink layout
	bufferElement, err := client.pipeline.GetElementByName("buffer")
	if err != nil {
		logger.Printf("[Error] Failed to find queue element: %v\n", err)
		return
	}
	
	client.buffer = bufferElement
}

/*
 * Function to start the client QUIC connection
 *
 * Parameters:
 *	- outputFile string for filesink
 *
 * Returns an error if any, otherwise NULL
 */
func (client *Client) RunClient(mainLoop *glib.MainLoop, outputFile string) error {
	client.InitializePipeline(outputFile)

	tlsConf := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"roq-10"},
	}

	// We must explicitly enable datagrams at the underlying QUIC transport layer!
	quicConf := &quic.Config{
		EnableDatagrams: true,
	}

	conn, err := quic.DialAddr(context.Background(), fmt.Sprintf("%s:%d", client.ip, client.port), tlsConf, quicConf)
	if err != nil {
		return fmt.Errorf("failed to dial QUIC address: %w", err)
	}
	
	client.quicConnection = conn
	
	streamCtx, stopStream := context.WithCancel(context.Background())
	defer stopStream()
	
	// Spin up goroutine to feed incoming QUIC packets to GStreamer appsrc
	go client.ReceivePackets(streamCtx, stopStream)
	go client.feedbackLoop(streamCtx)

	go func() {
		<-streamCtx.Done()
		glib.IdleAdd(func() bool{
			mainLoop.Quit()
			return false
		})
	}()

	client.pipeline.SetState(gst.StatePlaying)

	logger.Printf("[Status] Client connected. Receiving stream and saving raw TS data to %s...\n", outputFile)
	mainLoop.Run()
	client.CloseClient()
	logger.Printf("[Status] Streaming finished\n")
	return nil
}

/*
 * Helper function implementation from the SCReAM Go wrapper
 * Converts time to NTP
 *
 * Parameters:
 *	- t, which is a Time struct
 *
 * Returns time in uint32
 */
func toNTP(t time.Time) uint32{
	// seconds since 1st January 1900
	s := (float64(t.UnixNano()) / 1000000000.0) + 2208988800

	integerPart := uint32(s)
	fractionalPart := uint32((s - float64(integerPart)) * 0xFFFFFFFF)

	// higher 32 bits are the integer part, lower 32 bits are the fractional part
	return uint32(((uint64(integerPart)<<32 | uint64(fractionalPart)) >> 16) & 0xFFFFFFFF)
}

/*
 * Function to get client buffer time in nanoseconds
 * Returns client buffer time in uint64 (converted to Milliseconds)
 */
func (client *Client) GetClientBuffer() uint64{
	// current-level-time property of the queue element returns the buffer time in nanoseconds
	duration, err := client.buffer.GetProperty("current-level-time")
	
	if err != nil{
		logger.Printf("[Error] Error fetching client buffer: %s\n", err)
		return 0
	}
	
	nanoseconds, ok := duration.(uint64)
	
	if !ok{
		logger.Printf("[Error] Buffer property type mismatch\n")
		return 0	
	}
	
	// Real-time jitter metrics scale accurately inside millisecond boundaries.
	bufferMS := nanoseconds / 1000000

	// Throttled: this is called once per feedbackLoop tick (every 50ms =
	// 20x/sec). The actual feedback send stays at that cadence -- only
	// the print is throttled further, so the log stays readable without
	// changing what data reaches the server.
	if time.Since(client.lastBufferLogAt) >= 500*time.Millisecond {
		logger.Printf("Client BUFFER: %d ms\n", bufferMS)
		client.lastBufferLogAt = time.Now()
	}
	
	return bufferMS
}

/*
 * Function for receiving QUIC packets from the server peer 
 *  and sending RTCP feedback to the Server
 *
 * Parameters:
 *	- mainLoop glib event loop manager 
 */
func (client *Client) ReceivePackets(ctx context.Context, stop context.CancelFunc){
	appsource := app.SrcFromElement(client.source)

	for {
		rtpPacket, err := client.quicConnection.ReceiveDatagram(ctx)
		size := len(rtpPacket)

		if err != nil {
			logger.Printf("\n[Client Log] Server finished transmission. Wrapping up video file...\n")
			if errors.Is(err, context.Canceled) {
				return 
			}
			break
		}

		if len(rtpPacket) < 12 {
			continue
		}
		
		bytes := make([]byte, size)
		copy(bytes, rtpPacket[:size])

		ssrc := binary.BigEndian.Uint32(rtpPacket[8:12])
		// Record the latest SSRC for feedbackLoop's ticker to use --
		// FIX: this used to be "go client.SendFeedback(ssrc)" fired on
		// EVERY packet (dozens/sec, no interval control), spawning a
		// goroutine and a QUIC datagram send per packet and driving the
		// server's ABR() at the same uncontrolled rate. Recording the
		// value here is essentially free; the actual send now happens on
		// feedbackLoop's fixed 50ms ticker instead.
		client.latestSSRC.Store(ssrc)
		client.haveSSRC.Store(true)

		gstbuf := gst.NewBufferFromBytes(bytes)
		ret := appsource.PushBuffer(gstbuf)

		if ret != gst.FlowOK {
			break
		}
	}
	stop()
	appsource.EndStream()
	time.Sleep(2 * time.Second) // Let the pipeline drain cleanly
}

// feedbackLoop sends SCReAM-style buffer-level feedback back to the
// server on a fixed cadence, instead of once per received media packet.
func (client *Client) feedbackLoop(ctx context.Context) {
	ticker := time.NewTicker(feedbackInterval)
	defer ticker.Stop()

	for{
		select{
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !client.haveSSRC.Load(){
					continue
				}
				client.SendFeedback(client.latestSSRC.Load())
		}
	}
}

func (client *Client) SendFeedback(ssrc uint32){
	// no pacing yet
	// get the client buffer time in milliseconds
	clientBuffer := make([]byte, 8)
					
	// marshall (big endian) it for network transport
	binary.BigEndian.PutUint64(clientBuffer, client.GetClientBuffer())

	// create an RTCP APP (application defined packet) packet
	appPacket := rtcp.ApplicationDefined{
		SubType: uint8(0),
		SSRC:	 ssrc,
		Name:	 "BUFR",
		Data: 	 clientBuffer,
	}

	rawAppPacket, err := appPacket.Marshal()
		
	if err != nil {
	logger.Printf("[Client Log] Error marshaling app packet: %v\n\n", err)
		return
	}

	err = client.quicConnection.SendDatagram(rawAppPacket) //send RTCP application defined packet back to the server

	if err != nil{
		logger.Printf("\n[Client Log] Error sending RTCP feedback: %v\n", err)
	}
}

/*
 * Function for cleaning resources of the client
 */
func (client *Client) CloseClient() {
	if client.pipeline != nil {
		client.pipeline.SetState(gst.StateNull)
	}

	if client.quicConnection != nil {
		client.quicConnection.CloseWithError(0, "QUIC connection closed.")
	}
	if logFile != nil {
		logFile.Close()
	}
}

func main() {
	gst.Init(nil)
	InitLogger("client_abr.log")
	addr := "127.0.0.1:5000"
	outputFile := "received_output.ts"

	if len(os.Args) > 2 {
		addr = os.Args[1]
		outputFile = os.Args[2]
	} else if len(os.Args) > 1 {
		addr = os.Args[1]
	}

	address := strings.Split(addr, ":")
	ip := address[0]
	port, _ := strconv.ParseUint(address[1], 10, 64)

	mainLoop := glib.NewMainLoop(nil, false)
	client := CreateClient(ip, port)
	
	defer client.CloseClient()

	err := client.RunClient(mainLoop, outputFile)
	if err != nil {
		logger.Printf("[Error] Client failed: %v\n", err)
		os.Exit(1)
	}
}
