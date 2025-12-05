package cmd

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"os"
	"strings"

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

func ShowJSON(title string, res gjson.Result, format string, transform string) error {
	if format != "raw" && transform != "" {
		transformed := res.Get(transform)
		if transformed.Exists() {
			res = transformed
		}
	}
	switch strings.ToLower(format) {
	case "auto":
		return ShowJSON(title, res, "json", "")
	case "explore":
		return jsonview.ExploreJSON(title, res)
	case "pretty":
		jsonview.DisplayJSON(title, res)
		return nil
	case "json":
		prettyJSON := pretty.Pretty([]byte(res.Raw))
		if shouldUseColors(os.Stdout) {
			fmt.Print(string(pretty.Color(prettyJSON, pretty.TerminalStyle)))
		} else {
			fmt.Print(string(prettyJSON))
		}
		return nil
	case "raw":
		fmt.Println(res.Raw)
		return nil
	case "yaml":
		input := strings.NewReader(res.Raw)
		var yaml strings.Builder
		if err := json2yaml.Convert(&yaml, input); err != nil {
			return err
		}
		fmt.Print(yaml.String())
		return nil
	default:
		return fmt.Errorf("Invalid format: %s, valid formats are: %s", format, strings.Join(OutputFormats, ", "))
	}
}
