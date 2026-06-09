# ABROQ-BASELINE

## 1. Prerequisites

To run this environment, you need two things installed on your system:
1. **Go (Golang) v1.22+:** Ensure you have Go version 1.22.0 or newer installed to compile the application. You can verify your version by running `go version` in your terminal.
2. **GStreamer:** To generate, encode, and play the actual video feed.

### Go Dependencies
You do not need to install the dependencies manually. Running `go mod download` in the setup step will automatically pull the required packages defined in `go.mod`:
* `github.com/quic-go/quic-go` (v0.42.0)
* `github.com/mengelbart/roq`

# Download the required Go modules (reads from go.mod and go.sum)
go mod download

# Compile the baseline application
go build -o abroq-baseline main.go

# The Mininet Baseline Execution Guide

1. Start the Network & Open Terminals
sudo mn
mininet> xterm h1 h1 h2 h2

2. Start the Receiver (h2 - Window 1)
./abroq-baseline -role=receiver -addr=0.0.0.0:4321

3. Start the Video Player (h2 - Window 2)
gst-launch-1.0 udpsrc port=6004 ! application/x-rtp,media=video,clock-rate=90000,encoding-name=H264,payload=96 ! rtph264depay ! avdec_h264 ! videoconvert ! autovideosink sync=false

4. Start the Sender Proxy (h1 - Window 1)
(Note the IP is 10.0.0.2 to target h2 across the Mininet link)
./abroq-baseline -role=sender -addr=10.0.0.2:4321

5. Inject the Video Feed (h1 - Window 2)
(This pushes the video into the Sender Proxy on h1's local port)
gst-launch-1.0 videotestsrc ! video/x-raw,format=I420,width=640,height=480 ! videoconvert
