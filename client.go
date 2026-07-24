package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
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
			"t. ! queue name=buffer max-size-time=10000000000 max-size-buffers=0 max-size-bytes=0 ! fakesink sync=true ts-offset=3000000000", outputFile,
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
		
		ssrc := binary.BigEndian.Uint32(buffer[8:12])
		sequenceNumber := binary.BigEndian.Uint16(buffer[2:4])
		ceBits := uint8(0)
		mark := (buffer[1] & 0x80) != 0

		now := toNTP(time.Now())

		// this Receive method needs to be called every time an rtp packet arrives
		client.screamRx.Receive(now, ssrc, size, sequenceNumber, ceBits, mark)
		
		// if its a feedback, send feedback to the server on rtp over the media quic Connection
		if client.screamRx.IsFeedback(now){
				feedback, flag := client.screamRx.CreateStandardizedFeedback(now, mark) // feedback is an RTCP CC feedback packet with SCReAM metrics

				if flag{
					// create a receiver report packet, as type CompoundPacket needs to be prefixed by either an RR/SR
					rr := &rtcp.ReceiverReport{
						SSRC: ssrc,
					}

					rawRR, err := rr.Marshal()
					
					if err != nil {
						fmt.Printf("[SCReAM Log] Error marshaling RR: %v\n\n", err)
						continue
					}

					// it also states that the RR must be followed by an SDES packet
					sdes := rtcp.NewCNAMESourceDescription(ssrc, "client")
									
					rawSdes, err := sdes.Marshal()
					
					if err != nil {
						fmt.Printf("[SCReAM Log] Error marshaling SDES: %v\n\n", err)
						continue
					}
					
					// get the client buffer time in milliseconds
					clientBuffer := make([]byte, 8)
					
					// marshall (big endian) it for network transport
					binary.BigEndian.PutUint64(clientBuffer, client.GetClientBuffer())
					
					// create an RTCP APP (application defined packet) packet
					appPacket := rtcp.ApplicationDefined{
						SubType: uint8(0),
						SSRC:	 ssrc,
						Name:	 "BUFF",
						Data: 	 clientBuffer,
					}
					
					rawAppPacket, err := appPacket.Marshal()
					if err != nil {
						fmt.Printf("[SCReAM Log] Error marshaling app packet: %v\n\n", err)
						continue
					}

					// append all of them in order
					var compoundPacket []byte
					compoundPacket = append(compoundPacket, rawRR...)
					compoundPacket = append(compoundPacket, rawSdes...)
					compoundPacket = append(compoundPacket, feedback...)
					compoundPacket = append(compoundPacket, rawAppPacket...)
					
					if err != nil{
						fmt.Printf("\n[SCReAM Log] Error creating compound packet: %v\n", err)
					}
					
					err = client.quicConnection.SendDatagram(compoundPacket) //send the compound packet back to the server
					
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
