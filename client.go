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

var flowID uint64 = 1

var currentCapacityUsed uint64 = 0



type Client struct {
	/*Network attributes*/
	ip             string
	port           uint64
	quicConnection quic.Connection
	receiveFlow    *roq.ReceiveFlow
	
	/*SCReAM congestion controller (feedback)*/
	screamRx *scream.Rx

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
		screamRx:		scream.NewRx(12345678),
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

	// FIX: We must explicitly enable datagrams at the underlying QUIC transport layer!
	quicConf := &quic.Config{
		EnableDatagrams: true,
	}

	// Pass quicConf here instead of nil
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

	/*
	client.pipeline.GetPipelineBus().AddWatch(func(msg *gst.Message) bool {
		switch msg.Type() {
		case gst.MessageAsyncDone:

		case gst.MessageEOS:
			fmt.Println("End of Stream reached.")
			client.pipeline.BlockSetState(gst.StateNull)
			mainLoop.Quit()
		case gst.MessageError:
			err := msg.ParseError()
			fmt.Println("Error: ", err.Error())
			mainLoop.Quit()
		}
		return true
	})*/

	// Spin up goroutine to feed incoming QUIC packets to GStreamer appsrc
	go client.ReceivePackets(mainLoop)

	client.pipeline.SetState(gst.StatePlaying)

	fmt.Printf("[Status] Client connected. Receiving stream and saving raw TS data to %s...\n", outputFile)
	mainLoop.Run()

	fmt.Printf("[Status] Streaming finished\n")
	return nil
}


/*
	helper from SCReAM library
	(currently not exposed in the go packages, you have
	to manually inspect signatures via cli commands)
*/
func toNTP(t time.Time) uint32{
    s := (float64(t.UnixNano()) / 1000000000) + 2208988800
    return uint32(s * 65536)
}

/*
	for receiving rtp packets
*/
func (client *Client) ReceivePackets(mainLoop *glib.MainLoop){
	buffer := make([]byte, 65536)
	appsource := app.SrcFromElement(client.source)
	videodata := []byte{}

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
		
		//ntpTime := toNTP(time.Now())
		

		ssrc := uint32(buffer[8])<<24 | uint32(buffer[9])<<16 | uint32(buffer[10])<<8 | uint32(buffer[11])
		sequenceNumber := uint16(buffer[2])<<8 | uint16(buffer[3])
		ceBits := uint8(0)
		mark := (buffer[1] & 0x80) != 0


		// this Receive method needs to be called every time an rtp packet arrives
		client.screamRx.Receive(toNTP(time.Now()), ssrc, size, sequenceNumber, ceBits, mark)
		
		// if its a feedback, send feedback to the server on rtp over the media quic Connection
		if client.screamRx.IsFeedback(toNTP(time.Now())){
				feedback, flag := client.screamRx.CreateStandardizedFeedback(toNTP(time.Now()), mark)
				
				if flag{
					err := client.quicConnection.SendDatagram(feedback)
					
					if err != nil{
						fmt.Printf("\n[SCReAM Log] Error sending RTCP feedback: %v\n", err)
					}
				}
		}

		//New addition
		videodata = append(videodata, bytes...) 


		//Stream video data 
		if len(videodata) >= 1_250_000 && len(videodata) <= 3_750_000 {
		
			gstbuf := gst.NewBufferFromBytes(videodata)
			fmt.Println("size: ", len(videodata))
			ret := appsource.PushBuffer(gstbuf)

			if ret != gst.FlowOK {
				break
			}

		} else if len(videodata) > 3_750_000 {
			fmt.Printf("[Client Log] Buffer capacity exceeded, requesting server to slow down...\n")

			err := client.quicConnection.SendDatagram([]byte("SLOW_DOWN"))

			if err != nil { fmt.Printf("Error: %s\n", err) }
		}
		//////////////////////////////////////////

		
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
