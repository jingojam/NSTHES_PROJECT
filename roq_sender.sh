# note: I'm still familiarizing myself with the plugins, but the general syntax for pipelining is:
# Syntax: gst-launch-1.0 [Source] ! [Caps] ! [Filter/Encoder] ! [Payloader] ! [Sink]

echo "[Starting Sender]"

# ELEMENTS:
# videotestsrc: test video in Gstreamer
# videoconvert: automatically negotiate compatible format
# x264enc: h264 encoder
# rtph264pay: h264 codec payloader
# quinnquicsink: sender pipeline for RoQ
# address=127.0.0.1 port=5000: send on localhost:5000
# secure-connection=false: for simplicity purposes, and since we're communicating in localhost only
# use-datagram=true: use DATAGRAMS 
gst-launch-1.0 videotestsrc ! videoconvert ! x264enc ! rtph264pay ! quinnquicsink address=127.0.0.1 port=5000 secure-connection=false use-datagram=true
