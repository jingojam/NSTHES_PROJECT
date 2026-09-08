#!/bin/bash

WD=$(pwd)

echo "$WD"

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

# Extract the base name, then strip everything after the first underscore
VIDEO_BASENAME=$(basename -- "$VIDEO_FILE")
VIDEO_NAME="${VIDEO_BASENAME%%_*}"

echo "=========================================================="
echo "STARTING AUTOMATED EXPERIMENT: $SCENARIO (5 RUNS)"
echo "VIDEO SEQUENCE: $VIDEO_NAME"
echo "=========================================================="

# 1. Dynamically locate Mininet host PIDs
H1_PID=$(ps ax | grep "mininet:h1" | grep -v grep | awk '{print $1}')
H2_PID=$(ps ax | grep "mininet:h2" | grep -v grep | awk '{print $1}')

if [[ -z "$H1_PID" || -z "$H2_PID" ]]; then
    echo "[Error] Could not locate Mininet hosts h1 or h2. Is Mininet running?"
    exit 1
fi

echo "[Status] Found h1 (PID: $H1_PID) and h2 (PID: $H2_PID)"

# Execute 5 iterations
for RUN in {1..5}; do
    echo "----------------------------------------------------------"
    echo " STARTING RUN $RUN OF 5"
    echo "----------------------------------------------------------"

    # 2. Define staging TS file, final MP4, and PCAP destinations for this run
    RAW_TS="${WD}/received_${VIDEO_NAME}_${SCENARIO}_run${RUN}.ts"
    FINAL_MP4="${WD}/received_${VIDEO_NAME}_${SCENARIO}_run${RUN}.mp4"
    PCAP_FILE="${WD}/received_${VIDEO_NAME}_${SCENARIO}_run${RUN}.pcap"
    TMP_PCAP="/tmp/temp_capture_${VIDEO_NAME}_${SCENARIO}_run${RUN}.pcap"

    # Clean up any leftover staging files from previous interrupted attempts
    rm -f "$RAW_TS" "$PCAP_FILE" "$TMP_PCAP"

    # 3. Launch tshark on h1 (Background) capturing to /tmp/ to bypass permission drops
    echo "[Executing] Starting tshark packet capture on h1..."
    sudo mnexec -a $H1_PID tshark -i h1-eth0 -w "$TMP_PCAP" -q > /tmp/tshark_${VIDEO_NAME}_${SCENARIO}_run${RUN}.log 2>&1 &
    
    # Give tshark a moment to initialize the capture interface
    sleep 2

    # 4. Launch the Server on h1 (Background)
    echo "[Executing] Launching Server on h1..."
    sudo mnexec -a $H1_PID ./server_app 10.0.0.1:5000 "$VIDEO_FILE" > /tmp/server_${VIDEO_NAME}_${SCENARIO}_run${RUN}.log 2>&1 &

    sleep 5

    # 5. Launch the Client on h2 (Background) -> Saving TS stream
    echo "[Executing] Launching Client on h2 (Capturing TS stream)..."
    sudo mnexec -a $H2_PID ./client_app 10.0.0.1:5000 "$RAW_TS" > /tmp/client_${VIDEO_NAME}_${SCENARIO}_run${RUN}.log 2>&1 &

    # 6. Launch specific Network Profile script on h1 (Foreground)
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
        sudo pkill -f "tshark"
        exit 1
    fi

    echo "[Status] Network scenario profile sequence finished."
    echo "[Status] Waiting for client to naturally finish receiving video..."

    # 7. Polling Loop: Safely wait for the client to exit on its own
    while pgrep -f "client_app" > /dev/null; do
        sleep 2
    done

    # Give the OS a split second to release file handlers
    sleep 1

    # 8. Clean up processes, move PCAP, and grant user permissions
    echo "[Cleanup] Stream fully captured! Stopping server and tshark..."
    sudo pkill -f "server_app"
    sudo pkill -f "tshark"
    
    # Wait an extra second to ensure the port and pcap file are fully released
    sleep 1 

    if [ -f "$TMP_PCAP" ]; then
        mv "$TMP_PCAP" "$PCAP_FILE"
        chmod 664 "$PCAP_FILE"
        chown thesis:thesis "$PCAP_FILE" 2>/dev/null
    fi

    # 9. Post-Processing: TS Timestamp Preservation, Scaling, & Exact Frame Padding
    echo "[Processing] Aligning TS container, scaling to 1080p, and forcing exact 1800-frame output..."

    ffmpeg -y -i "$RAW_TS" \
      -filter_complex "[0:v]fps=30:round=near,scale=1920:1080,tpad=stop=-1:stop_mode=clone" \
      -frames:v 1800 \
      -c:v libx264 -crf 0 -preset ultrafast "$FINAL_MP4"

    # Fix permissions on generated MP4 so GUI file manager can copy it freely
    chmod 664 "$FINAL_MP4"
    chown thesis:thesis "$FINAL_MP4" 2>/dev/null

    # Clean up raw staging file to keep workspace neat (Keep PCAP & MP4)
    rm -f "$RAW_TS"

    echo "[Status] RUN $RUN COMPLETE:"
    echo "         Video: $FINAL_MP4"
    echo "         PCAP:  $PCAP_FILE"
    echo "----------------------------------------------------------"
    
    # Brief pause before starting the next run
    sleep 2 
done

echo "=========================================================="
echo "ALL 5 RUNS COMPLETE FOR $VIDEO_NAME UNDER $SCENARIO CONDITIONS"
echo "=========================================================="
