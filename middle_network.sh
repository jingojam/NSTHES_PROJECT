#!/bin/bash

# 0. Start Tshark with an extended 120-second window
tshark -i h1-eth0 -a duration:120 -w /tmp/middle_log.pcap &

# 1. Clear any old rules
tc qdisc del dev h1-eth0 root 2>/dev/null

echo "[Status] Starting Middle Scenario (6-second Oscillation Loop)"
tc qdisc add dev h1-eth0 root netem delay 20ms rate 12mbit

# Loop for 60 seconds (5 iterations of 12 seconds total per cycle)
for i in {1..5}
do
    echo "[$(( (i-1)*12 ))s] High Bandwidth (12 Mbps) -> Prompts 1080p Upgrade"
    tc qdisc change dev h1-eth0 root netem delay 20ms rate 12mbit
    sleep 6

    echo "[$(( (i-1)*12 + 6 ))s] Low Bandwidth (3.5 Mbps) -> Drains Buffer & Forces 720p Downgrade"
    tc qdisc change dev h1-eth0 root netem delay 20ms rate 3.5mbit
    sleep 6
done

echo "[60s] Oscillation loop complete. Restoring full bandwidth..."
tc qdisc change dev h1-eth0 root netem delay 20ms rate 12mbit
sleep 60

# 2. Copy the complete log file
cp /tmp/middle_log.pcap "/mnt/hgfs/THESIS SHARED FOLDER/"
echo "Log saved to Shared Folder!"
