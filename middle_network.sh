#!/bin/bash

# Grab the receiver's PID passed from the main experiment script
H2_PID=$1

if [ -z "$H2_PID" ]; then
    echo "[Error] H2_PID not provided to network script!"
    exit 1
fi

# 0. Start Tshark on the RECEIVER (h2) namespace to capture true network effects
nsenter -t $H2_PID -n tshark -i h2-eth0 -a duration:120 -w /tmp/middle_log.pcap &

# 1. Clear any old rules
tc qdisc del dev h1-eth0 root 2>/dev/null

echo "[Status] Starting Middle Scenario (3-second Oscillation Loop)"
tc qdisc add dev h1-eth0 root netem delay 20ms rate 12mbit

# Loop for ~60 seconds of active video streaming (10 iterations of 6 seconds)
for i in {1..10}
do
    echo "[$((i*6-6))s] Oscillation: 12 Mbps"
    tc qdisc change dev h1-eth0 root netem delay 20ms rate 12mbit
    sleep 3
    
    echo "[$((i*6-3))s] Oscillation: 6 Mbps"
    tc qdisc change dev h1-eth0 root netem delay 20ms rate 6mbit
    sleep 3
done

echo "[60s] Network profile timeline complete. Extending observation window..."
tc qdisc change dev h1-eth0 root netem delay 20ms rate 12mbit

# Wait for the remainder of the 120s capture plus a 2-second disk write safety buffer
sleep 62

# 2. Automate the CSV Generation
echo "Extracting QoS metrics to CSV..."
tshark -r /tmp/middle_log.pcap -d udp.port==5000,rtp -Y "rtp" -T fields \
  -e frame.number \
  -e frame.time_relative \
  -e frame.time_delta \
  -e frame.len \
  -e rtp.seq \
  -E header=y -E separator=, > /tmp/middle_qos_metrics.csv

# 3. Generate Automated RTP Statistics
echo "Extracting Automated Jitter and Packet Loss summary..."
tshark -r /tmp/middle_log.pcap -o "rtp.heuristic_rtp: TRUE" -d udp.port==5000,rtp -q -z rtp,streams > /tmp/middle_rtp_summary.txt

# 4. Copy ALL files over to your shared workspace
cp /tmp/middle_log.pcap "/mnt/hgfs/THESIS SHARED FOLDER/"
cp /tmp/middle_qos_metrics.csv "/mnt/hgfs/THESIS SHARED FOLDER/"
cp /tmp/middle_rtp_summary.txt "/mnt/hgfs/THESIS SHARED FOLDER/"

echo "Log, QoS CSV, and RTP Summary saved to Shared Folder!"
