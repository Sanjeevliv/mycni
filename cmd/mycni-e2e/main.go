package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func main() {
	prog := filepath.Base(os.Args[0])
	args := os.Args[1:]

	if prog == "mycni-e2e" && len(args) > 0 {
		switch args[0] {
		case "ping", "nslookup", "serve":
			prog = args[0]
			args = args[1:]
		}
	}

	switch prog {
	case "ping":
		os.Exit(runPing(args))
	case "nslookup":
		os.Exit(runNslookup(args))
	default:
		os.Exit(runServe(args))
	}
}

func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", ":8080", "HTTP listen address")
	_ = fs.Parse(args)

	podName := os.Getenv("POD_NAME")
	if podName == "" {
		podName, _ = os.Hostname()
	}
	nodeName := os.Getenv("NODE_NAME")
	podIP := os.Getenv("POD_IP")

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"pod":%q,"node":%q,"ip":%q}`+"\n", podName, nodeName, podIP)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go func() {
		<-ctx.Done()
		shutdownCtx, c := context.WithTimeout(context.Background(), 3*time.Second)
		defer c()
		_ = srv.Shutdown(shutdownCtx)
	}()

	fmt.Printf("mycni-e2e HTTP responder listening on %s (pod=%s node=%s ip=%s)\n", *addr, podName, nodeName, podIP)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "HTTP server error: %v\n", err)
		return 1
	}
	return 0
}

func runNslookup(args []string) int {
	var target string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			target = a
			break
		}
	}
	if target == "" {
		fmt.Fprintln(os.Stderr, "Usage: nslookup <hostname>")
		return 1
	}

	dnsServer := readNameserver()
	fmt.Printf("Server:\t\t%s\n", dnsServer)
	fmt.Printf("Address:\t%s#53\n\n", dnsServer)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	addrs, err := net.DefaultResolver.LookupHost(ctx, target)
	if err != nil || len(addrs) == 0 {
		fmt.Fprintf(os.Stderr, "** server can't find %s: %v\n", target, err)
		return 1
	}

	for _, addr := range addrs {
		fmt.Printf("Name:\t%s\nAddress: %s\n", target, addr)
	}
	return 0
}

func readNameserver() string {
	data, err := os.ReadFile("/etc/resolv.conf")
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[0] == "nameserver" {
				return fields[1]
			}
		}
	}
	return "10.96.0.10"
}

func runPing(args []string) int {
	fs := flag.NewFlagSet("ping", flag.ContinueOnError)
	count := fs.Int("c", 3, "number of echo requests to send")
	timeoutSec := fs.Int("W", 3, "timeout in seconds per request")

	var target string
	var flagArgs []string
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") {
			flagArgs = append(flagArgs, args[i])
			if (args[i] == "-c" || args[i] == "-W") && i+1 < len(args) {
				flagArgs = append(flagArgs, args[i+1])
				i++
			}
		} else {
			target = args[i]
		}
	}
	_ = fs.Parse(flagArgs)

	if target == "" {
		fmt.Fprintln(os.Stderr, "Usage: ping [-c count] [-W timeout] <destination>")
		return 1
	}

	ips, err := net.LookupIP(target)
	if err != nil || len(ips) == 0 {
		fmt.Fprintf(os.Stderr, "ping: unknown host %s: %v\n", target, err)
		return 1
	}
	var dstIP net.IP
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil {
			dstIP = v4
			break
		}
	}
	if dstIP == nil {
		fmt.Fprintf(os.Stderr, "ping: no IPv4 address for %s\n", target)
		return 1
	}

	fmt.Printf("PING %s (%s): 56 data bytes\n", target, dstIP.String())

	id := uint16(os.Getpid() & 0xffff)
	received := 0
	timeout := time.Duration(*timeoutSec) * time.Second

	for seq := 1; seq <= *count; seq++ {
		rtt, err := sendICMPEcho(dstIP, id, uint16(seq), timeout)
		if err != nil {
			fmt.Printf("Request timeout for icmp_seq %d: %v\n", seq, err)
		} else {
			received++
			fmt.Printf("64 bytes from %s: icmp_seq=%d ttl=64 time=%.2f ms\n",
				dstIP.String(), seq, float64(rtt.Microseconds())/1000.0)
		}
		if seq < *count {
			time.Sleep(200 * time.Millisecond)
		}
	}

	loss := 100.0 * float64(*count-received) / float64(*count)
	fmt.Printf("\n--- %s ping statistics ---\n", target)
	fmt.Printf("%d packets transmitted, %d packets received, %.0f%% packet loss\n", *count, received, loss)

	if received == 0 {
		return 1
	}
	return 0
}

func sendICMPEcho(dstIP net.IP, id, seq uint16, timeout time.Duration) (time.Duration, error) {
	conn, err := net.DialTimeout("ip4:icmp", dstIP.String(), timeout)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(timeout))

	// Build ICMP Echo Request (Type 8, Code 0)
	msg := make([]byte, 64)
	msg[0] = 8 // Echo Request
	msg[1] = 0 // Code 0
	binary.BigEndian.PutUint16(msg[4:6], id)
	binary.BigEndian.PutUint16(msg[6:8], seq)
	for i := 8; i < len(msg); i++ {
		msg[i] = byte(i)
	}
	csum := icmpChecksum(msg)
	binary.BigEndian.PutUint16(msg[2:4], csum)

	start := time.Now()
	if _, err := conn.Write(msg); err != nil {
		return 0, err
	}

	reply := make([]byte, 1500)
	for {
		n, err := conn.Read(reply)
		if err != nil {
			return 0, err
		}
		payload := reply[:n]
		// Strip IPv4 header if present
		if len(payload) >= 20 && (payload[0]>>4) == 4 {
			hdrLen := int(payload[0]&0x0f) * 4
			if len(payload) >= hdrLen+8 {
				payload = payload[hdrLen:]
			}
		}
		if len(payload) >= 8 && payload[0] == 0 { // Type 0 = Echo Reply
			replyID := binary.BigEndian.Uint16(payload[4:6])
			replySeq := binary.BigEndian.Uint16(payload[6:8])
			if replyID == id && replySeq == seq {
				return time.Since(start), nil
			}
		}
	}
}

func icmpChecksum(data []byte) uint16 {
	var sum uint32
	length := len(data)
	for i := 0; i < length-1; i += 2 {
		sum += uint32(binary.BigEndian.Uint16(data[i : i+2]))
	}
	if length%2 == 1 {
		sum += uint32(data[length-1]) << 8
	}
	for (sum >> 16) > 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}
