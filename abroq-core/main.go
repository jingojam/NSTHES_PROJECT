package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"os"
	"strings"

	"github.com/mengelbart/roq"
	"github.com/quic-go/quic-go"
)

func main() {
	role := flag.String("role", "sender", "Role to play: 'sender' or 'receiver'")
	addr := flag.String("addr", "127.0.0.1:4321", "The network QUIC address")
	flag.Parse()

	ctx := context.Background()

	if *role == "sender" {
		runSender(ctx, *addr)
	} else if *role == "receiver" {
		runReceiver(ctx, *addr)
	} else {
		fmt.Println("Invalid role. Use -role=sender or -role=receiver")
		os.Exit(1)
	}
}

func runSender(ctx context.Context, remoteQUICAddr string) {
	log.Printf("[SENDER] Starting. Connecting to Receiver at %s...\n", remoteQUICAddr)

	tlsConf := &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"roq"}}
	quicConfig := &quic.Config{EnableDatagrams: true}
	
	conn, err := quic.DialAddr(ctx, remoteQUICAddr, tlsConf, quicConfig)
	if err != nil {
		log.Fatalf("[SENDER] Failed to connect via QUIC: %v", err)
	}
	defer conn.CloseWithError(0, "goodbye")

	roqConn := roq.NewQUICGoConnection(conn)
	session, err := roq.NewSession(roqConn, true, nil)
	if err != nil {
		log.Fatalf("[SENDER] Failed to init RoQ Session: %v", err)
	}
	defer session.Close()

	sendFlow, err := session.NewSendFlow(1)
	if err != nil {
		log.Fatalf("[SENDER] Failed to create Send Flow: %v", err)
	}
	defer sendFlow.Close()

	localUDPAddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:5004")
	gstreamerIn, err := net.ListenUDP("udp", localUDPAddr)
	if err != nil {
		log.Fatalf("[SENDER] Failed to open local GStreamer socket: %v", err)
	}
	defer gstreamerIn.Close()

	log.Println("[SENDER] Setup complete. Awaiting video frames from GStreamer on local port 5004...")

	buf := make([]byte, 2048)
	packetCount := 0
	for {
		n, _, err := gstreamerIn.ReadFromUDP(buf)
		if err != nil {
			log.Printf("[SENDER] Read error: %v", err)
			continue
		}

		err = sendFlow.WriteRTPBytes(buf[:n])
		if err != nil {
			log.Printf("[SENDER] RoQ Write error: %v", err)
			break
		}

		packetCount++
		if packetCount == 1 || packetCount%30 == 0 {
			log.Printf("[SENDER] Telemetry: Caught and transmitted %d video packets via RoQ/QUIC\n", packetCount)
		}
	}
}

func runReceiver(ctx context.Context, listenQUICAddr string) {
	log.Printf("[RECEIVER] Starting. Listening for incoming QUIC at %s...\n", listenQUICAddr)

	gstOutAddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:6004")
	gstreamerOut, err := net.DialUDP("udp", nil, gstOutAddr)
	if err != nil {
		log.Fatalf("[RECEIVER] Failed to target local GStreamer player: %v", err)
	}
	defer gstreamerOut.Close()

	quicConfig := &quic.Config{EnableDatagrams: true}
	listener, err := quic.ListenAddr(listenQUICAddr, generateTLSConfig(), quicConfig)
	if err != nil {
		log.Fatalf("[RECEIVER] Failed to bind QUIC listener: %v", err)
	}
	defer listener.Close()

	conn, err := listener.Accept(ctx)
	if err != nil {
		log.Fatalf("[RECEIVER] Connection accept error: %v", err)
	}
	log.Println("[RECEIVER] Connected! Initializing RoQ Session...")

	roqConn := roq.NewQUICGoConnection(conn)
	// FIX: Changed second argument from false to true so the receiver accepts incoming video datagrams
	session, err := roq.NewSession(roqConn, true, nil)
	if err != nil {
		log.Fatalf("[RECEIVER] Failed to init RoQ Session: %v", err)
	}
	defer session.Close()

	receiveFlow, err := session.NewReceiveFlow(1)
	if err != nil {
		log.Fatalf("[RECEIVER] Failed to match Receive Flow: %v", err)
	}
	defer receiveFlow.Close()

	log.Println("[RECEIVER] Streaming pipeline active. Forwarding data to GStreamer port 6004...")

	buf := make([]byte, 2048)
	packetCount := 0
	for {
		n, err := receiveFlow.Read(buf)
		if err != nil {
			if strings.Contains(err.Error(), "deadline") {
				continue 
			}
			log.Printf("[RECEIVER] RoQ Read error: %v", err)
			break
		}

		_, err = gstreamerOut.Write(buf[:n])
		if err != nil {
			log.Printf("[RECEIVER] Forwarding error to GStreamer: %v", err)
		}

		packetCount++
		if packetCount == 1 || packetCount%30 == 0 {
			log.Printf("[RECEIVER] Telemetry: Received and forwarded %d video packets to GStreamer player\n", packetCount)
		}
	}
}

func generateTLSConfig() *tls.Config {
	key, err := rsa.GenerateKey(rand.Reader, 1025)
	if err != nil {
		panic(err)
	}
	template := x509.Certificate{SerialNumber: big.NewInt(1)}
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		panic(err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		NextProtos:   []string{"roq"},
	}
}
