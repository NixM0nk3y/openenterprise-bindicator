package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"golang.org/x/term"
)

const (
	defaultPort    = "23"
	otaPort        = "4242"
	defaultTimeout = 10 * time.Second
	readTimeout    = 5 * time.Second
	otaChunkSize   = 4096 // 4KB chunks for OTA
)

func main() {
	// Load .env file before parsing flags
	loadEnvFile()

	// Parse flags
	host := flag.String("host", "", "Device IP address (required)")
	port := flag.String("port", defaultPort, "Device port")
	cmd := flag.String("cmd", "", "Single command to execute (interactive mode if empty)")
	password := flag.String("password", "", "Console password (or use BINDICATOR_PASSWORD env var)")
	flag.Parse()

	if *host == "" {
		// Check for positional argument
		if flag.NArg() > 0 {
			*host = flag.Arg(0)
		} else {
			printUsage()
			os.Exit(1)
		}
	}

	// Check for command as second positional arg
	if *cmd == "" && flag.NArg() > 1 {
		*cmd = flag.Arg(1)
	}

	// Resolve password early for OTA commands that need console access
	pass := getPassword(*password)

	// Handle OTA commands specially
	if *cmd == "ota-push" || (flag.NArg() > 1 && flag.Arg(1) == "ota-push") {
		// Get firmware file path
		var fwPath string
		if flag.NArg() > 2 {
			fwPath = flag.Arg(2)
		} else {
			fmt.Println("Usage: bindicator-cli <ip> ota-push <firmware.uf2>")
			os.Exit(1)
		}
		if err := otaPush(*host, fwPath, pass); err != nil {
			fmt.Fprintf(os.Stderr, "OTA push failed: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if *cmd == "ota-info" || (flag.NArg() > 1 && flag.Arg(1) == "ota-info") {
		if err := otaInfo(*host, pass); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if *cmd == "ota-enable" || (flag.NArg() > 1 && flag.Arg(1) == "ota-enable") {
		// Get optional timeout
		var timeout string
		if flag.NArg() > 2 {
			timeout = flag.Arg(2)
		}
		if err := otaEnable(*host, timeout, pass); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// ota-file doesn't need a host, just inspect the file
	if *cmd == "ota-file" || (flag.NArg() > 0 && flag.Arg(0) == "ota-file") {
		var fwPath string
		if flag.NArg() > 1 {
			fwPath = flag.Arg(1)
		} else if flag.NArg() > 0 && flag.Arg(0) != "ota-file" {
			fwPath = flag.Arg(0)
		} else {
			fmt.Println("Usage: bindicator-cli ota-file <firmware.uf2>")
			os.Exit(1)
		}
		if err := readFirmwareInfo(fwPath); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	addr := net.JoinHostPort(*host, *port)

	if *cmd != "" {
		// Single command mode
		if err := runCommand(addr, *cmd, pass); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	} else {
		// Interactive mode
		if err := interactive(addr, pass); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	}
}

func printUsage() {
	fmt.Println("Bindicator CLI")
	fmt.Println()
	fmt.Println("Usage:")
	fmt.Println("  bindicator-cli <ip> [command]")
	fmt.Println("  bindicator-cli -host <ip> [-cmd <command>] [-password <pw>]")
	fmt.Println()
	fmt.Println("Authentication:")
	fmt.Println("  Password can be provided via:")
	fmt.Println("    -password flag")
	fmt.Println("    BINDICATOR_PASSWORD environment variable")
	fmt.Println("    .env file (BINDICATOR_PASSWORD=...)")
	fmt.Println("    Interactive prompt")
	fmt.Println()
	fmt.Println("Console Commands:")
	fmt.Println("  help, version, status, net, wifi, time, jobs, next, leds, ota")
	fmt.Println("  refresh, sleep <dur>, ota-enable [dur]")
	fmt.Println("  led-green, led-black, led-brown")
	fmt.Println()
	fmt.Println("OTA Commands:")
	fmt.Println("  ota-info                   Query device OTA status")
	fmt.Println("  ota-enable [dur]           Enable OTA server (default: 10m timeout)")
	fmt.Println("  ota-push <file.uf2>        Push firmware update (auto-enables OTA)")
	fmt.Println("  ota-file <file.uf2>        Inspect UF2 file (no device needed)")
	fmt.Println()
	fmt.Println("Examples:")
	fmt.Println("  bindicator-cli 172.18.1.136                      # Interactive mode")
	fmt.Println("  bindicator-cli 172.18.1.136 status               # Single command")
	fmt.Println("  bindicator-cli -password secret 172.18.1.136 status")
	fmt.Println("  BINDICATOR_PASSWORD=secret bindicator-cli 172.18.1.136 status")
	fmt.Println("  bindicator-cli ota-file build.uf2                # Inspect file")
}

// runCommand executes a single command and prints the response
func runCommand(addr, cmd, password string) error {
	conn, err := net.DialTimeout("tcp", addr, defaultTimeout)
	if err != nil {
		return fmt.Errorf("connect failed: %w", err)
	}
	defer conn.Close()

	// Authenticate
	if err := authenticate(conn, password); err != nil {
		return err
	}

	// Consume welcome message until we see the prompt
	consumeUntilPrompt(conn)

	// Send command
	_, err = conn.Write([]byte(cmd + "\r\n"))
	if err != nil {
		return fmt.Errorf("send failed: %w", err)
	}

	// Read response
	conn.SetReadDeadline(time.Now().Add(readTimeout))
	response := make([]byte, 4096)
	n, _ := conn.Read(response)

	// Print response (strip prompt)
	output := string(response[:n])
	output = strings.TrimSuffix(output, "> ")
	output = strings.TrimSpace(output)
	fmt.Println(output)

	return nil
}

// interactive runs an interactive session with the device
func interactive(addr, password string) error {
	fmt.Printf("Connecting to %s...\n", addr)

	conn, err := net.DialTimeout("tcp", addr, defaultTimeout)
	if err != nil {
		return fmt.Errorf("connect failed: %w", err)
	}
	defer conn.Close()

	// Authenticate
	if err := authenticate(conn, password); err != nil {
		return err
	}

	fmt.Println("Connected! Type 'quit' or Ctrl+C to exit.")
	fmt.Println()

	// Read welcome message
	conn.SetReadDeadline(time.Now().Add(readTimeout))
	welcome := make([]byte, 1024)
	n, _ := conn.Read(welcome)
	fmt.Print(string(welcome[:n]))

	// Interactive loop
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("> ")
		if !scanner.Scan() {
			break
		}

		input := strings.TrimSpace(scanner.Text())
		if input == "" {
			continue // Just show prompt again
		}

		if input == "quit" || input == "exit" {
			fmt.Println("Goodbye!")
			return nil
		}

		// Send command
		_, err = conn.Write([]byte(input + "\r\n"))
		if err != nil {
			return fmt.Errorf("send failed: %w", err)
		}

		// Read response
		conn.SetReadDeadline(time.Now().Add(readTimeout))
		response := make([]byte, 4096)
		n, err := conn.Read(response)
		if err != nil {
			// Try to reconnect
			fmt.Println("Connection lost, reconnecting...")
			conn.Close()
			conn, err = net.DialTimeout("tcp", addr, defaultTimeout)
			if err != nil {
				return fmt.Errorf("reconnect failed: %w", err)
			}
			// Re-authenticate
			if err := authenticate(conn, password); err != nil {
				return fmt.Errorf("reconnect auth failed: %w", err)
			}
			// Consume welcome
			consumeUntilPrompt(conn)
			continue
		}

		// Print response, stripping the device prompt
		output := string(response[:n])
		output = strings.TrimSuffix(output, "> ")
		output = strings.TrimSpace(output)
		if output != "" {
			fmt.Println(output)
		}
	}

	return nil
}

// otaInfo displays OTA status by querying the device console
func otaInfo(host, password string) error {
	addr := net.JoinHostPort(host, defaultPort)

	// Get OTA info from console
	fmt.Println("Querying device OTA status...")
	fmt.Println()

	conn, err := net.DialTimeout("tcp", addr, defaultTimeout)
	if err != nil {
		return fmt.Errorf("connect failed: %w", err)
	}
	defer conn.Close()

	// Authenticate
	if err := authenticate(conn, password); err != nil {
		return err
	}

	// Consume welcome message until we see the prompt
	consumeUntilPrompt(conn)

	// Send ota command
	conn.Write([]byte("ota\r\n"))

	// Read response
	conn.SetReadDeadline(time.Now().Add(readTimeout))
	response := make([]byte, 4096)
	n, _ := conn.Read(response)

	output := string(response[:n])
	output = strings.TrimSuffix(output, "> ")
	output = strings.TrimSpace(output)
	fmt.Println(output)

	return nil
}

// otaEnable enables the OTA server on the device via console command
func otaEnable(host, timeout, password string) error {
	addr := net.JoinHostPort(host, defaultPort)

	fmt.Println("Enabling OTA server...")

	conn, err := net.DialTimeout("tcp", addr, defaultTimeout)
	if err != nil {
		return fmt.Errorf("connect to console failed: %w", err)
	}
	defer conn.Close()

	// Authenticate
	if err := authenticate(conn, password); err != nil {
		return err
	}

	// Consume welcome message until we see the prompt
	consumeUntilPrompt(conn)

	// Send ota-enable command
	cmd := "ota-enable"
	if timeout != "" {
		cmd = cmd + " " + timeout
	}
	conn.Write([]byte(cmd + "\r\n"))

	// Read response
	conn.SetReadDeadline(time.Now().Add(readTimeout))
	response := make([]byte, 1024)
	n, err := conn.Read(response)
	if err != nil {
		return fmt.Errorf("no response: %w", err)
	}

	output := string(response[:n])
	output = strings.TrimSuffix(output, "> ")
	output = strings.TrimSpace(output)

	// Check for success
	if !strings.Contains(output, "enabled") && !strings.Contains(output, "ENABLED") {
		// Check if device doesn't support ota-enable (old firmware)
		if strings.Contains(output, "Unknown command") {
			return fmt.Errorf("device has old firmware without ota-enable support")
		}
		return fmt.Errorf("unexpected response: %s", output)
	}

	fmt.Println(output)
	return nil
}

// otaPush pushes a firmware update to the device
func otaPush(host, fwPath, password string) error {
	// Read firmware file
	uf2Data, err := os.ReadFile(fwPath)
	if err != nil {
		return fmt.Errorf("read firmware: %w", err)
	}

	// Validate and extract binary from UF2
	fw, err := extractUF2Binary(uf2Data)
	if err != nil {
		return fmt.Errorf("extract UF2: %w", err)
	}

	// Calculate SHA256 of extracted binary
	hash := sha256.Sum256(fw)
	fmt.Printf("Firmware: %s\n", fwPath)
	fmt.Printf("UF2 size: %d bytes\n", len(uf2Data))
	fmt.Printf("Binary size: %d bytes (%d KB)\n", len(fw), len(fw)/1024)
	fmt.Printf("SHA256: %x\n", hash[:8])
	fmt.Println()

	// Enable OTA server first (skip if device has old firmware with OTA always on)
	if err := otaEnable(host, "", password); err != nil {
		if strings.Contains(err.Error(), "old firmware") {
			fmt.Println("Note: Device has old firmware, OTA port may be always open")
			fmt.Println()
		} else {
			return fmt.Errorf("enable OTA: %w", err)
		}
	} else {
		fmt.Println()
		// Brief pause to let OTA server start listening
		time.Sleep(500 * time.Millisecond)
	}

	// Connect to OTA port
	addr := net.JoinHostPort(host, otaPort)
	fmt.Printf("Connecting to %s...\n", addr)

	conn, err := net.DialTimeout("tcp", addr, defaultTimeout)
	if err != nil {
		return fmt.Errorf("connect to OTA port failed: %w", err)
	}
	defer conn.Close()

	fmt.Println("Connected to OTA server")

	// Send OTA initiation
	conn.Write([]byte("OTA\n"))

	// Read READY response
	conn.SetReadDeadline(time.Now().Add(readTimeout))
	response := make([]byte, 256)
	n, err := conn.Read(response)
	if err != nil {
		return fmt.Errorf("no response from device: %w", err)
	}

	resp := strings.TrimSpace(string(response[:n]))
	if !strings.HasPrefix(resp, "READY") {
		return fmt.Errorf("unexpected response: %s", resp)
	}
	fmt.Printf("Device ready: %s\n", resp)

	// Send firmware in chunks
	totalChunks := (len(fw) + otaChunkSize - 1) / otaChunkSize
	fmt.Printf("Sending %d chunks...\n", totalChunks)

	for i := 0; i < len(fw); i += otaChunkSize {
		end := i + otaChunkSize
		if end > len(fw) {
			end = len(fw)
		}
		chunk := fw[i:end]

		// Send chunk: <4-byte length><data>
		lenBuf := make([]byte, 4)
		binary.LittleEndian.PutUint32(lenBuf, uint32(len(chunk)))
		conn.Write(lenBuf)
		conn.Write(chunk)

		// Wait for ACK - allow extra time for flash erase/write operations
		// Flash erase can take 400ms+ per 4KB sector
		conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		n, err := conn.Read(response)
		if err != nil {
			return fmt.Errorf("chunk %d: no ACK: %w", i/otaChunkSize+1, err)
		}

		resp := strings.TrimSpace(string(response[:n]))
		if !strings.HasPrefix(resp, "ACK") {
			return fmt.Errorf("chunk %d: bad response: %s", i/otaChunkSize+1, resp)
		}

		// Progress
		progress := (i + len(chunk)) * 100 / len(fw)
		fmt.Printf("\r[%3d%%] Chunk %d/%d", progress, i/otaChunkSize+1, totalChunks)
	}
	fmt.Println()

	// Send completion with hash
	hashHex := fmt.Sprintf("%x", hash)
	fmt.Printf("Verifying (hash: %s)...\n", hashHex)
	conn.Write([]byte(fmt.Sprintf("DONE %s\n", hashHex)))

	// Wait for VERIFIED
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	n, err = conn.Read(response)
	if err != nil {
		return fmt.Errorf("verification failed: %w", err)
	}

	resp = strings.TrimSpace(string(response[:n]))
	if resp != "VERIFIED" {
		return fmt.Errorf("verification failed: %s", resp)
	}

	fmt.Println("Firmware verified!")
	fmt.Println("Device will reboot to new partition...")

	return nil
}

// loadEnvFile loads environment variables from .env file in current directory
func loadEnvFile() {
	data, err := os.ReadFile(".env")
	if err != nil {
		return // File doesn't exist or can't be read, that's fine
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		// Remove quotes if present
		if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') ||
			(value[0] == '\'' && value[len(value)-1] == '\'')) {
			value = value[1 : len(value)-1]
		}

		// Only set if not already set in environment
		if os.Getenv(key) == "" {
			os.Setenv(key, value)
		}
	}
}

// getPassword resolves password from various sources
// Priority: flag > env > .env (already loaded) > interactive prompt
func getPassword(flagValue string) string {
	// 1. Flag has highest priority
	if flagValue != "" {
		return flagValue
	}

	// 2. Environment variable
	if envPass := os.Getenv("BINDICATOR_PASSWORD"); envPass != "" {
		return envPass
	}

	// 3. Interactive prompt (if terminal is available)
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Print("Password: ")
		password, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println() // Print newline after password
		if err == nil && len(password) > 0 {
			return string(password)
		}
	}

	return ""
}

// authenticate handles the password authentication after connecting
func authenticate(conn net.Conn, password string) error {
	// Read password prompt
	conn.SetReadDeadline(time.Now().Add(readTimeout))
	prompt := make([]byte, 64)
	n, err := conn.Read(prompt)
	if err != nil {
		return fmt.Errorf("read prompt failed: %w", err)
	}

	// Strip telnet IAC sequences from prompt
	promptStr := string(stripTelnetIAC(prompt[:n]))
	if !strings.Contains(strings.ToLower(promptStr), "password") {
		return fmt.Errorf("unexpected prompt: %s", promptStr)
	}

	// Send password
	_, err = conn.Write([]byte(password + "\r\n"))
	if err != nil {
		return fmt.Errorf("send password failed: %w", err)
	}

	return nil
}

// stripTelnetIAC removes telnet IAC (Interpret As Command) sequences from data.
// IAC = 0xFF, followed by command byte and possibly option byte.
func stripTelnetIAC(data []byte) []byte {
	result := make([]byte, 0, len(data))
	i := 0
	for i < len(data) {
		if data[i] == 0xFF && i+1 < len(data) {
			// IAC sequence - skip command and option bytes
			// WILL/WONT/DO/DONT (0xFB-0xFE) have an option byte
			cmd := data[i+1]
			if cmd >= 0xFB && cmd <= 0xFE && i+2 < len(data) {
				i += 3 // Skip IAC + command + option
			} else {
				i += 2 // Skip IAC + command
			}
		} else {
			result = append(result, data[i])
			i++
		}
	}
	return result
}

// consumeUntilPrompt reads from connection until we see "> " prompt or timeout.
// This ensures we fully consume welcome messages before sending commands.
func consumeUntilPrompt(conn net.Conn) {
	buf := make([]byte, 256)
	accumulated := ""
	deadline := time.Now().Add(readTimeout)

	for time.Now().Before(deadline) {
		conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, err := conn.Read(buf)
		if n > 0 {
			accumulated += string(stripTelnetIAC(buf[:n]))
			// Check if we've seen the prompt
			if strings.Contains(accumulated, "> ") {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// extractUF2Binary extracts the raw binary from a UF2 container file
func extractUF2Binary(uf2Data []byte) ([]byte, error) {
	if len(uf2Data) < 512 {
		return nil, fmt.Errorf("file too small to be UF2")
	}

	// Count blocks and validate
	numBlocks := len(uf2Data) / 512
	if len(uf2Data)%512 != 0 {
		return nil, fmt.Errorf("UF2 file size not multiple of 512")
	}

	// First pass: find the address range
	var minAddr, maxAddr uint32 = 0xFFFFFFFF, 0
	for i := 0; i < numBlocks; i++ {
		block := uf2Data[i*512 : (i+1)*512]

		// Check magic numbers
		magic1 := binary.LittleEndian.Uint32(block[0:4])
		magic2 := binary.LittleEndian.Uint32(block[4:8])
		magic3 := binary.LittleEndian.Uint32(block[508:512])
		if magic1 != 0x0A324655 || magic2 != 0x9E5D5157 || magic3 != 0x0AB16F30 {
			return nil, fmt.Errorf("block %d: invalid magic", i)
		}

		targetAddr := binary.LittleEndian.Uint32(block[12:16])
		payloadSize := binary.LittleEndian.Uint32(block[16:20])

		if targetAddr < minAddr {
			minAddr = targetAddr
		}
		if targetAddr+payloadSize > maxAddr {
			maxAddr = targetAddr + payloadSize
		}
	}

	// Allocate output buffer
	outputSize := maxAddr - minAddr
	if outputSize > 4*1024*1024 { // Sanity check: 4MB max
		return nil, fmt.Errorf("extracted binary too large: %d bytes", outputSize)
	}

	output := make([]byte, outputSize)

	// Second pass: copy payloads to correct offsets
	for i := 0; i < numBlocks; i++ {
		block := uf2Data[i*512 : (i+1)*512]

		targetAddr := binary.LittleEndian.Uint32(block[12:16])
		payloadSize := binary.LittleEndian.Uint32(block[16:20])

		// Payload is at offset 32, max 476 bytes (but typically 256)
		if payloadSize > 476 {
			payloadSize = 476
		}

		offset := targetAddr - minAddr
		copy(output[offset:offset+payloadSize], block[32:32+payloadSize])
	}

	return output, nil
}

// readFirmwareInfo reads and displays UF2 file information
func readFirmwareInfo(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	// Get file size
	stat, err := f.Stat()
	if err != nil {
		return err
	}
	fileSize := stat.Size()

	// Read first block to get info
	block := make([]byte, 512)
	_, err = io.ReadFull(f, block)
	if err != nil {
		return err
	}

	// UF2 block structure (512 bytes):
	// 0-3:   Magic 1 (0x0A324655 "UF2\n" little-endian)
	// 4-7:   Magic 2 (0x9E5D5157)
	// 8-11:  Flags
	// 12-15: Target address
	// 16-19: Payload size (typically 256)
	// 20-23: Block number
	// 24-27: Total blocks
	// 28-31: File size or family ID (depends on flags)
	// 32-475: Data (476 bytes max)
	// 476-479: Magic 3 (0x0AB16F30)

	magic1 := binary.LittleEndian.Uint32(block[0:4])
	magic2 := binary.LittleEndian.Uint32(block[4:8])
	flags := binary.LittleEndian.Uint32(block[8:12])
	targetAddr := binary.LittleEndian.Uint32(block[12:16])
	payloadSize := binary.LittleEndian.Uint32(block[16:20])
	_ = binary.LittleEndian.Uint32(block[20:24]) // blockNo (unused)
	numBlocks := binary.LittleEndian.Uint32(block[24:28])
	familyID := binary.LittleEndian.Uint32(block[28:32])
	magic3 := binary.LittleEndian.Uint32(block[508:512])

	// Validate magic numbers
	if magic1 != 0x0A324655 || magic2 != 0x9E5D5157 || magic3 != 0x0AB16F30 {
		return fmt.Errorf("not a valid UF2 file (bad magic)")
	}

	fmt.Printf("UF2 File: %s\n", path)
	fmt.Printf("  File size: %d bytes (%d KB)\n", fileSize, fileSize/1024)
	fmt.Printf("  Blocks: %d (block 0 shown)\n", numBlocks)
	fmt.Printf("  Target address: 0x%08x\n", targetAddr)
	fmt.Printf("  Payload per block: %d bytes\n", payloadSize)
	fmt.Printf("  Flags: 0x%08x\n", flags)

	// Decode flags
	if flags&0x00000001 != 0 {
		fmt.Printf("    - NOT_MAIN_FLASH\n")
	}
	if flags&0x00001000 != 0 {
		fmt.Printf("    - FILE_CONTAINER\n")
	}
	if flags&0x00002000 != 0 {
		fmt.Printf("    - FAMILY_ID_PRESENT\n")
	}
	if flags&0x00004000 != 0 {
		fmt.Printf("    - MD5_CHECKSUM_PRESENT\n")
	}
	if flags&0x00008000 != 0 {
		fmt.Printf("    - EXTENSION_TAGS_PRESENT\n")
	}

	// Family ID (if present)
	if flags&0x00002000 != 0 {
		fmt.Printf("  Family ID: 0x%08x", familyID)
		switch familyID {
		case 0xe48bff56:
			fmt.Printf(" (RP2040)\n")
		case 0xe48bff57:
			fmt.Printf(" (RP2350 ARM-S)\n")
		case 0xe48bff58:
			fmt.Printf(" (RP2350 ARM-NS)\n")
		case 0xe48bff59:
			fmt.Printf(" (RP2350 RISC-V)\n")
		default:
			fmt.Printf(" (unknown)\n")
		}
	}

	// Estimated firmware size
	fwSize := uint64(numBlocks) * uint64(payloadSize)
	fmt.Printf("  Firmware size: ~%d bytes (%d KB)\n", fwSize, fwSize/1024)

	return nil
}
