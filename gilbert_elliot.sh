#!/bin/bash

# 0. Start Tshark with an extended 120-second window to capture the full streaming lifecycle
tshark -i h1-eth0 -a duration:120 -w /tmp/loss_log.pcap &

# 1. Clear any leftover traffic control rules on h1
tc qdisc del dev h1-eth0 root 2>/dev/null

echo "[$((i*6-6))s] Applying Gilbert-Elliot Loss."

# parameters: p r 1-h 1-k
# p (probability from Good to Bad) = 1% chance to worsen
# r (probability from Bad to Good) = 10% chance to improve
# 1-h (probability of a successful transmission when Bad) = 0.1% to successfully transmit packets
# 1-k (probability of packet loss when Good) = 40% chance to drop packets
tc qdisc add dev h1-eth0 root netem loss gemodel 1% 10% 0.1% 40%

echo "Optimal baseline applied. Tshark is logging in the background..."

# 4. Copy the complete log file over to your shared workspace
cp /tmp/loss_log.pcap "/home/jingo-green/Desktop/NSTHES_PROJECT/logs"
echo "Log saved to Shared Folder!"
