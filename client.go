package main

import (
	"context"
	"crypto/tls"
	"encoding/binary" // Added for reading RTP Sequence Numbers
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
)

var flowID uint64 = 1

type Client struct {
	ip             string
	port           uint64
	quicConnection quic.Connection
	receiveFlow    *roq.ReceiveFlow

	pipeline     *gst.Pipeline
	source       *gst.Element
	depayloader  *gst.Element
	parser       *gst.Element
	decoder      *gst.Element
	videoconvert *gst.Element
	sink         *gst.Element
}

func CreateClient(ip string, port uint64) *Client {
	return &Client{
		ip:             ip,
		port:           port,
		quicConnection: nil,
		receiveFlow:    nil,
		pipeline:       nil,
		source:         nil,
		depayloader:    nil,
		parser:         nil,
		decoder:        nil,
		videoconvert:   nil,
		sink:           nil,
	}
}

func (client *Client) InitializePipeline(outputFile string) {
	// True Timestamp Preservation with MPEG-TS Muxing
	pipelineStr := fmt.Sprintf(
		"appsrc name=src is-live=true do-timestamp=true format=time caps=\"application/x-rtp, media=video, clock-rate=90000, encoding-name=H264\" ! "+
			"rtph264depay ! h264parse ! mpegtsmux ! filesink location=\"%s\"", outputFile,
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

func (client *Client) ReceivePackets(mainLoop *glib.MainLoop) {
	buffer := make([]byte, 65536)
	appsource := app.SrcFromElement(client.source)

	// --- ADDED: CSV Logging Setup ---
	csvFile, err := os.Create("/tmp/qos_metrics_baseline_roq.csv")
	if err == nil {
		defer csvFile.Close()
		csvFile.WriteString("frame.number,frame.time_relative,frame.time_delta,frame.len,rtp.seq\n")
		fmt.Println("[Log] Writing QoS metrics to /tmp/qos_metrics_baseline_roq.csv")
	} else {
		fmt.Printf("[Warning] Could not create CSV log file: %v\n", err)
	}

	var frameCount uint64 = 0
	var startTime time.Time
	var lastTime time.Time
	// --------------------------------

	for {
		size, err := client.receiveFlow.Read(buffer)
		if err != nil {
			fmt.Printf("\n[Client Log] Server finished transmission. Wrapping up video file...\n")
			break
		}

		if size == 0 {
			continue
		}

		// --- ADDED: Extract Metrics & Write to CSV ---
		if size >= 12 { // Minimum RTP header size
			frameCount++
			now := time.Now()
			
			if frameCount == 1 {
				startTime = now
				lastTime = now
			}
			
			timeRelative := now.Sub(startTime).Seconds()
			timeDelta := now.Sub(lastTime).Seconds()
			lastTime = now

			// RTP Sequence Number is stored in bytes 2 and 3 (Big Endian)
			seq := binary.BigEndian.Uint16(buffer[2:4])

			if csvFile != nil {
				csvFile.WriteString(fmt.Sprintf("%d,%.6f,%.6f,%d,%d\n", frameCount, timeRelative, timeDelta, size, seq))
			}
		}
		// ---------------------------------------------

		bytes := make([]byte, size)
		copy(bytes, buffer[:size])
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
