package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/gorilla/websocket"
	"github.com/onkernel/hypeman-go"
	"github.com/urfave/cli/v3"
	"golang.org/x/term"
)

// ExecExitError is returned when exec completes with a non-zero exit code
type ExecExitError struct {
	Code int
}

func (e *ExecExitError) Error() string {
	return fmt.Sprintf("exec exited with code %d", e.Code)
}

// execRequest represents the JSON body for exec requests
type execRequest struct {
	Command []string          `json:"command"`
	TTY     bool              `json:"tty"`
	Env     map[string]string `json:"env,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
	Timeout int32             `json:"timeout,omitempty"`
}

var execCmd = cli.Command{
	Name:      "exec",
	Usage:     "Execute a command in a running instance",
	ArgsUsage: "<instance-id> [-- command...]",
	Flags: []cli.Flag{
		&cli.BoolFlag{
			Name:    "it",
			Aliases: []string{"i", "t"},
			Usage:   "Enable interactive TTY mode",
		},
		&cli.BoolFlag{
			Name:    "no-tty",
			Aliases: []string{"T"},
			Usage:   "Disable TTY allocation",
		},
		&cli.StringSliceFlag{
			Name:    "env",
			Aliases: []string{"e"},
			Usage:   "Set environment variable (KEY=VALUE, can be repeated)",
		},
		&cli.StringFlag{
			Name:  "cwd",
			Usage: "Working directory inside the instance",
		},
		&cli.IntFlag{
			Name:  "timeout",
			Usage: "Execution timeout in seconds (0 = no timeout)",
		},
	},
	Action:          handleExec,
	HideHelpCommand: true,
}

func handleExec(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args().Slice()
	if len(args) < 1 {
		return fmt.Errorf("instance ID required\nUsage: hypeman exec [flags] <instance-id> [-- command...]")
	}

	// Resolve instance by ID, partial ID, or name
	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)
	instanceID, err := ResolveInstance(ctx, &client, args[0])
	if err != nil {
		return err
	}

	var command []string

	// Parse command after -- separator or remaining args
	if len(args) > 1 {
		command = args[1:]
	}

	// Determine TTY mode
	tty := true // default
	if cmd.Bool("no-tty") {
		tty = false
	} else if cmd.Bool("it") {
		tty = true
	} else {
		// Auto-detect: enable TTY if stdin and stdout are terminals
		tty = term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
	}

	// Parse environment variables
	env := make(map[string]string)
	for _, e := range cmd.StringSlice("env") {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			env[parts[0]] = parts[1]
		} else {
			fmt.Fprintf(os.Stderr, "Warning: ignoring malformed env var: %s\n", e)
		}
	}

	// Build exec request
	execReq := execRequest{
		Command: command,
		TTY:     tty,
	}
	if len(env) > 0 {
		execReq.Env = env
	}
	if cwd := cmd.String("cwd"); cwd != "" {
		execReq.Cwd = cwd
	}
	if timeout := cmd.Int("timeout"); timeout > 0 {
		execReq.Timeout = int32(timeout)
	}

	reqBody, err := json.Marshal(execReq)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	// Get base URL and API key
	baseURL := cmd.Root().String("base-url")
	if baseURL == "" {
		baseURL = os.Getenv("HYPEMAN_BASE_URL")
	}
	if baseURL == "" {
		baseURL = "http://localhost:8080"
	}

	apiKey := os.Getenv("HYPEMAN_API_KEY")
	if apiKey == "" {
		return fmt.Errorf("HYPEMAN_API_KEY environment variable required")
	}

	// Build WebSocket URL
	u, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("invalid base URL: %w", err)
	}
	u.Path = fmt.Sprintf("/instances/%s/exec", instanceID)

	// Convert scheme to WebSocket
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}

	// Connect WebSocket with auth header
	headers := http.Header{}
	headers.Set("Authorization", fmt.Sprintf("Bearer %s", apiKey))

	dialer := &websocket.Dialer{}
	ws, resp, err := dialer.DialContext(ctx, u.String(), headers)
	if err != nil {
		if resp != nil {
			body, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("websocket connect failed (HTTP %d): %s", resp.StatusCode, string(body))
		}
		return fmt.Errorf("websocket connect failed: %w", err)
	}
	defer ws.Close()

	// Send JSON request as first message
	if err := ws.WriteMessage(websocket.TextMessage, reqBody); err != nil {
		return fmt.Errorf("failed to send exec request: %w", err)
	}

	// Run interactive or non-interactive mode
	var exitCode int
	if tty {
		exitCode, err = runExecInteractive(ws)
	} else {
		exitCode, err = runExecNonInteractive(ws)
	}

	if err != nil {
		return err
	}

	if exitCode != 0 {
		return &ExecExitError{Code: exitCode}
	}

	return nil
}

func runExecInteractive(ws *websocket.Conn) (int, error) {
	// Put terminal in raw mode
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return 255, fmt.Errorf("failed to set raw mode: %w", err)
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	// Handle signals gracefully
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

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
					exitCodeCh <- 0
				}
				return
			}

			// Check for exit code message
			if msgType == websocket.TextMessage && bytes.Contains(message, []byte("exitCode")) {
				var exitMsg struct {
					ExitCode int `json:"exitCode"`
				}
				if json.Unmarshal(message, &exitMsg) == nil {
					exitCodeCh <- exitMsg.ExitCode
					return
				}
			}

			// Write binary messages to stdout (actual output)
			if msgType == websocket.BinaryMessage {
				os.Stdout.Write(message)
			}
		}
	}()

	select {
	case err := <-errCh:
		return 255, err
	case exitCode := <-exitCodeCh:
		return exitCode, nil
	case <-sigCh:
		return 130, nil // 128 + SIGINT
	}
}

func runExecNonInteractive(ws *websocket.Conn) (int, error) {
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
				if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) ||
					err == io.EOF {
					exitCodeCh <- 0
					return
				}
				errCh <- fmt.Errorf("websocket read error: %w", err)
				return
			}

			// Check for exit code message
			if msgType == websocket.TextMessage && bytes.Contains(message, []byte("exitCode")) {
				var exitMsg struct {
					ExitCode int `json:"exitCode"`
				}
				if json.Unmarshal(message, &exitMsg) == nil {
					exitCodeCh <- exitMsg.ExitCode
					return
				}
			}

			// Write to stdout (binary messages contain actual output)
			if msgType == websocket.BinaryMessage {
				os.Stdout.Write(message)
			}
		}
	}()

	select {
	case err := <-errCh:
		return 255, err
	case exitCode := <-exitCodeCh:
		return exitCode, nil
	case <-doneCh:
		return 0, nil
	}
}

