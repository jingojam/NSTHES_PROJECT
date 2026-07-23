#!/bin/bash

# 0. Start Tshark with an extended 120-second window
tshark -i h1-eth0 -a duration:120 -w /tmp/middle_log.pcap &

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
sleep 60

# 2. Copy the complete log file
cp /tmp/middle_log.pcap "/home/jingo-green/Desktop/NSTHES_PROJECT/logs"
echo "Log saved to Shared Folder!"
