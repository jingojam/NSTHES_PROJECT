#!/bin/bash

# 0. Start Tshark with an extended 120-second window to capture the full streaming lifecycle
tshark -i h1-eth0 -a duration:120 -w /tmp/optimal_log.pcap &

# 1. Clear any leftover traffic control rules on h1
tc qdisc del dev h1-eth0 root 2>/dev/null

# 2. Applying combined network parameters
tc qdisc add dev h1-eth0 root netem delay 50ms loss gemodel 1% 10% 0.1% 20% rate 100mbit

echo "Combined baseline applied. Tshark is logging in the background..."

# 3. Sleep for 122 seconds (120s capture window + 2s disk-write safety buffer)
sleep 122

# 4. Copy the complete log file over to your shared workspace
cp /tmp/optimal_log.pcap "/home/jingo-green/Desktop/NSTHES_PROJECT/logs"
echo "Log saved to Shared Folder!"
