#!/bin/bash

# 0. Start Tshark with an extended 120-second window
tshark -i h1-eth0 -a duration:120 -w /tmp/suboptimal_log_abroq.pcap &

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
sleep 110

# 2. Copy the complete log file and QoS metrics over to your shared workspace
cp /tmp/suboptimal_log_abroq.pcap "/mnt/hgfs/THESIS SHARED FOLDER/suboptimal_log_abroq.pcap"
mv /tmp/qos_metrics_abroq.csv "/mnt/hgfs/THESIS SHARED FOLDER/suboptimal_qos_metrics_abroq.csv" 2>/dev/null

echo "Log and QoS CSV saved to Shared Folder!"
