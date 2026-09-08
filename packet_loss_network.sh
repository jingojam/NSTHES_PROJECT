#!/bin/bash

# 1. Clear any leftover traffic control rules on h1
tc qdisc del dev h1-eth0 root 2>/dev/null

# 2. Apply the 5% Packet Loss condition (on the sender)
tc qdisc add dev h1-eth0 root netem delay 20ms loss random 5% rate 100mbit

echo "Packet Loss (5%) scenario applied."
