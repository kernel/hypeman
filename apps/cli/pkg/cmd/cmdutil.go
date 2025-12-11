package cmd

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/onkernel/hypeman-cli/pkg/jsonview"
	"github.com/onkernel/hypeman-go/option"

	"github.com/itchyny/json2yaml"
	"github.com/tidwall/gjson"
	"github.com/tidwall/pretty"
	"github.com/urfave/cli/v3"
	"golang.org/x/term"
)

func getDefaultRequestOptions(cmd *cli.Command) []option.RequestOption {
	opts := []option.RequestOption{
		option.WithHeader("User-Agent", fmt.Sprintf("Hypeman/CLI %s", Version)),
		option.WithHeader("X-Stainless-Lang", "cli"),
		option.WithHeader("X-Stainless-Package-Version", Version),
		option.WithHeader("X-Stainless-Runtime", "cli"),
		option.WithHeader("X-Stainless-CLI-Command", cmd.FullName()),
	}

	// Override base URL if the --base-url flag is provided
	if baseURL := cmd.String("base-url"); baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}

	return opts
}

var debugMiddlewareOption = option.WithMiddleware(
	func(r *http.Request, mn option.MiddlewareNext) (*http.Response, error) {
		logger := log.Default()

		if reqBytes, err := httputil.DumpRequest(r, true); err == nil {
			logger.Printf("Request Content:\n%s\n", reqBytes)
		}

		resp, err := mn(r)
		if err != nil {
			return resp, err
		}

		if respBytes, err := httputil.DumpResponse(resp, true); err == nil {
			logger.Printf("Response Content:\n%s\n", respBytes)
		}

		return resp, err
	},
)

func isInputPiped() bool {
	stat, _ := os.Stdin.Stat()
	return (stat.Mode() & os.ModeCharDevice) == 0
}

func isTerminal(w io.Writer) bool {
	switch v := w.(type) {
	case *os.File:
		return term.IsTerminal(int(v.Fd()))
	default:
		return false
	}
}

func streamOutput(label string, generateOutput func(w *os.File) error) error {
	// For non-tty output (probably a pipe), write directly to stdout
	if !isTerminal(os.Stdout) {
		return streamToStdout(generateOutput)
	}

	pagerInput, outputFile, isSocketPair, err := createPagerFiles()
	if err != nil {
		return err
	}
	defer pagerInput.Close()
	defer outputFile.Close()

	cmd, err := startPagerCommand(pagerInput, label, isSocketPair)
	if err != nil {
		return err
	}

	if err := pagerInput.Close(); err != nil {
		return err
	}

	// If the pager exits before reading all input, then generateOutput() will
	// produce a broken pipe error, which is fine and we don't want to propagate it.
	if err := generateOutput(outputFile); err != nil && !strings.Contains(err.Error(), "broken pipe") {
		return err
	}

	return cmd.Wait()
}

func streamToStdout(generateOutput func(w *os.File) error) error {
	signal.Ignore(syscall.SIGPIPE)
	err := generateOutput(os.Stdout)
	if err != nil && strings.Contains(err.Error(), "broken pipe") {
		return nil
	}
	return err
}

func createPagerFiles() (*os.File, *os.File, bool, error) {
	// We prefer sockets when available because they allow for smaller buffer
	// sizes, preventing unnecessary data streaming from the backend. Pipes
	// typically have large buffers but serve as a decent alternative when
	// sockets aren't available (e.g., on Windows).
	pagerInput, outputFile, isSocketPair, err := createSocketPair()
	if err == nil {
		return pagerInput, outputFile, isSocketPair, nil
	}

	r, w, err := os.Pipe()
	return r, w, false, err
}

// Start a subprocess running the user's preferred pager (or `less` if `$PAGER` is unset)
func startPagerCommand(pagerInput *os.File, label string, useSocketpair bool) (*exec.Cmd, error) {
	pagerProgram := os.Getenv("PAGER")
	if pagerProgram == "" {
		pagerProgram = "less"
	}

	if shouldUseColors(os.Stdout) {
		os.Setenv("FORCE_COLOR", "1")
	}

	var cmd *exec.Cmd
	if useSocketpair {
		cmd = exec.Command(pagerProgram, fmt.Sprintf("/dev/fd/%d", pagerInput.Fd()))
		cmd.ExtraFiles = []*os.File{pagerInput}
	} else {
		cmd = exec.Command(pagerProgram)
		cmd.Stdin = pagerInput
	}

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(),
		"LESS=-r -f -P "+label,
		"MORE=-r -f -P "+label,
	)

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	return cmd, nil
}

func shouldUseColors(w io.Writer) bool {
	force, ok := os.LookupEnv("FORCE_COLOR")
	if ok {
		if force == "1" {
			return true
		}
		if force == "0" {
			return false
		}
	}
	return isTerminal(w)
}

func ShowJSON(out *os.File, title string, res gjson.Result, format string, transform string) error {
	if format != "raw" && transform != "" {
		transformed := res.Get(transform)
		if transformed.Exists() {
			res = transformed
		}
	}
	switch strings.ToLower(format) {
	case "auto":
		return ShowJSON(out, title, res, "json", "")
	case "explore":
		return jsonview.ExploreJSON(title, res)
	case "pretty":
		_, err := out.WriteString(jsonview.RenderJSON(title, res) + "\n")
		return err
	case "json":
		prettyJSON := pretty.Pretty([]byte(res.Raw))
		if shouldUseColors(out) {
			_, err := out.Write(pretty.Color(prettyJSON, pretty.TerminalStyle))
			return err
		} else {
			_, err := out.Write(prettyJSON)
			return err
		}
	case "jsonl":
		// @ugly is gjson syntax for "no whitespace", so it fits on one line
		oneLineJSON := res.Get("@ugly").Raw
		if shouldUseColors(out) {
			bytes := append(pretty.Color([]byte(oneLineJSON), pretty.TerminalStyle), '\n')
			_, err := out.Write(bytes)
			return err
		} else {
			_, err := out.Write([]byte(oneLineJSON + "\n"))
			return err
		}
	case "raw":
		if _, err := out.Write([]byte(res.Raw + "\n")); err != nil {
			return err
		}
		return nil
	case "yaml":
		input := strings.NewReader(res.Raw)
		var yaml strings.Builder
		if err := json2yaml.Convert(&yaml, input); err != nil {
			return err
		}
		_, err := out.Write([]byte(yaml.String()))
		return err
	default:
		return fmt.Errorf("Invalid format: %s, valid formats are: %s", format, strings.Join(OutputFormats, ", "))
	}
}
