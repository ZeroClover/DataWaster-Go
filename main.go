package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

type Config struct {
	Help      bool
	Down      bool
	Up        bool
	TestTime  int
	Size      float64
	Parallel  int
	Interface string
	FwMark    int
	LogFile   string
	IPv4Only  bool
	IPv6Only  bool
	Debug     bool
}

type Stats struct {
	StartTime    time.Time
	EndTime      time.Time
	TotalBytes   int64
	LastBytes    int64
	LastTime     time.Time
	MaxSpeed     float64
	CurrentSpeed float64
	ECHAccepted  bool
	mu           sync.RWMutex
}

func (s *Stats) AddBytes(bytes int64) {
	atomic.AddInt64(&s.TotalBytes, bytes)
}

func (s *Stats) UpdateCurrentSpeed() {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	currentTotal := atomic.LoadInt64(&s.TotalBytes)

	if !s.LastTime.IsZero() {
		elapsed := now.Sub(s.LastTime).Seconds()
		if elapsed > 0 {
			bytesDiff := currentTotal - s.LastBytes
			s.CurrentSpeed = float64(bytesDiff) / elapsed
			if s.CurrentSpeed > s.MaxSpeed {
				s.MaxSpeed = s.CurrentSpeed
			}
		}
	} else {
		// First measurement - initialize with some speed if we have data
		if currentTotal > 0 {
			elapsed := now.Sub(s.StartTime).Seconds()
			if elapsed > 0 {
				s.CurrentSpeed = float64(currentTotal) / elapsed
				s.MaxSpeed = s.CurrentSpeed
			}
		}
	}

	s.LastBytes = currentTotal
	s.LastTime = now
}

func (s *Stats) GetCurrentSpeed() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.CurrentSpeed
}

func (s *Stats) GetMaxSpeed() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.MaxSpeed
}

func (s *Stats) GetTotalBytes() int64 {
	return atomic.LoadInt64(&s.TotalBytes)
}

var (
	config      Config
	stats       Stats
	logger      *log.Logger
	cancel      context.CancelFunc
	ctx         context.Context
	echConfig   []byte      // Cached ECH configuration
	echConfigMu sync.Mutex  // Protects echConfig
	echFetched  bool        // Whether ECH config has been fetched
)

func init() {
	flag.BoolVar(&config.Help, "h", false, "Show help")
	flag.BoolVar(&config.Help, "help", false, "Show help")
	flag.BoolVar(&config.Down, "d", false, "Perform download speed test")
	flag.BoolVar(&config.Down, "down", false, "Perform download speed test")
	flag.BoolVar(&config.Up, "u", false, "Perform upload speed test")
	flag.BoolVar(&config.Up, "up", false, "Perform upload speed test")
	flag.IntVar(&config.TestTime, "t", 0, "Test duration in seconds (0 for continuous)")
	flag.IntVar(&config.TestTime, "time", 0, "Test duration in seconds (0 for continuous)")
	flag.Float64Var(&config.Size, "s", 0.1, "Test size in GiB")
	flag.Float64Var(&config.Size, "size", 0.1, "Test size in GiB")
	flag.IntVar(&config.Parallel, "P", 4, "Number of parallel connections")
	flag.IntVar(&config.Parallel, "parallel", 4, "Number of parallel connections")
	flag.StringVar(&config.Interface, "I", "", "Network interface to use")
	flag.StringVar(&config.Interface, "interface", "", "Network interface to use")
	flag.IntVar(&config.FwMark, "fwmark", 0, "Firewall mark")
	flag.StringVar(&config.LogFile, "log", "errout.log", "Error log file path")
	flag.BoolVar(&config.IPv4Only, "4", false, "Use IPv4 only")
	flag.BoolVar(&config.IPv6Only, "6", false, "Use IPv6 only")
	flag.BoolVar(&config.Debug, "debug", false, "Enable debug logging")
}

// checkFlagConflicts detects if both short and long forms of the same flag were used
// and ensures help flag doesn't coexist with other flags
func checkFlagConflicts() {
	args := os.Args[1:]

	// Check if help flag is present
	var hasHelp bool
	var helpFlag string
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			hasHelp = true
			helpFlag = arg
			break
		}
	}

	// Check if help flag is used with other flags
	if hasHelp {
		var otherFlags []string
		for _, arg := range args {
			if arg != helpFlag && strings.HasPrefix(arg, "-") {
				otherFlags = append(otherFlags, arg)
			}
		}

		if len(otherFlags) > 0 {
			fmt.Fprintf(os.Stderr, "Error: Help flag (%s) cannot be used with other flags: %v\n", helpFlag, otherFlags)
			fmt.Fprintf(os.Stderr, "Use '%s' alone to show help information.\n", helpFlag)
			os.Exit(1)
		}
		return
	}

	// Check for short/long form conflicts
	conflictPairs := map[string][]string{
		"help":      {"-h", "--help"},
		"down":      {"-d", "--down"},
		"up":        {"-u", "--up"},
		"time":      {"-t", "--time"},
		"size":      {"-s", "--size"},
		"parallel":  {"-P", "--parallel"},
		"interface": {"-I", "--interface"},
	}

	for category, flags := range conflictPairs {
		var foundFlags []string
		for _, flag := range flags {
			for _, arg := range args {
				if arg == flag || strings.HasPrefix(arg, flag+"=") {
					foundFlags = append(foundFlags, flag)
					break
				}
			}
		}

		if len(foundFlags) > 1 {
			fmt.Fprintf(os.Stderr, "Error: Conflicting flags detected for %s: %v\n", category, foundFlags)
			fmt.Fprintf(os.Stderr, "Please use either the short form (%s) or long form (%s), but not both.\n",
				flags[0], flags[1])
			os.Exit(1)
		}
	}
}

func main() {
	// Check for flag conflicts before parsing
	checkFlagConflicts()

	flag.Parse()

	if config.Help {
		showHelp()
		return
	}

	if config.Down && config.Up {
		fmt.Fprintf(os.Stderr, "Error: -d/--down and -u/--up are mutually exclusive\n")
		os.Exit(1)
	}

	if !config.Down && !config.Up {
		fmt.Fprintf(os.Stderr, "Error: Must specify either -d/--down or -u/--up\n")
		os.Exit(1)
	}

	if config.IPv4Only && config.IPv6Only {
		fmt.Fprintf(os.Stderr, "Error: -4 and -6 are mutually exclusive\n")
		os.Exit(1)
	}

	setupLogger()
	setupSignalHandler()

	// Validate interface if specified
	if config.Interface != "" {
		if _, err := net.InterfaceByName(config.Interface); err != nil {
			fmt.Fprintf(os.Stderr, "Error: Interface '%s' not found\n", config.Interface)
			fmt.Fprintf(os.Stderr, "Available interfaces:\n")
			interfaces, _ := net.Interfaces()
			for _, iface := range interfaces {
				if iface.Flags&net.FlagUp != 0 && iface.Flags&net.FlagLoopback == 0 {
					fmt.Fprintf(os.Stderr, "  %s (UP)\n", iface.Name)
				} else {
					fmt.Fprintf(os.Stderr, "  %s (DOWN)\n", iface.Name)
				}
			}
			logger.Printf("Interface validation failed: %v", err)
			os.Exit(1)
		}
	}

	stats.StartTime = time.Now()
	stats.LastTime = time.Now()

	// Pre-fetch ECH configuration
	debugf("Pre-fetching ECH configuration before starting test...")
	_, _ = fetchECHConfig("speed.cloudflare.com")

	// Pre-resolve IP addresses using DoH
	debugf("Pre-resolving IP addresses via DoH before starting test...")
	_, err := resolveHostnameViaDoH("speed.cloudflare.com")
	if err != nil {
		logger.Printf("Warning: Failed to resolve IPs via DoH: %v (will use system DNS as fallback)", err)
	}

	if config.Down {
		performDownloadTest()
	} else {
		performUploadTest()
	}
}

func showHelp() {
	fmt.Println("Speed Test Tool")
	fmt.Println("")
	fmt.Println("Usage:")
	fmt.Println("  -h, --help       Show this help message")
	fmt.Println("  -d, --down       Perform download speed test (mutually exclusive with -u)")
	fmt.Println("  -u, --up         Perform upload speed test (mutually exclusive with -d)")
	fmt.Println("  -t, --time       Test duration in seconds (default: continuous)")
	fmt.Println("  -s, --size       Test size in GiB (default: 0.1)")
	fmt.Println("  -P, --parallel   Number of parallel connections (default: 4)")
	fmt.Println("  -I, --interface  Network interface to use")
	fmt.Println("  --fwmark         Firewall mark")
	fmt.Println("  --log            Error log file path (default: errout.log)")
	fmt.Println("  -4               Use IPv4 only")
	fmt.Println("  -6               Use IPv6 only")
	fmt.Println("  --debug          Enable debug logging")
}

func setupLogger() {
	// Clear the log file if it exists, or create it if it doesn't
	logFile, err := os.OpenFile(config.LogFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0666)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening log file: %v\n", err)
		os.Exit(1)
	}
	logger = log.New(logFile, "", log.LstdFlags)
}

// debugf logs debug information only when debug mode is enabled
func debugf(format string, args ...interface{}) {
	if config.Debug {
		logger.Printf("[DEBUG] "+format, args...)
	}
}

func setupSignalHandler() {
	ctx, cancel = context.WithCancel(context.Background())
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-c
		logger.Println("Received signal, shutting down gracefully...")
		stats.EndTime = time.Now()
		cancel()

		time.Sleep(100 * time.Millisecond)
	}()

	if config.TestTime > 0 {
		go func() {
			time.Sleep(time.Duration(config.TestTime) * time.Second)
			stats.EndTime = time.Now()
			cancel()
		}()
	}
}

// happyEyeballsDialContext implements Happy Eyeballs algorithm
func happyEyeballsDialContext(ctx context.Context, dialer *net.Dialer, network, addr string) (net.Conn, error) {
	// Check interface binding IP version restrictions
	var canUseIPv4, canUseIPv6 bool = true, true

	if dialer.LocalAddr != nil {
		if tcpAddr, ok := dialer.LocalAddr.(*net.TCPAddr); ok {
			if tcpAddr.IP.To4() != nil {
				canUseIPv6 = false
				debugf("Happy Eyeballs: Interface bound to IPv4 %s, IPv6 disabled", tcpAddr.IP)
			} else {
				canUseIPv4 = false
				debugf("Happy Eyeballs: Interface bound to IPv6 %s, IPv4 disabled", tcpAddr.IP)
			}
		}
	}

	// Parse host and port
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}

	// Resolve addresses
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// Use DoH-resolved IPs if cached
	var ips []net.IPAddr
	resolvedIPsMu.RLock()
	cachedIPs, hasCached := resolvedIPs[host]
	resolvedIPsMu.RUnlock()

	if hasCached {
		debugf("Happy Eyeballs: Using DoH-resolved IPs for %s", host)
		for _, ip := range cachedIPs {
			ips = append(ips, net.IPAddr{IP: ip})
		}
	} else {
		debugf("Happy Eyeballs: Using system DNS for %s (DoH cache miss)", host)
		var lookupErr error
		ips, lookupErr = net.DefaultResolver.LookupIPAddr(ctx, host)
		if lookupErr != nil {
			return nil, lookupErr
		}
	}

	var ipv4Addrs, ipv6Addrs []net.IP
	for _, ip := range ips {
		if ip.IP.To4() != nil && canUseIPv4 {
			ipv4Addrs = append(ipv4Addrs, ip.IP)
		} else if ip.IP.To4() == nil && canUseIPv6 {
			ipv6Addrs = append(ipv6Addrs, ip.IP)
		}
	}

	debugf("Happy Eyeballs: found %d usable IPv4 and %d usable IPv6 addresses for %s",
		len(ipv4Addrs), len(ipv6Addrs), host)

	if len(ipv6Addrs) == 0 && len(ipv4Addrs) > 0 {
		debugf("Happy Eyeballs: using IPv4 only")
		return dialer.DialContext(ctx, "tcp4", net.JoinHostPort(ipv4Addrs[0].String(), port))
	}
	if len(ipv4Addrs) == 0 && len(ipv6Addrs) > 0 {
		debugf("Happy Eyeballs: using IPv6 only")
		return dialer.DialContext(ctx, "tcp6", net.JoinHostPort(ipv6Addrs[0].String(), port))
	}

	if len(ipv4Addrs) > 0 && len(ipv6Addrs) > 0 {
		return raceConnections(ctx, dialer, ipv4Addrs[0], ipv6Addrs[0], port)
	}

	return nil, fmt.Errorf("no usable addresses found for %s (IPv4 allowed: %v, IPv6 allowed: %v)",
		host, canUseIPv4, canUseIPv6)
}

// raceConnections implements the Happy Eyeballs connection racing
func raceConnections(ctx context.Context, dialer *net.Dialer, ipv4, ipv6 net.IP, port string) (net.Conn, error) {
	debugf("Happy Eyeballs: racing IPv6 %s vs IPv4 %s", ipv6, ipv4)

	type connResult struct {
		conn net.Conn
		err  error
		ipv6 bool
	}

	results := make(chan connResult, 2)

	// Start IPv6 connection
	go func() {
		addr := net.JoinHostPort(ipv6.String(), port)
		conn, err := dialer.DialContext(ctx, "tcp6", addr)
		results <- connResult{conn: conn, err: err, ipv6: true}
	}()

	// Start IPv4 connection after 250ms delay
	go func() {
		select {
		case <-ctx.Done():
			results <- connResult{err: ctx.Err(), ipv6: false}
			return
		case <-time.After(250 * time.Millisecond):
		}

		addr := net.JoinHostPort(ipv4.String(), port)
		conn, err := dialer.DialContext(ctx, "tcp4", addr)
		results <- connResult{conn: conn, err: err, ipv6: false}
	}()

	// Wait for first successful connection
	var lastErr error
	for i := 0; i < 2; i++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case result := <-results:
			if result.err == nil {
				protocol := "IPv4"
				if result.ipv6 {
					protocol = "IPv6"
				}
				debugf("Happy Eyeballs: %s connection succeeded first", protocol)

				// Close remaining connections
				go func() {
					for j := i + 1; j < 2; j++ {
						if remaining := <-results; remaining.conn != nil {
							remaining.conn.Close()
						}
					}
				}()

				return result.conn, nil
			}
			lastErr = result.err
			debugf("Happy Eyeballs: connection failed: %v", result.err)
		}
	}

	return nil, fmt.Errorf("all connection attempts failed, last error: %v", lastErr)
}

// forcedIPVersionDialContext enforces specific IP version
func forcedIPVersionDialContext(ctx context.Context, dialer *net.Dialer, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// Use DoH-resolved IPs if cached
	var ips []net.IPAddr
	resolvedIPsMu.RLock()
	cachedIPs, hasCached := resolvedIPs[host]
	resolvedIPsMu.RUnlock()

	if hasCached {
		debugf("Forced IP version: Using DoH-resolved IPs for %s", host)
		for _, ip := range cachedIPs {
			ips = append(ips, net.IPAddr{IP: ip})
		}
	} else {
		debugf("Forced IP version: Using system DNS for %s (DoH cache miss)", host)
		var lookupErr error
		ips, lookupErr = net.DefaultResolver.LookupIPAddr(ctx, host)
		if lookupErr != nil {
			return nil, fmt.Errorf("DNS resolution failed for %s: %v", host, lookupErr)
		}
	}

	var targetIP net.IP
	var targetNetwork string

	if config.IPv4Only {
		for _, ip := range ips {
			if ip.IP.To4() != nil {
				targetIP = ip.IP
				targetNetwork = "tcp4"
				break
			}
		}
		if targetIP == nil {
			logger.Printf("FATAL: IPv4-only mode requested but no IPv4 addresses found for %s", host)
			return nil, fmt.Errorf("IPv4-only mode: no IPv4 addresses available for %s", host)
		}
		debugf("IPv4-only: using %s", targetIP)
	} else if config.IPv6Only {
		for _, ip := range ips {
			if ip.IP.To4() == nil {
				targetIP = ip.IP
				targetNetwork = "tcp6"
				break
			}
		}
		if targetIP == nil {
			logger.Printf("FATAL: IPv6-only mode requested but no IPv6 addresses found for %s", host)
			return nil, fmt.Errorf("IPv6-only mode: no IPv6 addresses available for %s", host)
		}
		debugf("IPv6-only: using %s", targetIP)
	}

	// Connect to specific IP
	targetAddr := net.JoinHostPort(targetIP.String(), port)
	return dialer.DialContext(ctx, targetNetwork, targetAddr)
}

// macOS-specific constants and functions
const (
	// macOS socket options
	IP_BOUND_IF   = 25  // Bind socket to specific interface (IPv4)
	IPV6_BOUND_IF = 125 // Bind socket to specific interface (IPv6)
)

// if_nametoindex converts interface name to index (macOS/BSD)
func if_nametoindex(name string) (uint32, error) {
	ief, err := net.InterfaceByName(name)
	if err != nil {
		return 0, err
	}
	return uint32(ief.Index), nil
}

// bindToInterfaceLinux binds socket to interface using SO_BINDTODEVICE (Linux)
func bindToInterfaceLinux(fd int, interfaceName string) error {
	const SO_BINDTODEVICE = 25
	return syscall.SetsockoptString(fd, syscall.SOL_SOCKET, SO_BINDTODEVICE, interfaceName)
}

// bindToInterfaceDarwin binds socket to interface using IP_BOUND_IF/IPV6_BOUND_IF (macOS)
func bindToInterfaceDarwin(fd int, interfaceName string, isIPv6 bool) error {
	ifIndex, err := if_nametoindex(interfaceName)
	if err != nil {
		return fmt.Errorf("failed to get interface index for %s: %v", interfaceName, err)
	}

	// Convert uint32 to byte slice for setsockopt
	indexBytes := (*[4]byte)(unsafe.Pointer(&ifIndex))[:]

	if isIPv6 {
		// IPv6: use IPV6_BOUND_IF
		_, _, errno := syscall.Syscall6(syscall.SYS_SETSOCKOPT,
			uintptr(fd),
			uintptr(syscall.IPPROTO_IPV6),
			uintptr(IPV6_BOUND_IF),
			uintptr(unsafe.Pointer(&indexBytes[0])),
			uintptr(len(indexBytes)),
			0)
		if errno != 0 {
			return fmt.Errorf("setsockopt IPV6_BOUND_IF failed: %v", errno)
		}
	} else {
		// IPv4: use IP_BOUND_IF
		_, _, errno := syscall.Syscall6(syscall.SYS_SETSOCKOPT,
			uintptr(fd),
			uintptr(syscall.IPPROTO_IP),
			uintptr(IP_BOUND_IF),
			uintptr(unsafe.Pointer(&indexBytes[0])),
			uintptr(len(indexBytes)),
			0)
		if errno != 0 {
			return fmt.Errorf("setsockopt IP_BOUND_IF failed: %v", errno)
		}
	}

	return nil
}

// createInterfaceBoundDialer creates a dialer bound to a specific interface
func createInterfaceBoundDialer() (*net.Dialer, error) {
	if config.Interface == "" {
		return &net.Dialer{Timeout: 5 * time.Second}, nil
	}

	ief, err := net.InterfaceByName(config.Interface)
	if err != nil {
		return nil, fmt.Errorf("interface %s not found: %v", config.Interface, err)
	}

	debugf("Interface %s details: Index=%d, MTU=%d, HardwareAddr=%s, Flags=%s",
		config.Interface, ief.Index, ief.MTU, ief.HardwareAddr, ief.Flags)

	addrs, err := ief.Addrs()
	if err != nil {
		return nil, fmt.Errorf("failed to get addresses for interface %s: %v", config.Interface, err)
	}

	debugf("Interface %s has %d addresses:", config.Interface, len(addrs))
	for i, addr := range addrs {
		debugf("  Address %d: %s", i, addr.String())
	}

	// Check available IP versions
	var hasIPv4, hasIPv6 bool
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				hasIPv4 = true
				debugf("Found IPv4 address: %s", ipnet.IP.String())
			} else if ipnet.IP.To16() != nil {
				hasIPv6 = true
				debugf("Found IPv6 address: %s", ipnet.IP.String())
			}
		}
	}

	// Validate IP version availability
	if config.IPv4Only && !hasIPv4 {
		return nil, fmt.Errorf("IPv4-only mode requested but no IPv4 address found on interface %s", config.Interface)
	}
	if config.IPv6Only && !hasIPv6 {
		return nil, fmt.Errorf("IPv6-only mode requested but no IPv6 address found on interface %s", config.Interface)
	}

	debugf("Using OS-specific interface binding for %s (OS: %s)", config.Interface, runtime.GOOS)

	dialer := &net.Dialer{
		Timeout: 5 * time.Second,
		Control: func(network, address string, c syscall.RawConn) error {
			var operr error
			fn := func(fd uintptr) {
				intFd := int(fd)

				switch runtime.GOOS {
				case "linux":
					if err := bindToInterfaceLinux(intFd, config.Interface); err != nil {
						operr = fmt.Errorf("failed to bind to interface %s on Linux: %v", config.Interface, err)
						logger.Printf("Linux interface binding failed: %v", operr)
					} else {
						debugf("Linux: Successfully bound socket to interface %s using SO_BINDTODEVICE", config.Interface)
					}

				case "darwin":
					isIPv6 := network == "tcp6" || network == "udp6" || strings.HasSuffix(network, "6")
					if err := bindToInterfaceDarwin(intFd, config.Interface, isIPv6); err != nil {
						operr = fmt.Errorf("failed to bind to interface %s on macOS: %v", config.Interface, err)
						logger.Printf("macOS interface binding failed for %s: %v", network, operr)
					} else {
						protocol := "IPv4"
						if isIPv6 {
							protocol = "IPv6"
						}
						debugf("macOS: Successfully bound %s socket (%s) to interface %s (index %d)",
							protocol, network, config.Interface, ief.Index)
					}

				default:
					debugf("Warning: Unsupported OS %s, trying Linux-style binding", runtime.GOOS)
					if err := bindToInterfaceLinux(intFd, config.Interface); err != nil {
						operr = fmt.Errorf("failed to bind to interface %s on %s: %v", config.Interface, runtime.GOOS, err)
						logger.Printf("Fallback interface binding failed: %v", operr)
					} else {
						debugf("Fallback: Successfully bound socket to interface %s", config.Interface)
					}
				}
			}

			if err := c.Control(fn); err != nil {
				return err
			}
			return operr
		},
	}

	return dialer, nil
}

// testReachability tests if the bound interface can reach the target server
func testReachability(dialer *net.Dialer) {
	debugf("Testing reachability using interface-bound dialer to speed.cloudflare.com...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Test both IPv4 and IPv6 connectivity
	targets := []string{
		"speed.cloudflare.com:443",   // Let Go choose IPv4 or IPv6
		"1.1.1.1:443",                // Force IPv4
		"[2606:4700:4700::1111]:443", // Force IPv6
	}

	success := false
	for _, target := range targets {
		debugf("Testing connectivity to %s...", target)
		conn, err := dialer.DialContext(ctx, "tcp", target)
		if err != nil {
			debugf("Failed to connect to %s: %v", target, err)
			continue
		}
		defer conn.Close()

		debugf("SUCCESS: Connected to %s", target)
		debugf("Local endpoint: %s -> Remote endpoint: %s", conn.LocalAddr(), conn.RemoteAddr())
		success = true
		break
	}

	if !success {
		logger.Printf("REACHABILITY TEST FAILED: Cannot reach any target from interface")
		logger.Printf("This suggests a routing or network configuration issue with the specified interface")
	} else {
		debugf("REACHABILITY TEST PASSED: Interface can reach external servers")
	}
}

// DoHResponse represents DNS-over-HTTPS JSON response
type DoHResponse struct {
	Status int `json:"Status"`
	Answer []struct {
		Name string `json:"name"`
		Type int    `json:"type"`
		TTL  int    `json:"TTL"`
		Data string `json:"data"`
	} `json:"Answer"`
}

// resolvedIPs stores resolved IP addresses to avoid repeated DNS queries
var (
	resolvedIPsMu sync.RWMutex
	resolvedIPs   = make(map[string][]net.IP)
)

// resolveHostnameViaDoH resolves hostname using DNS-over-HTTPS
func resolveHostnameViaDoH(hostname string) ([]net.IP, error) {
	// Check cache
	resolvedIPsMu.RLock()
	if ips, ok := resolvedIPs[hostname]; ok {
		resolvedIPsMu.RUnlock()
		debugf("DoH DNS: Using cached IPs for %s (%d addresses)", hostname, len(ips))
		return ips, nil
	}
	resolvedIPsMu.RUnlock()

	debugf("DoH DNS: Resolving %s via DNS-over-HTTPS", hostname)

	var allIPs []net.IP

	// Query A record (IPv4)
	ipv4s, err := queryDoHRecord(hostname, 1)
	if err == nil {
		for _, ip := range ipv4s {
			debugf("DoH DNS: Resolved %s -> %s (IPv4)", hostname, ip)
			allIPs = append(allIPs, ip)
		}
	} else {
		debugf("DoH DNS: Failed to resolve A record for %s: %v", hostname, err)
	}

	// Query AAAA record (IPv6)
	ipv6s, err := queryDoHRecord(hostname, 28)
	if err == nil {
		for _, ip := range ipv6s {
			debugf("DoH DNS: Resolved %s -> %s (IPv6)", hostname, ip)
			allIPs = append(allIPs, ip)
		}
	} else {
		debugf("DoH DNS: Failed to resolve AAAA record for %s: %v", hostname, err)
	}

	if len(allIPs) == 0 {
		return nil, fmt.Errorf("no IP addresses found for %s", hostname)
	}

	// Cache results
	resolvedIPsMu.Lock()
	resolvedIPs[hostname] = allIPs
	resolvedIPsMu.Unlock()

	return allIPs, nil
}

// queryDoHRecord queries a specific DNS record type via DoH
func queryDoHRecord(hostname string, recordType int) ([]net.IP, error) {
	dohURL := "https://cloudflare-dns.com/dns-query"
	url := fmt.Sprintf("%s?name=%s&type=%d", dohURL, hostname, recordType)

	client := &http.Client{
		Timeout: 5 * time.Second,
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/dns-json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("DoH returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var dohResp DoHResponse
	if err := json.Unmarshal(body, &dohResp); err != nil {
		return nil, err
	}

	if dohResp.Status != 0 {
		return nil, fmt.Errorf("DNS query failed with status %d", dohResp.Status)
	}

	var ips []net.IP
	for _, answer := range dohResp.Answer {
		if answer.Type == recordType {
			ip := net.ParseIP(answer.Data)
			if ip != nil {
				ips = append(ips, ip)
			}
		}
	}

	return ips, nil
}

// fetchECHConfig fetches ECH configuration via DoH
func fetchECHConfig(hostname string) ([]byte, error) {
	// Check cache
	echConfigMu.Lock()
	if echFetched {
		cfg := echConfig
		echConfigMu.Unlock()
		if len(cfg) > 0 {
			debugf("ECH: Using cached config (%d bytes)", len(cfg))
		}
		return cfg, nil
	}
	echConfigMu.Unlock()

	dohProviders := []string{
		"https://cloudflare-dns.com/dns-query",
	}

	echHostname := hostname
	if hostname == "speed.cloudflare.com" {
		echHostname = "cloudflare-ech.com"
		debugf("ECH: Using cloudflare-ech.com for %s", hostname)
	}

	// Fetch ECH config from DoH providers
	var lastErr error
	for _, provider := range dohProviders {
		debugf("ECH: Trying DoH provider %s for %s", provider, echHostname)
		cfg, err := fetchECHFromDoH(provider, echHostname)
		if err == nil && len(cfg) > 0 {
			debugf("ECH: Successfully fetched config from %s (%d bytes)", provider, len(cfg))
			echConfigMu.Lock()
			echConfig = cfg
			echFetched = true
			echConfigMu.Unlock()
			return cfg, nil
		}
		debugf("ECH: Failed to fetch from %s: %v", provider, err)
		lastErr = err
	}

	// Mark as fetched
	echConfigMu.Lock()
	echFetched = true
	echConfigMu.Unlock()

	return nil, fmt.Errorf("all DoH providers failed, last error: %v", lastErr)
}

// fetchECHFromDoH queries DoH provider for ECH configuration
func fetchECHFromDoH(dohURL, hostname string) ([]byte, error) {
	url := fmt.Sprintf("%s?name=%s&type=65", dohURL, hostname)

	client := &http.Client{
		Timeout: 5 * time.Second,
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Accept", "application/dns-json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("DoH request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("DoH returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var dohResp DoHResponse
	if err := json.Unmarshal(body, &dohResp); err != nil {
		return nil, fmt.Errorf("parse JSON: %w", err)
	}

	if dohResp.Status != 0 {
		return nil, fmt.Errorf("DNS query failed with status %d", dohResp.Status)
	}

	if len(dohResp.Answer) == 0 {
		return nil, fmt.Errorf("no HTTPS records found")
	}

	// Parse ECH from HTTPS record
	for _, answer := range dohResp.Answer {
		if answer.Type == 65 { // HTTPS record
			return parseECHFromHTTPSRecord(answer.Data)
		}
	}

	return nil, fmt.Errorf("no ECH config in HTTPS records")
}

// parseECHFromHTTPSRecord extracts ECH config from HTTPS record data
func parseECHFromHTTPSRecord(data string) ([]byte, error) {
	// DoH providers return HTTPS records in different formats:
	// 1. RFC 3597 format: "\\# <length> <hexdata>" (Cloudflare, Google)
	// 2. Text format: "priority target [ech=<base64>]" (some providers)

	// Handle RFC 3597 hex format
	if strings.HasPrefix(data, "\\#") || strings.HasPrefix(data, "\\\\#") {
		// Format: "\\# <length> <hexdata>"
		data = strings.TrimPrefix(data, "\\\\")
		data = strings.TrimPrefix(data, "\\")
		parts := strings.Fields(data)
		if len(parts) >= 3 && parts[0] == "#" {
			// Join hex bytes
			hexStr := strings.Join(parts[2:], "")
			hexBytes, err := hexDecode(hexStr)
			if err != nil {
				return nil, fmt.Errorf("decode hex: %w", err)
			}
			// Parse HTTPS record wire format to extract ECH
			return parseHTTPSWireFormat(hexBytes)
		}
	}

	// Handle text format: try to find ech= parameter
	parts := strings.Fields(data)
	for _, part := range parts {
		if strings.HasPrefix(part, "ech=") {
			echB64 := strings.TrimPrefix(part, "ech=")
			echB64 = strings.Trim(echB64, "\"")
			decoded, err := base64.StdEncoding.DecodeString(echB64)
			if err == nil && len(decoded) > 0 {
				return decoded, nil
			}
		}
	}

	return nil, fmt.Errorf("no ECH parameter found in HTTPS record")
}

// hexDecode converts hex string to bytes
func hexDecode(s string) ([]byte, error) {
	s = strings.ReplaceAll(s, " ", "")
	if len(s)%2 != 0 {
		return nil, fmt.Errorf("odd length hex string")
	}
	result := make([]byte, len(s)/2)
	for i := 0; i < len(s); i += 2 {
		var b byte
		_, err := fmt.Sscanf(s[i:i+2], "%02x", &b)
		if err != nil {
			return nil, err
		}
		result[i/2] = b
	}
	return result, nil
}

// parseHTTPSWireFormat parses HTTPS record wire format to extract ECH config
func parseHTTPSWireFormat(data []byte) ([]byte, error) {
	// HTTPS record format (RFC 9460):
	// - 2 bytes: SvcPriority
	// - variable: TargetName
	// - variable: SvcParams (key-value pairs)
	//   - ECH param key = 5

	if len(data) < 3 {
		return nil, fmt.Errorf("HTTPS record too short")
	}

	// Skip priority (2 bytes)
	offset := 2

	// Parse target name (DNS name format)
	for offset < len(data) {
		labelLen := int(data[offset])
		offset++
		if labelLen == 0 {
			break // End of name
		}
		if offset+labelLen > len(data) {
			return nil, fmt.Errorf("invalid target name")
		}
		offset += labelLen
	}

	// Parse SvcParams
	for offset < len(data) {
		if offset+4 > len(data) {
			break
		}

		// Read key (2 bytes)
		key := uint16(data[offset])<<8 | uint16(data[offset+1])
		offset += 2

		// Read value length (2 bytes)
		valueLen := int(data[offset])<<8 | int(data[offset+1])
		offset += 2

		if offset+valueLen > len(data) {
			return nil, fmt.Errorf("invalid SvcParam value length")
		}

		// Check if this is ECH param (key = 5)
		if key == 5 {
			echConfig := make([]byte, valueLen)
			copy(echConfig, data[offset:offset+valueLen])
			debugf("ECH: Found ECH config in HTTPS record (key=5, %d bytes)", valueLen)
			return echConfig, nil
		}

		offset += valueLen
	}

	return nil, fmt.Errorf("no ECH param (key=5) found in HTTPS record")
}

// tlsVersionName returns the name of the TLS version
func tlsVersionName(version uint16) string {
	switch version {
	case tls.VersionTLS10:
		return "1.0"
	case tls.VersionTLS11:
		return "1.1"
	case tls.VersionTLS12:
		return "1.2"
	case tls.VersionTLS13:
		return "1.3"
	default:
		return fmt.Sprintf("0x%04x", version)
	}
}

func createHTTPClient() *http.Client {
	dialer, err := createInterfaceBoundDialer()
	if err != nil {
		logger.Printf("Unexpected interface binding error: %v", err)
		fmt.Fprintf(os.Stderr, "Error: Failed to bind to interface: %v\n", err)
		os.Exit(1)
	}

	if config.Interface == "" && (config.IPv4Only || config.IPv6Only) {
		debugf("Applying IP version restriction: IPv4Only=%v, IPv6Only=%v", config.IPv4Only, config.IPv6Only)
	}

	if config.Interface != "" {
		debugf("Interface binding: using interface %s with OS-specific binding (%s)", config.Interface, runtime.GOOS)
		testReachability(dialer)
	}

	transport := &http.Transport{
		MaxIdleConns:        10,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     30 * time.Second,
		DisableKeepAlives:   false,
	}

	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if config.IPv4Only || config.IPv6Only {
			return forcedIPVersionDialContext(ctx, dialer, network, addr)
		} else {
			return happyEyeballsDialContext(ctx, dialer, network, addr)
		}
	}

	transport.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		// Establish raw connection
		var raw net.Conn
		var err error

		if config.IPv4Only || config.IPv6Only {
			raw, err = forcedIPVersionDialContext(ctx, dialer, network, addr)
		} else {
			raw, err = happyEyeballsDialContext(ctx, dialer, network, addr)
		}

		if err != nil {
			return nil, err
		}

		// Perform TLS handshake with ECH
		tlsConfig := &tls.Config{
			ServerName: "speed.cloudflare.com",
			MinVersion: tls.VersionTLS13,
		}

		// Fetch and enable ECH configuration if available
		echConfig, echErr := fetchECHConfig("speed.cloudflare.com")
		if echErr == nil && len(echConfig) > 0 {
			tlsConfig.EncryptedClientHelloConfigList = echConfig
			debugf("ECH: Enabled with config (%d bytes)", len(echConfig))
		} else {
			debugf("ECH: Not available, using standard TLS 1.3 (%v)", echErr)
		}

		conn := tls.Client(raw, tlsConfig)
		if err = conn.Handshake(); err != nil {
			raw.Close()
			return nil, fmt.Errorf("TLS handshake failed: %v", err)
		}

		// Check ECH status
		state := conn.ConnectionState()
		if len(echConfig) > 0 {
			if state.ECHAccepted {
				debugf("ECH: Status = ACCEPTED ✓")
				stats.mu.Lock()
				stats.ECHAccepted = true
				stats.mu.Unlock()
			} else {
				debugf("ECH: Status = REJECTED")
			}
		}

		debugf("TLS handshake successful (TLS %s)", tlsVersionName(state.Version))
		return conn, nil
	}

	return &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}
}

func performDownloadTest() {
	var wg sync.WaitGroup
	var displayWg sync.WaitGroup

	displayWg.Add(1)
	go func() {
		defer displayWg.Done()
		displayProgress()
	}()

	for i := 0; i < config.Parallel; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			downloadWorker(workerID)
		}(i)
	}

	wg.Wait()
	if stats.EndTime.IsZero() {
		stats.EndTime = time.Now()
		cancel() // Signal display routine to stop
	}

	displayWg.Wait()

	printFinalStats()
}

func downloadWorker(workerID int) {
	client := createHTTPClient()

	downloadSize := int64(config.Size * 1024 * 1024 * 1024)

	for {
		select {
		case <-ctx.Done():
			return
		default:
			url := fmt.Sprintf("https://speed.cloudflare.com/__down?bytes=%d", downloadSize)

			resp, err := client.Get(url)
			if err != nil {
				logger.Printf("Worker %d: Download error: %v", workerID, err)
				time.Sleep(100 * time.Millisecond)
				continue
			}

			writer := &statsWriter{}
			bytes, err := io.Copy(writer, resp.Body)
			resp.Body.Close()

			if err != nil {
				logger.Printf("Worker %d: Read error: %v", workerID, err)
				time.Sleep(100 * time.Millisecond)
				continue
			}

			if bytes > writer.written {
				stats.AddBytes(bytes - writer.written)
			}
		}
	}
}

// Custom writer that updates stats as data is written
type statsWriter struct {
	written int64
}

func (w *statsWriter) Write(p []byte) (n int, err error) {
	n = len(p)
	w.written += int64(n)
	stats.AddBytes(int64(n))
	return n, nil
}

func performUploadTest() {
	var wg sync.WaitGroup
	var displayWg sync.WaitGroup

	displayWg.Add(1)
	go func() {
		defer displayWg.Done()
		displayProgress()
	}()

	for i := 0; i < config.Parallel; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			uploadWorker(workerID)
		}(i)
	}

	wg.Wait()
	if stats.EndTime.IsZero() {
		stats.EndTime = time.Now()
		cancel() // Signal display routine to stop
	}

	displayWg.Wait()

	printFinalStats()
}

func uploadWorker(workerID int) {
	client := createHTTPClient()

	uploadSize := int64(config.Size * 1024 * 1024 * 1024)

	for {
		select {
		case <-ctx.Done():
			return
		default:
			data := make([]byte, uploadSize)

			reader := &statsReader{
				reader: bytes.NewReader(data),
				size:   uploadSize,
			}

			resp, err := client.Post("https://speed.cloudflare.com/__up", "application/octet-stream", reader)
			if err != nil {
				logger.Printf("Worker %d: Upload error: %v", workerID, err)
				time.Sleep(100 * time.Millisecond)
				continue
			}

			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}
}

// Custom reader that updates stats during upload
type statsReader struct {
	reader io.Reader
	size   int64
	read   int64
}

func (r *statsReader) Read(p []byte) (n int, err error) {
	n, err = r.reader.Read(p)
	if n > 0 {
		r.read += int64(n)
		stats.AddBytes(int64(n))
	}
	return n, err
}

func displayProgress() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	firstOutput := true

	for {
		select {
		case <-ctx.Done():
			if !firstOutput {
				fmt.Print("\r\033[K\033[1A\033[K\033[1A\033[K\033[1A\033[K")
			}
			return
		case <-ticker.C:
			stats.UpdateCurrentSpeed()

			currentSpeed := stats.GetCurrentSpeed()
			totalBytes := stats.GetTotalBytes()
			elapsed := time.Since(stats.StartTime)

			if !firstOutput {
				fmt.Print("\033[3A\033[J")
			}
			firstOutput = false

			fmt.Printf("Speed: %s\n", formatSpeed(currentSpeed))
			fmt.Printf("Elapsed Time: %s\n", formatDuration(elapsed))
			fmt.Printf("Data Used: %s\n", formatBytes(totalBytes))

			if totalBytes > 0 {
				debugf("Total bytes: %d, Speed: %.2f B/s", totalBytes, currentSpeed)
			}
		}
	}
}

func formatSpeed(bytesPerSec float64) string {
	units := []string{"bps", "Kbps", "Mbps", "Gbps"}
	bitsPerSec := bytesPerSec * 8
	size := bitsPerSec
	unitIndex := 0

	for size >= 1000 && unitIndex < len(units)-1 {
		size /= 1000
		unitIndex++
	}

	return fmt.Sprintf("%.2f %s", size, units[unitIndex])
}

func formatBytes(bytes int64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	size := float64(bytes)
	unitIndex := 0

	for size >= 1024 && unitIndex < len(units)-1 {
		size /= 1024
		unitIndex++
	}

	return fmt.Sprintf("%.2f %s", size, units[unitIndex])
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.0fs", d.Seconds())
	}

	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	minutes := int(d.Minutes()) % 60
	seconds := int(d.Seconds()) % 60

	var parts []string

	if days > 0 {
		parts = append(parts, fmt.Sprintf("%dd", days))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if minutes > 0 {
		parts = append(parts, fmt.Sprintf("%dm", minutes))
	}
	if seconds > 0 {
		parts = append(parts, fmt.Sprintf("%ds", seconds))
	}

	return strings.Join(parts, " ")
}

func printFinalStats() {
	fmt.Print("\r\033[K\033[J")

	totalDuration := stats.EndTime.Sub(stats.StartTime)
	totalBytes := stats.GetTotalBytes()
	averageSpeed := float64(totalBytes) / totalDuration.Seconds()

	stats.mu.RLock()
	echStatus := stats.ECHAccepted
	stats.mu.RUnlock()

	fmt.Printf("Test Start Time: %s\n", stats.StartTime.Format("2006-01-02 15:04:05"))
	fmt.Printf("Test End Time: %s\n", stats.EndTime.Format("2006-01-02 15:04:05"))
	fmt.Printf("Total Test Duration: %s\n", formatDuration(totalDuration))
	fmt.Printf("Total Data Used: %s\n", formatBytes(totalBytes))
	fmt.Printf("Average Speed: %s\n", formatSpeed(averageSpeed))
	fmt.Printf("Maximum Speed: %s\n", formatSpeed(stats.GetMaxSpeed()))
	fmt.Printf("ECH: %v\n", echStatus)
}
