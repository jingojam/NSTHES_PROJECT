# NSTHES_PROJECT

** System Dependencies **
- OS: Ubuntu 24.04.3 LTS (VMWare)

** Guide **
Execute these commands in a terminal (in order):

0) `cd $HOME` -> this is generally `/home/<user>`, to check: `echo $HOME`
1) `git clone https://github.com/jingojam/NSTHES_PROJECT`
2) `cd NSTHES_PROJECT/`
3) `cp conf.sh roq_receiver.sh roq_sender.sh $HOME` -> make sure to copy these 3 files in your `$HOME` directory
4) `cd $HOME`
5) `chmod +x conf.sh`
6) `./conf.sh` -> this may take a while

** Ensure builds are successful: use these commands: **
1) `gst-inspect-1.0 rtp` -> should output RTP plugins
2) `gst-inspect-1.0 quinn` -> should output quinn (RoQ) plugins (e.g., quinnquicdemux/mux, quinnroqdemux/mux)

** To test output **
1) Open two terminals
2) In terminal A: `./roq_receiver.sh` -> receiver must be run first
3) In terminal B: `./roq_sender.sh` 
4) Optionally: you can run `sudo wireshark` in another terminal to sniff
QUIC packets (filter it to QUIC)
