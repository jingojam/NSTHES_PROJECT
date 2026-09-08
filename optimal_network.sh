#!/bin/bash

# 1. Clear any leftover traffic control rules on h1
tc qdisc del dev h1-eth0 root 2>/dev/null

# 2. Apply the Optimal baseline conditions
tc qdisc add dev h1-eth0 root netem delay 20ms rate 100mbit

echo "Optimal baseline applied."
