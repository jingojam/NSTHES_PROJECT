#!/bin/bash

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

echo "[60s] Network profile timeline complete."
tc qdisc change dev h1-eth0 root netem delay 20ms rate 12mbit
