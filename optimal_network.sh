#!/bin/bash

# 0. Start Tshark with an extended 120-second window to capture the full streaming lifecycle
tshark -i h1-eth0 -a duration:120 -w /tmp/optimal_log_abroq.pcap &

# 1. Clear any leftover traffic control rules on h1
tc qdisc del dev h1-eth0 root 2>/dev/null

# 2. Apply the Optimal baseline conditions
tc qdisc add dev h1-eth0 root netem delay 20ms rate 100mbit

echo "Optimal baseline applied. Tshark is logging in the background..."

# 3. Sleep for 122 seconds (120s capture window + 2s disk-write safety buffer)
sleep 122

# 4. Copy the complete log file and QoS metrics over to your shared workspace
cp /tmp/optimal_log_abroq.pcap "/mnt/hgfs/THESIS SHARED FOLDER/optimal_log_abroq.pcap"
mv /tmp/qos_metrics_abroq.csv "/mnt/hgfs/THESIS SHARED FOLDER/optimal_qos_metrics_abroq.csv" 2>/dev/null

echo "Log and QoS CSV saved to Shared Folder!"
