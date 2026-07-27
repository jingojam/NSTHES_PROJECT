#!/bin/bash

H2_PID=$1

if [ -z "$H2_PID" ]; then
    echo "[Error] H2_PID not provided to network script!"
    exit 1
fi

# 0. Start Tshark on the RECEIVER (h2) namespace to capture raw traffic
nsenter -t $H2_PID -n tshark -i h2-eth0 -a duration:120 -w /tmp/middle_log_baseline_roq.pcap &

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

# Wait for remainder of the 120s capture plus safety buffer
sleep 62

# 2. Copy the files over to your shared workspace
# We move and rename the CSV generated directly by client.go
cp /tmp/middle_log_baseline_roq.pcap "/mnt/hgfs/THESIS SHARED FOLDER/"
mv /tmp/qos_metrics_baseline_roq.csv "/mnt/hgfs/THESIS SHARED FOLDER/middle_qos_metrics_baseline_roq.csv" 2>/dev/null

echo "Log and QoS CSV saved to Shared Folder!"
