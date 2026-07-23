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
## ABR Heuristics & Key Updates

### 1. Updated Resolution Tiers & Bitrate Mapping
Bitrate targets and encoder minimum floors were updated to match standard encoding tiers:

| Tier | Quality | Target Bitrate ($T_{raw}$) | Minimum Encoder Bitrate | Resolution |
| :--- | :--- | :--- | :--- | :--- |
| **0 (Low)** | 480p | 800 kbps (0.8 Mbps) | 800 kbps | 854x480 |
| **1 (Mid)** | 720p | 2.5 Mbps | 2500 kbps | 1280x720 |
| **2 (High)** | 1080p | 5.0 Mbps | 5000 kbps | 1920x1080 |

---

### 2. Key Heuristic Differences & Changes

* **Stability Timer Reduced (5.0s → 2.5s):** 
  * **Old:** Required holding higher throughput target for **5.0 seconds** before triggering a resolution upgrade.
  * **New:** Reduced the upgrade stability timer to **2.5 seconds** (`2500ms`) to enable faster adaptation to available bandwidth.

* **Throughput Smoothing via Moving Average:**
  * **Old:** Used instantaneous $T_{safe}$ values, which caused rapid bitrate oscillation on short throughput spikes.
  * **New:** Implemented a **10-sample moving average** (`getSmoothedTsafe()`) over $T_{safe}$ telemetry (~1 second window) to smooth out decisions.

* **Realistic Tier Selection Thresholds (`ComputeTier`):**
  * **Old:** Required reaching full upper bitrates (e.g., 8.0 Mbps for 1080p).
  * **New:** Uses practical safe throughput requirement thresholds:
    * **1080p:** $T_{safe} \ge 3.5\text{ Mbps}$
    * **720p:** $T_{safe} \ge 1.5\text{ Mbps}$
    * **480p:** Fallback ($T_{safe} < 1.5\text{ Mbps}$)

* **Dynamic Encoder Probing & Minimum Bitrate Floors:**
  * **Old:** Bitrate was static per tier transition.
  * **New:** Dynamic encoder bitrate recalculation per feedback packet using a **1.30x probing headroom** ($1.30 \times T_{raw}$). Enforces a strict `minEncoderKbps` floor per tier to prevent spatial starvation/blurriness.

* **Improved Fast-Start Startup & Cooldown Logic:**
  * **Fast-Start Exit Conditions:** Fast-start phase now terminates either when reaching **1080p** or if client playout buffer drops below **300 ms**.
  * **Switch Cooldown:** Added a **3-second cooldown lock** after any resolution change to allow GStreamer caps pipeline adjustments to stabilize.

---

