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
	scream "github.com/mengelbart/scream-go"
	"github.com/quic-go/quic-go"
)

const (
	mediaFlowID      uint64        = 1
	streamSSRC       uint32        = 12345678
	feedbackInterval time.Duration = 50 * time.Millisecond
)

// 32-bit RTCP APP Packet `name` field = "BUFF" (big-endian)
var appPacketName uint32 = (uint32('B') << 24) |
	(uint32('U') << 16) |
	(uint32('F') << 8) |
	uint32('F')

/*
 * RTCP APP Packet struct (RFC 3550 Section 6.7)
 */
type RTCP_APP_Packet struct {
	version uint8    // 2 bits
	padding uint8    // 1 bit
	subtype uint8    // 5 bits
	pt      uint8    // 8 bits   (1 byte: 204 for APP)
	length  uint16   // 16 bits  (2 bytes)
	ssrc    uint32   // 32 bits  (4 bytes)
	name    uint32   // 32 bits  (4 bytes)
	data    []uint32 // variable (n bytes)
}

func CreateRTCP_APP_Packet(length uint16, ssrc uint32, name uint32, data []uint32) *RTCP_APP_Packet {
	return &RTCP_APP_Packet{
		version: 2,
		padding: 0,
		subtype: 0,
		pt:      204,
		length:  length,
		ssrc:    ssrc,
		name:    name,
		data:    data,
	}
}

func (appPacket *RTCP_APP_Packet) Marshal() []byte {
	totalBytes := 12 + (len(appPacket.data) * 4)
	packet := make([]byte, totalBytes)

	packet[0] = ((appPacket.version & 0x03) << 6) | ((appPacket.padding & 0x01) << 5) | (appPacket.subtype & 0x1f)
	packet[1] = appPacket.pt & 0xff

	packet[2] = byte((appPacket.length >> 8) & 0xff)
	packet[3] = byte((appPacket.length) & 0xff)

	packet[4] = byte((appPacket.ssrc >> 24) & 0xff)
	packet[5] = byte((appPacket.ssrc >> 16) & 0xff)
	packet[6] = byte((appPacket.ssrc >> 8) & 0xff)
	packet[7] = byte((appPacket.ssrc) & 0xff)

	packet[8] = byte((appPacket.name >> 24) & 0xff)
	packet[9] = byte((appPacket.name >> 16) & 0xff)
	packet[10] = byte((appPacket.name >> 8) & 0xff)
	packet[11] = byte((appPacket.name) & 0xff)

	for i, value := range appPacket.data {
		index := 12 + (i * 4)
		packet[index+0] = byte((value >> 24) & 0xff)
		packet[index+1] = byte((value >> 16) & 0xff)
		packet[index+2] = byte((value >> 8) & 0xff)
		packet[index+3] = byte(value & 0xff)
	}

	return packet
}

type Client struct {
	ip             string
	port           uint64
	quicConnection quic.Connection
	receiveFlow    *roq.ReceiveFlow

	screamRx *scream.Rx

	pipeline     *gst.Pipeline
	source       *gst.Element
	playoutQueue *gst.Element
}

func CreateClient(ip string, port uint64) *Client {
	return &Client{
		ip:       ip,
		port:     port,
		screamRx: scream.NewRx(streamSSRC),
	}
}

func (client *Client) InitializePipeline(outputFile string) {
	// Replaced jitterbuffer with a clean tee architecture:
	// - appsrc assigns live timestamps (is-live=true do-timestamp=true).
	// - tee splits the stream BEFORE mpegtsmux.
	// - Branch 1 (filesink): multiplexes to MPEG-TS and saves to disk.
	// - Branch 2 (playout_queue): acts as the dynamic buffer we measure, held back 
	//   by a fakesink with a 3-second offset.
	pipelineStr := fmt.Sprintf(
		`appsrc name=src is-live=true do-timestamp=true format=time caps="application/x-rtp, media=video, clock-rate=90000, encoding-name=H264" ! `+
			`rtph264depay ! h264parse ! `+
			`tee name=t `+
			`t. ! queue ! mpegtsmux ! filesink location="%s" `+
			`t. ! queue name=playout_queue max-size-time=10000000000 max-size-bytes=0 max-size-buffers=0 ! `+
			`fakesink sync=true ts-offset=3000000000`,
		outputFile,
	)

	pipeline, err := gst.NewPipelineFromString(pipelineStr)
	if err != nil {
		fmt.Printf("[Error] Failed to parse pipeline launch string: %v\n", err)
		return
	}

	client.pipeline = pipeline
	client.source, _ = client.pipeline.GetElementByName("src")
	client.playoutQueue, _ = client.pipeline.GetElementByName("playout_queue")
}

func (client *Client) RunClient(outputFile string) error {
	client.InitializePipeline(outputFile)

	tlsConf := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"roq-10"},
	}

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

	flow, err := session.NewReceiveFlow(mediaFlowID)
	if err != nil {
		return fmt.Errorf("failed to create receive flow: %w", err)
	}
	client.receiveFlow = flow

	mainLoop := glib.NewMainLoop(nil, false)

	go client.ReceivePackets(mainLoop)
	go client.feedbackLoop()

	client.pipeline.SetState(gst.StatePlaying)

	fmt.Printf("[Status] Client connected. Receiving stream and saving raw TS data to %s...\n", outputFile)
	mainLoop.Run()

	fmt.Printf("[Status] Streaming finished\n")
	return nil
}

// Replaced toCompactNTP with toNTP to fix the RTT calculation mismatch
func toNTP(t time.Time) uint32 {
	s := (float64(t.UnixNano()) / 1000000000) + 2208988800
	return uint32(s * 65536)
}

// Retrieves the current depth of the playout queue in milliseconds
func (client *Client) getBufferLevelMs() uint32 {
	if client.playoutQueue == nil {
		return 0
	}

	val, err := client.playoutQueue.GetProperty("current-level-time")
	if err != nil {
		return 0
	}

	var levelNs uint64
	switch v := val.(type) {
	case uint64:
		levelNs = v
	default:
		return 0
	}

	return uint32(levelNs / 1_000_000)
}

func (client *Client) ReceivePackets(mainLoop *glib.MainLoop) {
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

		// Ensure we extract the SSRC properly from a standard 12-byte RTP header
		if len(bytes) >= 12 {
			ssrc := uint32(bytes[8])<<24 | uint32(bytes[9])<<16 | uint32(bytes[10])<<8 | uint32(bytes[11])
			sequenceNumber := uint16(bytes[2])<<8 | uint16(bytes[3])
			mark := (bytes[1] & 0x80) != 0

			client.screamRx.Receive(toNTP(time.Now()), ssrc, size, sequenceNumber, 0, mark)
		}

		gstbuf := gst.NewBufferFromBytes(bytes)
		ret := appsource.PushBuffer(gstbuf)

		if ret != gst.FlowOK {
			break
		}
	}

	appsource.EndStream()
	time.Sleep(2 * time.Second)
	mainLoop.Quit()
}

func (client *Client) feedbackLoop() {
	ticker := time.NewTicker(feedbackInterval)
	defer ticker.Stop()

	for range ticker.C {
		nowNTP := toNTP(time.Now())
		if !client.screamRx.IsFeedback(nowNTP) {
			continue
		}

		feedbackBuf, ok := client.screamRx.CreateStandardizedFeedback(nowNTP, false)
		if !ok || len(feedbackBuf) == 0 {
			continue
		}

		// Dynamically read buffer occupancy from playoutQueue
		bufferMs := client.getBufferLevelMs()

		// Construct the correctly formatted RTCP APP packet
		appPacket := CreateRTCP_APP_Packet(
			uint16(3+1)-1,
			streamSSRC,
			appPacketName,
			[]uint32{bufferMs},
		)

		compoundPacket := append(feedbackBuf, appPacket.Marshal()...)

		if err := client.quicConnection.SendDatagram(compoundPacket); err != nil {
			fmt.Printf("[Client Log] failed to send SCReAM feedback: %v\n", err)
		}
	}
}

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
