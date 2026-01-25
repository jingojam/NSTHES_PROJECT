# when testing, it is important to start the receiver first BEFORE the sender
echo "[Staring Receiver]"

# ELEMENTS:
# quinnquicsrc: receiver pipeline for RoQ
# secure-connection=false: only on localhost
# use-datagram=true: use DATAGRAMS
# "application/x-rtp,media=video,encoding-name=H264": 
#   - application/x-rtp: identify data format as an RTP stream
#   - media=video: specify that RTP stream contains video data
#   - encoding-name=H264: specify that encoding scheme uses H.264 codec
# rtph264depay: H264 depayloader
# h264parse: prepare raw h264 data for decoders/mux
# avdec_h264: H264 decoder
# videoconvert: automatically negotiate compatible format
# autovideosink: automatic playback output
gst-launch-1.0 quinnquicsrc port=5000 secure-connection=false use-datagram=true ! "application/x-rtp,media=video,encoding-name=H264" ! rtph264depay ! h264parse ! avdec_h264 ! videoconvert ! autovideosink
