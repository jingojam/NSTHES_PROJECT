# ABROQ: Automated Baseline Evaluation Enhancement

## TLDR: RoQ delivery still works originally as in Tian's Repo, just added enhancements to cater data standardization for QoE Evaluation

This repository contains the upgraded scientific evaluation harness for the Adaptive Bitrate over Real-time Media over QUIC (ABROQ) thesis. 

The original codebase provided a basic proof-of-concept for RoQ. This upgraded version transforms it into an automated, highly synchronized test environment capable of safely logging TLS traffic, capturing raw network streams without UI overhead, and perfectly formatting video outputs for FFMetrics VMAF/SSIM analysis.

## ⚠️ IMPORTANT SETUP FOR TEAMMATES
Before running the experiments on your local VM, you **must** update the shared directory path in the bash scripts. 

Open `run_experiment.sh` and locate **Line 26**:
` ` `bash
# CHANGE THIS PATH TO MATCH YOUR MACHINE'S SHARED FOLDER OR LOCAL DIRECTORY
SHARED_DIR="/mnt/hgfs/THESIS SHARED FOLDER" 
` ` `
*(Note: Be sure to remove the spaces between the backticks when you paste this part if your editor adds them!)*

#If you do not change this to a valid directory on your system, the script will fail to save the final `.mp4` and `.pcap` files

---

## 📦 Dependencies
To run this suite, your machine must have the following installed:

### 1. System Packages (Ubuntu/Debian)
` ` `bash
sudo apt-get update
sudo apt-get install -y mininet wireshark tshark ffmpeg libgstreamer1.0-dev libgstreamer-plugins-base1.0-dev gstreamer1.0-plugins-good gstreamer1.0-plugins-bad gstreamer1.0-plugins-ugly gcc pkg-config
` ` `

### 2. Go Environment
* **Go** (v1.18+ recommended)
* **CGO must be enabled** for the GStreamer Go-bindings to work. 
` ` `bash
export CGO_ENABLED=1
go mod tidy
` ` `

---

## 📄 Upgraded File Breakdown

### `server.go`
The RoQ server has been upgraded to properly emulate a live media server.
* **Media Pacing (`sync=true`):** The GStreamer `appsink` now respects presentation timestamps, forcing the server to send data at exactly 30fps instead of dumping the file instantly and overflowing network buffers.
* **TLS Keylogging:** Automatically exports the QUIC TLS 1.3 session keys to `secrets.txt`. This allows `tshark` to decrypt the payload and accurately track dropped packets.

### `client.go`
The RoQ client has been upgraded for headless execution and raw data capture.
* **Tee Pipeline:** Replaced the simple screen output with a split pipeline. It now drops network bytes directly to a raw `output.ts` file for lossless metrics analysis, while optionally playing the video on screen (with a `leaky` queue so UI lag doesn't ruin the recorded file).
* **Headless Toggle:** Contains a `headlessMode` boolean. Set to `true` to disable the video popup entirely to save VM CPU resources during heavy testing.
* **Graceful Teardown:** Detects when the server stops sending data, waits 2 seconds, and automatically closes the application.

### `run_experiment.sh`
The automation orchestrator. This script manages the entire Mininet lifecycle.
* Dynamically locates the Mininet `h1` and `h2` processes.
* Launches the server and client inside the correct virtual nodes.
* Triggers the specified network condition script (`optimal_network.sh`, `middle_network.sh`, etc.).
* Polls the client to wait for a graceful exit.
* **FFmpeg Post-Processing:** Automatically extracts the raw `.ts` file, pads any missing frames caused by network packet drops using the `tpad` filter, and forces exactly 1800 frames into an `.mp4` container for perfect FFMetrics synchronization.

---

## 🚀 How to Run the Experiments

### Step 1: Start Mininet
Open a terminal and start your Mininet topology. Leave this terminal running.
` ` `bash
sudo mn --topo=single,2 --mac --controller=none
` ` `

### Step 2: Build the Go Binaries
In a new terminal pane, compile the updated Go code:
` ` `bash
go build -o server_app server.go
go build -o client_app client.go
` ` `

### Step 3: Run the Automation Script
Execute the script using `sudo`, passing the desired network scenario and your standardized Baseline profile reference video.

**Syntax:**
`sudo ./run_experiment.sh <scenario> <video_file>`

**Examples:**
` ` `bash
sudo ./run_experiment.sh optimal ./elephant_baseline_5mbps.mp4
sudo ./run_experiment.sh middle ./elephant_baseline_5mbps.mp4
sudo ./run_experiment.sh suboptimal ./elephant_baseline_5mbps.mp4
` ` `

### Step 4: Check Your Output
Once the terminal reads `EXPERIMENT COMPLETE`, navigate to your defined `SHARED_DIR`. You will find:
1.  **`received_<scenario>.mp4`**: The padded, synchronized video ready for FFMetrics.
2.  **`<scenario>_log.pcap`**: The full network packet capture (with `secrets.txt` available in the root folder for decryption).
