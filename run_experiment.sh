#!/bin/bash

# Ensure script is run with sudo
if [ "$EUID" -ne 0 ]; then
  echo "Please run as root (sudo ./run_experiment.sh <scenario> <video_file>)"
  exit 1
fi

SCENARIO=$1   # optimal, middle, suboptimal
VIDEO_FILE=$2 # path to the input video file

if [[ -z "$SCENARIO" || -z "$VIDEO_FILE" ]]; then
    echo "Usage: sudo ./run_experiment.sh <optimal|middle|suboptimal> <path_to_video>"
    exit 1
fi

echo "=========================================================="
echo "STARTING AUTOMATED EXPERIMENT: $SCENARIO"
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
RAW_TS="$SHARED_DIR/received_${SCENARIO}.ts"
FINAL_MP4="$SHARED_DIR/received_${SCENARIO}.mp4"

# Clean up any leftover staging file from previous attempts
rm -f "$RAW_TS"

# 3. Launch the Server on h1 (Background) with GStreamer Debugging Enabled
echo "[Executing] Launching Server on h1..."
sudo mnexec -a $H1_PID env GST_DEBUG=3 ./server_app 10.0.0.1:5000 "$VIDEO_FILE" > /tmp/server_execution.log 2>&1 &
SERVER_PROC_PID=$!

sleep 2

# 4. Launch the Client on h2 (Background) -> Saving TS stream
echo "[Executing] Launching Client on h2 (Capturing TS stream)..."
sudo mnexec -a $H2_PID ./client_app 10.0.0.1:5000 "$RAW_TS" > /tmp/client_execution.log 2>&1 &
CLIENT_PROC_PID=$!

# 5. Launch specific Network Profile script on h1 (Foreground)
echo "[Executing] Triggering Network Scenario Profile..."
if [ "$SCENARIO" == "optimal" ]; then
    sudo mnexec -a $H1_PID ./optimal_network.sh
elif [ "$SCENARIO" == "middle" ]; then
    sudo mnexec -a $H1_PID ./middle_network.sh
elif [ "$SCENARIO" == "suboptimal" ]; then
    sudo mnexec -a $H1_PID ./suboptimal_network.sh
else
    echo "[Error] Invalid scenario choice."
    sudo pkill -f "server_app"
    sudo pkill -f "client_app"
    exit 1
fi

echo "[Status] Network scenario profile sequence finished."
echo "[Status] Waiting for client to naturally finish receiving video..."

# 6. Polling Loop: Safely wait for the client to exit on its own!
while pgrep -f "client_app" > /dev/null; do
    sleep 2
done

# Give the OS a split second to release file handlers
sleep 1

# 7. Clean up just the server process
echo "[Cleanup] Stream fully captured! Stopping server..."
sudo pkill -f "server_app"

# 8. Post-Processing: TS Timestamp Preservation, Scaling, & Exact Frame Padding
echo "[Processing] Aligning TS container, scaling to 1080p, and forcing exact 1800-frame output..."

# 1. We use fps=30 to lock internal timestamps and freeze-frame any network drops.
# 2. We use scale=1920:1080 to up-sample the raw network video to 1080p for VMAF compatibility.
# 3. We use tpad with 'stop=-1' to guarantee a massive tail of cloned frames.
# 4. We use -frames:v 1800 to perfectly slice the padded video at exactly 1800 frames.
ffmpeg -y -i "$RAW_TS" \
  -filter_complex "[0:v]fps=30:round=near,scale=1920:1080,tpad=stop=-1:stop_mode=clone" \
  -frames:v 1800 \
  -c:v libx264 -crf 0 -preset ultrafast "$FINAL_MP4"

# Clean up raw staging file to keep shared folder neat
rm -f "$RAW_TS"

echo "=========================================================="
echo "EXPERIMENT COMPLETE: Final file -> $FINAL_MP4"
echo "=========================================================="
