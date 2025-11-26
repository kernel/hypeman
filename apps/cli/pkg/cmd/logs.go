package cmd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"

	"github.com/onkernel/hypeman-go"
	"github.com/urfave/cli/v3"
)

var logsCmd = cli.Command{
	Name:      "logs",
	Usage:     "Fetch the logs of an instance",
	ArgsUsage: "<instance>",
	Flags: []cli.Flag{
		&cli.BoolFlag{
			Name:    "follow",
			Aliases: []string{"f"},
			Usage:   "Follow log output",
		},
		&cli.IntFlag{
			Name:  "tail",
			Usage: "Number of lines to show from the end of the logs",
			Value: 100,
		},
	},
	Action:          handleLogs,
	HideHelpCommand: true,
}

func handleLogs(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args().Slice()
	if len(args) < 1 {
		return fmt.Errorf("instance ID required\nUsage: hypeman logs [flags] <instance>")
	}

	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)

	// Resolve instance by ID, partial ID, or name
	instanceID, err := ResolveInstance(ctx, &client, args[0])
	if err != nil {
		return err
	}

	// Build URL for logs endpoint
	baseURL := cmd.Root().String("base-url")
	if baseURL == "" {
		baseURL = os.Getenv("HYPEMAN_BASE_URL")
	}
	if baseURL == "" {
		baseURL = "http://localhost:8080"
	}

	u, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("invalid base URL: %w", err)
	}
	u.Path = fmt.Sprintf("/instances/%s/logs", instanceID)

	// Add query parameters
	q := u.Query()
	q.Set("tail", fmt.Sprintf("%d", cmd.Int("tail")))
	if cmd.Bool("follow") {
		q.Set("follow", "true")
	}
	u.RawQuery = q.Encode()

	// Make HTTP request
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	apiKey := os.Getenv("HYPEMAN_API_KEY")
	if apiKey != "" {
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", apiKey))
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to fetch logs: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to fetch logs (HTTP %d): %s", resp.StatusCode, string(body))
	}

	// Stream the response to stdout
	_, err = io.Copy(os.Stdout, resp.Body)
	return err
}


