# RoQ (RTP over QUIC) Video Streaming Engine

An adaptive bitrate (ABR) video streaming system built in **Go** that transports RTP packets over QUIC Datagrams (`roq-10` protocol) using **GStreamer** for media encoding/decoding and **SCReAM** for network congestion control and telemetry.

---

## Architecture Overview

```text
+-------------------------------------------------------------------------------+
|                                 SERVER SIDE                                   |
|  [Filesrc] -> [x264enc] -> [rtph264pay] -> [AppSink]                          |
|                                                |                              |
|                                         (packetChan)                          |
|                                                v                              |
|  [SCReAM TX Rate Control] <---> [DrainQueue Pacer] -> [QUIC SendFlow]         |
+-------------------------------------------------------------------------------+
                                                 |
                                         QUIC Datagrams
                                                 |
+-------------------------------------------------------------------------------+
|                                 CLIENT SIDE                                   |
|  [QUIC ReceiveFlow] -> [AppSrc] -> [Tee] -> [Playout Queue] -> [Fakesink]     |
|                                      |                                        |
|                              (SCReAM RX Telemetry)                            |
|                                      v                                        |
|                          [Compound RTCP Feedback]                             |
|                    (SCReAM CC Feedback + BUFF APP Packet)                     |
+-------------------------------------------------------------------------------+
```

---

## Key Features

1. **RTP over QUIC (RoQ):** Low-latency media transport using QUIC Datagrams.
2. **SCReAM Congestion Control:** Standardized congestion control designed for real-time media transport (RFC 8888 feedback).
3. **Buffer-Aware Dynamic ABR:** Custom ABR algorithm utilizing moving average throughput smoothing ($T_{safe} = 0.8 \times T_{raw}$) and client buffer level telemetry (`"BUFF"` RTCP APP packets) to handle Fast-Start startup and steady-state switching.
4. **Dynamic Rescaling:** Live modification of GStreamer `capsfilter` resolution constraints (**480p**, **720p**, **1080p**) without tearing down the connection.

---

## Critical Bug to Fix: Server Pacing

Currently, packets are being burst-transmitted without adhering to SCReAM’s pacing intervals.

### Location: `server.go` -> `DrainQueue()`

**Current Implementation:**
```go
func (server *Server) DrainQueue() {
    for pkt := range server.packetChan {
        // BUG: Delay calculation is ignored, no sleep is executed!
        server.screamCC.IsOkToTransmit(time.Now(), 12345678)

        err := server.sendFlow.WriteRTPBytes(pkt.payload)
        ...
    }
}
```

### The Fix:
`IsOkToTransmit()` returns a `float64` representing the required transmit delay in seconds. You need to capture this delay and force the goroutine to sleep if the delay is greater than 0:

```go
func (server *Server) DrainQueue() {
    for pkt := range server.packetChan {
        // 1. Get pacing delay in seconds
        transmitDelay := server.screamCC.IsOkToTransmit(time.Now(), 12345678)

        // 2. Enforce the pacing delay
        if transmitDelay > 0 {
            time.Sleep(time.Duration(transmitDelay * float64(time.Second)))
        }

        // 3. Transmit packet over QUIC
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
```

---

## Prerequisites & Dependencies

### 1. System Packages (Linux/Ubuntu)
You must have Cgo and GStreamer 1.0 development packages installed:

```bash
sudo apt update
sudo apt install -y \
    golang \
    pkg-config \
    libgstreamer1.0-dev \
    libgstreamer-plugins-base1.0-dev \
    gstreamer1.0-plugins-base \
    gstreamer1.0-plugins-good \
    gstreamer1.0-plugins-bad \
    gstreamer1.0-plugins-ugly \
    gstreamer1.0-libav \
    gstreamer1.0-tools
```

### 2. TLS Certificates
QUIC requires TLS. Generate self-signed certificates in the project root directory:

```bash
openssl req -x509 -newkey rsa:2048 -nodes \
    -keyout server.key -out server.crt -days 365 \
    -subj "/CN=localhost"
```

---

## How to Run

### Step 1: Launch the Server
The server requires an IP, port, and a path to an input video file (`.mp4` or `.mkv`).

```bash
go run server.go 127.0.0.1:5000 /path/to/sample_video.mp4
```

### Step 2: Launch the Client
In a separate terminal, launch the client. It takes the target server address and an output file path for saved stream output (`.ts`).

```bash
go run client.go 127.0.0.1:5000 output.ts
```

---

## Log Analysis & Telemetry

* **Server Logs (`server_abr.log`):** Logs bitrate changes, buffer feedback, active quality tiers, and encoder adjustments.
* **QUIC TLS Secrets (`secrets.txt`):** Automatically logs TLS session keys so you can decrypt traffic in Wireshark.

### ABR Resolution Tiers
| Tier | Quality | Target Bitrate ($T_{raw}$) | Encoder Minimum | Dimensions |
| :--- | :--- | :--- | :--- | :--- |
| **0** | 480p | 800 kbps | 800 kbps | 854x480 |
| **1** | 720p | 2.5 Mbps | 2500 kbps | 1280x720 |
| **2** | 1080p | 5.0 Mbps | 5000 kbps | 1920x1080 |
