#!/bin/bash

# Grab the receiver's PID passed from the main experiment script
H2_PID=$1

if [ -z "$H2_PID" ]; then
    echo "[Error] H2_PID not provided to network script!"
    exit 1
fi

# 0. Start Tshark on the RECEIVER (h2) namespace to capture true network effects
# We use nsenter to jump into h2's network namespace just for this capture
nsenter -t $H2_PID -n tshark -i h2-eth0 -a duration:120 -w /tmp/optimal_log.pcap &

# 1. Clear any leftover traffic control rules on h1
tc qdisc del dev h1-eth0 root 2>/dev/null

# 2. Apply the Optimal baseline conditions (on the sender)
tc qdisc add dev h1-eth0 root netem delay 20ms rate 100mbit

echo "Optimal baseline applied. Tshark is logging on h2-eth0 in the background..."

# 3. Sleep for 122 seconds (120s capture window + 2s disk-write safety buffer)
sleep 122

# 4. Automate the CSV Generation
echo "Extracting QoS metrics to CSV..."
tshark -r /tmp/optimal_log.pcap -d udp.port==5000,rtp -Y "rtp" -T fields \
  -e frame.number \
  -e frame.time_relative \
  -e frame.time_delta \
  -e frame.len \
  -e rtp.seq \
  -E header=y -E separator=, > /tmp/optimal_qos_metrics.csv

# 4.5 Generate Automated RTP Statistics
# We force Wireshark to heuristically recognize the dynamic payload as RTP
echo "Extracting Automated Jitter and Packet Loss summary..."
tshark -r /tmp/optimal_log.pcap -o "rtp.heuristic_rtp: TRUE" -d udp.port==5000,rtp -q -z rtp,streams > /tmp/optimal_rtp_summary.txt

# 5. Copy ALL files over to your shared workspace
cp /tmp/optimal_log.pcap "/mnt/hgfs/THESIS SHARED FOLDER/"
cp /tmp/optimal_qos_metrics.csv "/mnt/hgfs/THESIS SHARED FOLDER/"
cp /tmp/optimal_rtp_summary.txt "/mnt/hgfs/THESIS SHARED FOLDER/"

echo "Log, QoS CSV, and RTP Summary saved to Shared Folder!"
