#!/bin/bash

H2_PID=$1

if [ -z "$H2_PID" ]; then
    echo "[Error] H2_PID not provided to network script!"
    exit 1
fi

# 0. Start Tshark on the RECEIVER (h2) namespace to capture raw traffic
nsenter -t $H2_PID -n tshark -i h2-eth0 -a duration:120 -w /tmp/suboptimal_log_baseline_roq.pcap &

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
sleep 112

# 2. Copy the files over to your shared workspace
# We move and rename the CSV generated directly by client.go
cp /tmp/suboptimal_log_baseline_roq.pcap "/mnt/hgfs/THESIS SHARED FOLDER/"
mv /tmp/qos_metrics_baseline_roq.csv "/mnt/hgfs/THESIS SHARED FOLDER/suboptimal_qos_metrics_baseline_roq.csv" 2>/dev/null

echo "Log and QoS CSV saved to Shared Folder!"
