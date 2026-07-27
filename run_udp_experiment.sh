#!/bin/bash

# Ensure script is run with sudo
if [ "$EUID" -ne 0 ]; then
  echo "Please run as root (sudo ./run_udp_experiment.sh <scenario> <video_file>)"
  exit 1
fi

SCENARIO=$1   # optimal, middle, suboptimal
VIDEO_FILE=$2 # path to the input video file

if [[ -z "$SCENARIO" || -z "$VIDEO_FILE" ]]; then
    echo "Usage: sudo ./run_udp_experiment.sh <optimal|middle|suboptimal> <path_to_video>"
    exit 1
fi

echo "=========================================================="
echo "STARTING AUTOMATED NATIVE UDP EXPERIMENT: $SCENARIO"
echo "=========================================================="

# 1. Dynamically locate Mininet host PIDs
H1_PID=$(ps ax | grep "mininet:h1" | grep -v grep | awk '{print $1}')
H2_PID=$(ps ax | grep "mininet:h2" | grep -v grep | awk '{print $1}')

if [[ -z "$H1_PID" || -z "$H2_PID" ]]; then
    echo "[Error] Could not locate Mininet hosts h1 or h2. Is Mininet running?"
    exit 1
fi

echo "[Status] Found h1 (PID: $H1_PID) and h2 (PID: $H2_PID)"

# 2. Define staging TS file and final MP4 destination
SHARED_DIR="/mnt/hgfs/THESIS SHARED FOLDER"
RAW_TS="$SHARED_DIR/udp_received_${SCENARIO}.ts"
FINAL_MP4="$SHARED_DIR/udp_received_${SCENARIO}.mp4"

# Clean up any leftover staging file from previous attempts
rm -f "$RAW_TS"

# 3. Launch the GStreamer UDP Client on h2 (Background)
# Listens for incoming RTP H264 data on port 5000 and packages it cleanly into a TS container
echo "[Executing] Launching GStreamer Receiver on h2..."
sudo mnexec -a $H2_PID gst-launch-1.0 -q \
  udpsrc port=5000 caps="application/x-rtp, media=(string)video, clock-rate=(int)90000, encoding-name=(string)H264, payload=(int)96" ! \
  rtph264depay ! h264parse ! mpegtsmux ! filesink location="$RAW_TS" > /tmp/udp_client_execution.log 2>&1 &
CLIENT_PROC_PID=$!

sleep 1

# 4. Launch the GStreamer UDP Server on h1 (Background)
# Reads the source MP4, payload-encodes it to RTP, paces the stream (sync=true), and fires to h2 (10.0.0.2)
echo "[Executing] Launching GStreamer Streamer on h1..."
sudo mnexec -a $H1_PID gst-launch-1.0 -q \
  filesrc location="$VIDEO_FILE" ! qtdemux ! h264parse ! \
  rtph264pay config-interval=1 pt=96 ! udpsink host=10.0.0.2 port=5000 sync=true > /tmp/udp_server_execution.log 2>&1 &
SERVER_PROC_PID=$!

sleep 1

# 5. Launch specific Network Profile script on h1 (Foreground)
# H2_PID is now successfully passed down so the network script can run tshark on the receiver
echo "[Executing] Triggering Network Scenario Profile..."
if [ "$SCENARIO" == "optimal" ]; then
    sudo mnexec -a $H1_PID ./optimal_network.sh $H2_PID
elif [ "$SCENARIO" == "middle" ]; then
    sudo mnexec -a $H1_PID ./middle_network.sh $H2_PID
elif [ "$SCENARIO" == "suboptimal" ]; then
    sudo mnexec -a $H1_PID ./suboptimal_network.sh $H2_PID
else
    echo "[Error] Invalid scenario choice."
    sudo pkill -x gst-launch-1.0
    exit 1
fi

echo "[Status] Network scenario profile sequence finished."
echo "[Status] Waiting for GStreamer server to hit EOS and finish streaming..."

# 6. Polling Loop: Monitor the SERVER process since it will exit naturally at the end of the file
while ps -p $SERVER_PROC_PID > /dev/null; do
    sleep 2
done

# Give the final packets a brief moment to clear the network buffer and write to disk
echo "[Status] Stream transmission complete. Wrapping up client capture..."
sleep 2

# 7. Clean up the passive client process
sudo kill $CLIENT_PROC_PID 2>/dev/null
sudo pkill -f "udpsrc port=5000" 2>/dev/null

# 8. Post-Processing: Exact Frame Padding
echo "[Processing] Aligning TS container and forcing exact 1800-frame output..."

if [ ! -f "$RAW_TS" ]; then
    echo "[Error] Staging file $RAW_TS was never generated! Check GStreamer pipeline errors in /tmp/udp_client_execution.log"
    exit 1
fi

# Locks frame rate, freeze-frames dropped elements, and slices cleanly at 1800 frames
ffmpeg -y -i "$RAW_TS" \
  -filter_complex "[0:v]fps=30:round=near,tpad=stop=-1:stop_mode=clone" \
  -frames:v 1800 \
  -c:v libx264 -crf 0 -preset ultrafast "$FINAL_MP4"

# Clean up raw staging file to keep shared folder neat
rm -f "$RAW_TS"

echo "=========================================================="
echo "UDP EXPERIMENT COMPLETE: Final file -> $FINAL_MP4"
echo "=========================================================="
