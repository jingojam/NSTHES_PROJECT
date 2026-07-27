#!/bin/bash

# 0. Start Tshark with an extended 120-second window to capture the full streaming lifecycle
tshark -i h1-eth0 -a duration:120 -w /tmp/loss_log.pcap &

# 1. Clear any leftover traffic control rules on h1
tc qdisc del dev h1-eth0 root 2>/dev/null

echo "[$((i*6-6))s] Applying Static Loss."

# 5% Loss Rate
tc qdisc add dev h1-eth0 root netem loss random 5%

echo "Optimal baseline applied. Tshark is logging in the background..."

# 4. Copy the complete log file over to your shared workspace
cp /tmp/loss_log.pcap "/home/jingo-green/Desktop/NSTHES_PROJECT/logs"
echo "Log saved to Shared Folder!"
