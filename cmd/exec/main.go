package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/gorilla/websocket"
	"golang.org/x/term"
)

// envFlags allows multiple -e or --env flags
type envFlags []string

func (e *envFlags) String() string {
	return strings.Join(*e, ",")
}

func (e *envFlags) Set(value string) error {
	*e = append(*e, value)
	return nil
}

func main() {
	// Parse flags
	var envVars envFlags
	interactive := flag.Bool("it", false, "Interactive mode with TTY (auto-detected if not set)")
	token := flag.String("token", "", "JWT token (or use HYPEMAN_TOKEN env var)")
	apiURL := flag.String("api-url", "http://localhost:8080", "API server URL")
	user := flag.String("user", "", "Username to run as")
	uid := flag.Int("uid", 0, "UID to run as (overrides --user)")
	cwd := flag.String("cwd", "", "Working directory")
	timeout := flag.Int("timeout", 0, "Execution timeout in seconds (0 = no timeout)")
	flag.Var(&envVars, "env", "Environment variable (KEY=VALUE, can be repeated)")
	flag.Var(&envVars, "e", "Environment variable (short form)")
	flag.Parse()

	// Get instance ID and optional command
	args := flag.Args()
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "Usage: %s [OPTIONS] <instance-id> [command...]\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nOptions:\n")
		flag.PrintDefaults()
		os.Exit(1)
	}

	instanceID := args[0]
	var command []string
	if len(args) > 1 {
		command = args[1:]
	}

	// Auto-detect TTY if not explicitly set
	tty := *interactive
	if !tty && flag.Lookup("it").Value.String() == "false" {
		// Flag wasn't explicitly set, auto-detect
		if term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())) {
			tty = true
		}
	}

	// Parse environment variables
	env := make(map[string]string)
	for _, e := range envVars {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			env[parts[0]] = parts[1]
		} else {
			fmt.Fprintf(os.Stderr, "Warning: ignoring malformed env var: %s\n", e)
		}
	}

	// Get JWT token
	jwtToken := *token
	if jwtToken == "" {
		jwtToken = os.Getenv("HYPEMAN_TOKEN")
	}
	if jwtToken == "" {
		fmt.Fprintf(os.Stderr, "Error: JWT token required (use --token or HYPEMAN_TOKEN env var)\n")
		os.Exit(1)
	}

	// Build request body JSON
	execReq := map[string]interface{}{
		"command": command,
		"tty":     tty,
	}
	if *user != "" {
		execReq["user"] = *user
	}
	if *uid != 0 {
		execReq["uid"] = *uid
	}
	if len(env) > 0 {
		execReq["env"] = env
	}
	if *cwd != "" {
		execReq["cwd"] = *cwd
	}
	if *timeout > 0 {
		execReq["timeout"] = *timeout
	}

	reqBody, err := json.Marshal(execReq)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to marshal request: %v\n", err)
		os.Exit(1)
	}

	// First, do HTTP POST with JSON body to initiate the request
	u, err := url.Parse(*apiURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Invalid API URL: %v\n", err)
		os.Exit(1)
	}
	u.Path = fmt.Sprintf("/instances/%s/exec", instanceID)

	// Build WebSocket URL
	wsURL := *u
	if wsURL.Scheme == "https" {
		wsURL.Scheme = "wss"
	} else if wsURL.Scheme == "http" {
		wsURL.Scheme = "ws"
	}

	// Create HTTP POST request with body to send before WebSocket handshake
	// We'll use a custom dialer that sends the body
	req, err := http.NewRequest("POST", u.String(), bytes.NewReader(reqBody))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to create request: %v\n", err)
		os.Exit(1)
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", jwtToken))
	req.Header.Set("Content-Type", "application/json")

	// Use custom WebSocket dialer
	dialer := &websocket.Dialer{}
	
	// Set up headers for WebSocket connection (body was already sent)
	headers := http.Header{}
	headers.Set("Authorization", fmt.Sprintf("Bearer %s", jwtToken))

	// Make HTTP POST with body
	client := &http.Client{}
	
	// Actually, we need a custom approach. Let me use a modified request
	// that sends body AND upgrades to WebSocket.
	// For simplicity, let's POST the JSON as the Sec-WebSocket-Protocol header value (hacky but works)
	// OR we can encode params in URL query string
	
	// Actually, the simplest approach: POST the body first, get a session ID, then connect WebSocket
	// But that requires server changes.
	
	// Let's use the approach where we send JSON as first WebSocket message after connect
	ws, resp, err := dialer.Dial(wsURL.String(), headers)
	if err != nil {
		if resp != nil {
			body, _ := io.ReadAll(resp.Body)
			fmt.Fprintf(os.Stderr, "Error: HTTP %d: %s\n", resp.StatusCode, string(body))
		} else {
			fmt.Fprintf(os.Stderr, "Error: Failed to connect: %v\n", err)
		}
		os.Exit(1)
	}
	defer ws.Close()

	// Send JSON body as first WebSocket text message
	if err := ws.WriteMessage(websocket.TextMessage, reqBody); err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to send request: %v\n", err)
		os.Exit(1)
	}

	_ = client // unused for now

	exitCode := 0
	// Handle interactive mode
	if tty {
		exitCode, err = runInteractive(ws)
	} else {
		exitCode, err = runNonInteractive(ws)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		if exitCode == 0 {
			exitCode = 255 // Transport error
		}
	}

	os.Exit(exitCode)
}

func runInteractive(ws *websocket.Conn) (int, error) {
	// Put terminal in raw mode
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return 255, fmt.Errorf("failed to set raw mode: %w", err)
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	// Handle Ctrl-C gracefully
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	// Channel for errors and exit code
	errCh := make(chan error, 2)
	exitCodeCh := make(chan int, 1)

	// Forward stdin to WebSocket
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				if err != io.EOF {
					errCh <- fmt.Errorf("stdin read error: %w", err)
				}
				return
			}
			if n > 0 {
				if err := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
					errCh <- fmt.Errorf("websocket write error: %w", err)
					return
				}
			}
		}
	}()

	// Forward WebSocket to stdout
	go func() {
		for {
			msgType, message, err := ws.ReadMessage()
			if err != nil {
				if !websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					// Normal close, default exit code 0
					exitCodeCh <- 0
				}
				return
			}

			// Check if it's a JSON exit code message
			if msgType == websocket.TextMessage && bytes.Contains(message, []byte("exitCode")) {
				var exitMsg struct {
					ExitCode int `json:"exitCode"`
				}
				if json.Unmarshal(message, &exitMsg) == nil {
					exitCodeCh <- exitMsg.ExitCode
					return
				}
			}

			// Otherwise, write to stdout
			if _, err := os.Stdout.Write(message); err != nil {
				errCh <- fmt.Errorf("stdout write error: %w", err)
				return
			}
		}
	}()

	// Wait for error, signal, exit code, or completion
	select {
	case err := <-errCh:
		return 255, err
	case exitCode := <-exitCodeCh:
		return exitCode, nil
	case <-sigCh:
		return 130, nil // 128 + SIGINT
	}
}

func runNonInteractive(ws *websocket.Conn) (int, error) {
	// Channel for errors and exit code
	errCh := make(chan error, 2)
	exitCodeCh := make(chan int, 1)
	doneCh := make(chan struct{})

	// Forward stdin to WebSocket
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				if err != io.EOF {
					errCh <- fmt.Errorf("stdin read error: %w", err)
				}
				return
			}
			if n > 0 {
				if err := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
					errCh <- fmt.Errorf("websocket write error: %w", err)
					return
				}
			}
		}
	}()

	// Forward WebSocket to stdout
	go func() {
		defer close(doneCh)
		for {
			msgType, message, err := ws.ReadMessage()
			if err != nil {
				// Connection closed is normal - default exit code 0
				if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) || 
				   err == io.EOF {
					exitCodeCh <- 0
					return
				}
				errCh <- fmt.Errorf("websocket read error: %w", err)
				return
			}

			// Check if it's a JSON exit code message
			if msgType == websocket.TextMessage && bytes.Contains(message, []byte("exitCode")) {
				var exitMsg struct {
					ExitCode int `json:"exitCode"`
				}
				if json.Unmarshal(message, &exitMsg) == nil {
					exitCodeCh <- exitMsg.ExitCode
					return
				}
			}

			// Otherwise, write to stdout
			if _, err := os.Stdout.Write(message); err != nil {
				errCh <- fmt.Errorf("stdout write error: %w", err)
				return
			}
		}
	}()

	// Wait for completion, exit code, or error
	select {
	case err := <-errCh:
		return 255, err
	case exitCode := <-exitCodeCh:
		return exitCode, nil
	case <-doneCh:
		return 0, nil
	}
}
