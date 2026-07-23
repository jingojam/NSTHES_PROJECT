package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

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

// 32-bit RTCP APP Packet `name` field = "BUFF" (big-endian)
var appPacketName uint32 = (uint32('B') << 24) | 
						   (uint32('U') << 16) | 
						   (uint32('F') << 8)  | 
							uint32('F')

/*
 * RTCP APP Packet struct fields
 * as per https://datatracker.ietf.org/doc/html/rfc3550#section-6.7
 */
type RTCP_APP_Packet struct{
	version uint8   // 2 bits
	padding uint8   // 1 bit
	subtype uint8   // 5 bits
	pt uint8  		// 8 bits   (1 byte)
	length uint16   // 16 bits  (2 bytes)
	ssrc uint32     // 32 bits  (4 bytes)
	name uint32     // 32 bits  (4 bytes)
	data []uint32   // variable (n bytes)
}

/*
 * Function to initialize an RTCP APP Packet struct,
 * Returns an RTCP APP packet
 */
func CreateRTCP_APP_Packet(length uint16, ssrc uint32, name uint32, data []uint32) *RTCP_APP_Packet{
	return &RTCP_APP_Packet{
		version:	2,
		padding:	0,
		subtype:	0,
		pt:			204,
		length:		length,
		ssrc:		ssrc,
		name:		name,
		data:		data,
	}
}

/*
 * Function for marshalling (converting structure into network-transmittable format)
 * an RTCP APP Packet in big-endian order
 * Returns RTCP APP packet in []byte array
 */
func (appPacket *RTCP_APP_Packet) Marshal() []byte{
	totalBytes := 12 + (len(appPacket.data) * 4)
	packet := make([]byte, totalBytes)
	
	// each `packet` index is 1 byte (8 bits) wide
	packet[0] = ((appPacket.version & 0x03) << 6) | ((appPacket.padding & 0x01) << 5) | (appPacket.subtype & 0x1f) // couple all 3 fields into one byte
	
	packet[1] = appPacket.pt & 0xff
	
	// length field is 16-bits (2 bytes) in size
	packet[2] = byte((appPacket.length >> 8) & 0xff)
	packet[3] = byte((appPacket.length) & 0xff)
	
	// ssrc field is 32-bits (4 bytes) in size
	packet[4] = byte((appPacket.ssrc >> 24) & 0xff)
	packet[5] = byte((appPacket.ssrc >> 16) & 0xff)
	packet[6] = byte((appPacket.ssrc >> 8) & 0xff)
	packet[7] = byte((appPacket.ssrc) & 0xff)
	
	// name field is 32-bits (4 bytes) in size
	packet[8]  = byte((appPacket.name >> 24) & 0xff)
	packet[9]  = byte((appPacket.name >> 16) & 0xff)
	packet[10] = byte((appPacket.name >> 8) & 0xff)
	packet[11] = byte((appPacket.name) & 0x0ff)
	
	// data field is variable-length
	for i, value := range appPacket.data{
		index := 12 + (i * 4) // index per block of 4 bytes starting from index 12...onwards
		packet[index + 0]  = byte((value >> 24) & 0xff)
		packet[index + 1]  = byte((value >> 16) & 0xff)
		packet[index + 2] = byte((value >> 8) & 0xff)
		packet[index + 3] = byte(value & 0x0ff)
	}
	
	return packet
}

/*
 * RoQ Client struct
 */
type Client struct {
	/*Network attributes*/
	ip             string
	port           uint64
	quicConnection quic.Connection
	receiveFlow    *roq.ReceiveFlow
	
	/*SCReAM congestion controller (feedback)*/
	screamRx *scream.Rx

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
		receiveFlow:    nil,
		screamRx:		scream.NewRx(12345678), // ssrc is 12345678 by default to match server-peer ssrc
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
			"t. ! queue name=buffer max-size-time=10000000000 max-size-buffers=1000 max-size-bytes=0 ! fakesink sync=true ts-offset=3000000000", outputFile,
	)

	pipeline, err := gst.NewPipelineFromString(pipelineStr)
	if err != nil {
		fmt.Printf("[Error] Failed to parse pipeline launch string: %v\n", err)
		return
	}

	client.pipeline = pipeline

	srcElement, err := client.pipeline.GetElementByName("src")
	if err != nil {
		fmt.Printf("[Error] Failed to find src element: %v\n", err)
		return
	}
	client.source = srcElement
	
	// Targets the branch buffer right before the fakesink layout
	bufferElement, err := client.pipeline.GetElementByName("buffer")
	if err != nil {
		fmt.Printf("[Error] Failed to find queue element: %v\n", err)
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
func (client *Client) RunClient(outputFile string) error {
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

	roqConn := roq.NewQUICGoConnection(conn)
	
	session, err := roq.NewSession(roqConn, true, nil)
	if err != nil {
		return fmt.Errorf("failed to create RoQ client session: %w", err)
	}

	flow, err := session.NewReceiveFlow(flowID)
	if err != nil {
		return fmt.Errorf("failed to create receive flow: %w", err)
	}
	client.receiveFlow = flow

	mainLoop := glib.NewMainLoop(nil, false)

	// Spin up goroutine to feed incoming QUIC packets to GStreamer appsrc
	go client.ReceivePackets(mainLoop)

	client.pipeline.SetState(gst.StatePlaying)

	fmt.Printf("[Status] Client connected. Receiving stream and saving raw TS data to %s...\n", outputFile)
	mainLoop.Run()

	fmt.Printf("[Status] Streaming finished\n")
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
    s := (float64(t.UnixNano()) / 1000000000) + 2208988800
    return uint32(s * 65536)
}

/*
 * Function to get client buffer time in nanoseconds
 * Returns client buffer time in uint64 (converted to Milliseconds)
 */
func (client *Client) GetClientBuffer() uint64{
	// current-level-time property of the queue element returns the buffer time in nanoseconds
	duration, err := client.buffer.GetProperty("current-level-time")
	
	if err != nil{
		fmt.Printf("[Error] Error fetching client buffer: %s\n", err)
		return 0
	}
	
	nanoseconds, ok := duration.(uint64)
	
	if !ok{
		fmt.Printf("[Error] Buffer property type mismatch\n")
		return 0	
	}
	
	// Real-time jitter metrics scale accurately inside millisecond boundaries.
	bufferMS := nanoseconds / 1000000
	fmt.Printf("Client BUFFER: %d ms\n", bufferMS)
	
	return bufferMS
}

/*
 * Function for receiving QUIC packets from the server peer 
 *  and sending RTCP feedback to the Server
 *
 * Parameters:
 *	- mainLoop glib event loop manager 
 */
func (client *Client) ReceivePackets(mainLoop *glib.MainLoop){
	buffer := make([]byte, 65536)
	appsource := app.SrcFromElement(client.source)

	for {
		size, err := client.receiveFlow.Read(buffer)
		
		if err != nil {
			fmt.Printf("\n[Client Log] Server finished transmission. Wrapping up video file...\n")
			break
		}

		if size == 0 {
			continue
		}

		bytes := make([]byte, size)
		copy(bytes, buffer[:size])
		
		ssrc := uint32(buffer[8])<<24 | uint32(buffer[9])<<16 | uint32(buffer[10])<<8 | uint32(buffer[11])
		sequenceNumber := uint16(buffer[2])<<8 | uint16(buffer[3])
		ceBits := uint8(0)
		mark := (buffer[1] & 0x80) != 0

		now := toNTP(time.Now())

		// this Receive method needs to be called every time an rtp packet arrives
		client.screamRx.Receive(now, ssrc, size, sequenceNumber, ceBits, mark)
		
		// if its a feedback, send feedback to the server on rtp over the media quic Connection
		if client.screamRx.IsFeedback(now){
				feedback, flag := client.screamRx.CreateStandardizedFeedback(now, mark) // feedback is an RTCP CC feedback packet with SCReAM metrics

				if flag{
					clientBuffer := []uint32{uint32(client.GetClientBuffer())} // get the client buffer time in milliseconds
					appPacket := CreateRTCP_APP_Packet(uint16(3 + len(clientBuffer)) - 1, 12345678, appPacketName, clientBuffer)// create an APP packet with the client buffer time as data
					compoundPacket := append(feedback, appPacket.Marshal()...) // create an RTCP Compound packet (2+ RTCP packets concatenated)

					err := client.quicConnection.SendDatagram(compoundPacket) //send the compound packet back to the server
					
					if err != nil{
						fmt.Printf("\n[SCReAM Log] Error sending RTCP feedback: %v\n", err)
					}
				}
		}

		gstbuf := gst.NewBufferFromBytes(bytes)
		ret := appsource.PushBuffer(gstbuf)

		if ret != gst.FlowOK {
			break
		}
	}

	appsource.EndStream()
	time.Sleep(2 * time.Second) // Let the pipeline drain cleanly
	mainLoop.Quit()
}

/*
 * Function for cleaning resources of the client
 */
func (client *Client) CloseClient() {
	if client.pipeline != nil {
		client.pipeline.SetState(gst.StateNull)
	}
	if client.receiveFlow != nil {
		client.receiveFlow.Close()
	}
	if client.quicConnection != nil {
		client.quicConnection.CloseWithError(0, "QUIC connection closed.")
	}
}

func main() {
	gst.Init(nil)
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

	client := CreateClient(ip, port)
	defer client.CloseClient()

	err := client.RunClient(outputFile)
	if err != nil {
		fmt.Printf("[Error] Client failed: %v\n", err)
		os.Exit(1)
	}
}
