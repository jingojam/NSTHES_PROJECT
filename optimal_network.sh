#!/bin/bash

H2_PID=$1

if [ -z "$H2_PID" ]; then
    echo "[Error] H2_PID not provided to network script!"
    exit 1
fi

# 0. Start Tshark on the RECEIVER (h2) namespace to capture raw traffic
nsenter -t $H2_PID -n tshark -i h2-eth0 -a duration:120 -w /tmp/optimal_log_baseline_roq.pcap &

# 1. Clear any leftover traffic control rules on h1
tc qdisc del dev h1-eth0 root 2>/dev/null

# 2. Apply the Optimal baseline conditions
tc qdisc add dev h1-eth0 root netem delay 20ms rate 100mbit
echo "Optimal baseline applied. Tshark is logging in the background..."

# 3. Sleep for 122 seconds (120s capture window + 2s disk-write safety buffer)
sleep 122

# 4. Copy the files over to your shared workspace
# We move and rename the CSV generated directly by client.go
cp /tmp/optimal_log_baseline_roq.pcap "/mnt/hgfs/THESIS SHARED FOLDER/"
mv /tmp/qos_metrics_baseline_roq.csv "/mnt/hgfs/THESIS SHARED FOLDER/optimal_qos_metrics_baseline_roq.csv" 2>/dev/null

echo "Log and QoS CSV saved to Shared Folder!"
