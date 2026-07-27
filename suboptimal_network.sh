#!/bin/bash

# Grab the receiver's PID passed from the main experiment script
H2_PID=$1

if [ -z "$H2_PID" ]; then
    echo "[Error] H2_PID not provided to network script!"
    exit 1
fi

# 0. Start Tshark on the RECEIVER (h2) namespace to capture true network effects
nsenter -t $H2_PID -n tshark -i h2-eth0 -a duration:120 -w /tmp/suboptimal_log.pcap &

# 1. Clear any old rules
tc qdisc del dev h1-eth0 root 2>/dev/null

echo "[00s] Enforcing 12 Mbps Baseline for Fast-Start..."
tc qdisc add dev h1-eth0 root netem delay 20ms rate 12mbit

# Give it 10 seconds to build the 1080p buffer (Steady State)
echo "Waiting 10 seconds for steady state..."
sleep 10

echo "[10s] PANIC DROP: Throttling strictly to 3 Mbps!"
tc qdisc change dev h1-eth0 root netem delay 20ms rate 3mbit

echo "Network profile timeline complete. Extending observation window..."
# Wait for the remainder of the 120s capture plus a 2-second disk write safety buffer
sleep 112

# 2. Automate the CSV Generation
echo "Extracting QoS metrics to CSV..."
tshark -r /tmp/suboptimal_log.pcap -d udp.port==5000,rtp -Y "rtp" -T fields \
  -e frame.number \
  -e frame.time_relative \
  -e frame.time_delta \
  -e frame.len \
  -e rtp.seq \
  -E header=y -E separator=, > /tmp/suboptimal_qos_metrics.csv

# 3. Generate Automated RTP Statistics
echo "Extracting Automated Jitter and Packet Loss summary..."
tshark -r /tmp/suboptimal_log.pcap -o "rtp.heuristic_rtp: TRUE" -d udp.port==5000,rtp -q -z rtp,streams > /tmp/suboptimal_rtp_summary.txt

# 4. Copy ALL files over to your shared workspace
cp /tmp/suboptimal_log.pcap "/mnt/hgfs/THESIS SHARED FOLDER/"
cp /tmp/suboptimal_qos_metrics.csv "/mnt/hgfs/THESIS SHARED FOLDER/"
cp /tmp/suboptimal_rtp_summary.txt "/mnt/hgfs/THESIS SHARED FOLDER/"

echo "Log, QoS CSV, and RTP Summary saved to Shared Folder!"
