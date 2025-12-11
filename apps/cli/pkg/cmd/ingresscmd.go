package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/onkernel/hypeman-go"
	"github.com/onkernel/hypeman-go/option"
	"github.com/urfave/cli/v3"
)

var ingressCmd = cli.Command{
	Name:  "ingress",
	Usage: "Manage ingresses",
	Commands: []*cli.Command{
		&ingressCreateCmd,
		&ingressListCmd,
		&ingressDeleteCmd,
	},
	HideHelpCommand: true,
}

var ingressCreateCmd = cli.Command{
	Name:      "create",
	Usage:     "Create an ingress for an instance",
	ArgsUsage: "<instance>",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:     "hostname",
			Aliases:  []string{"H"},
			Usage:    "Hostname to match (exact match on Host header)",
			Required: true,
		},
		&cli.IntFlag{
			Name:     "port",
			Aliases:  []string{"p"},
			Usage:    "Target port on the instance",
			Required: true,
		},
		&cli.IntFlag{
			Name:  "host-port",
			Usage: "Host port to listen on (default: 80)",
			Value: 80,
		},
		&cli.BoolFlag{
			Name:  "tls",
			Usage: "Enable TLS termination (certificate auto-issued via ACME)",
		},
		&cli.BoolFlag{
			Name:  "redirect-http",
			Usage: "Auto-create HTTP to HTTPS redirect (only applies when --tls is enabled)",
		},
		&cli.StringFlag{
			Name:  "name",
			Usage: "Ingress name (auto-generated from hostname if not provided)",
		},
	},
	Action:          handleIngressCreate,
	HideHelpCommand: true,
}

var ingressListCmd = cli.Command{
	Name:  "list",
	Usage: "List ingresses",
	Flags: []cli.Flag{
		&cli.BoolFlag{
			Name:    "quiet",
			Aliases: []string{"q"},
			Usage:   "Only display ingress IDs",
		},
	},
	Action:          handleIngressList,
	HideHelpCommand: true,
}

var ingressDeleteCmd = cli.Command{
	Name:            "delete",
	Usage:           "Delete an ingress",
	ArgsUsage:       "<id>",
	Action:          handleIngressDelete,
	HideHelpCommand: true,
}

func handleIngressCreate(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args().Slice()
	if len(args) < 1 {
		return fmt.Errorf("instance name or ID required\nUsage: hypeman ingress create <instance> --hostname <hostname> --port <port>")
	}

	instance := args[0]
	hostname := cmd.String("hostname")
	port := cmd.Int("port")
	hostPort := cmd.Int("host-port")
	tls := cmd.Bool("tls")
	redirectHTTP := cmd.Bool("redirect-http")
	name := cmd.String("name")

	// Auto-generate name from hostname if not provided
	if name == "" {
		name = generateIngressName(hostname)
	}

	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)

	var opts []option.RequestOption
	if cmd.Root().Bool("debug") {
		opts = append(opts, debugMiddlewareOption)
	}

	params := hypeman.IngressNewParams{
		Name: name,
		Rules: []hypeman.IngressRuleParam{
			{
				Match: hypeman.IngressMatchParam{
					Hostname: hostname,
					Port:     hypeman.Int(int64(hostPort)),
				},
				Target: hypeman.IngressTargetParam{
					Instance: instance,
					Port:     int64(port),
				},
				Tls:          hypeman.Bool(tls),
				RedirectHTTP: hypeman.Bool(redirectHTTP),
			},
		},
	}

	fmt.Fprintf(os.Stderr, "Creating ingress %s...\n", name)

	result, err := client.Ingresses.New(ctx, params, opts...)
	if err != nil {
		return err
	}

	fmt.Println(result.ID)
	return nil
}

func handleIngressList(ctx context.Context, cmd *cli.Command) error {
	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)

	var opts []option.RequestOption
	if cmd.Root().Bool("debug") {
		opts = append(opts, debugMiddlewareOption)
	}

	ingresses, err := client.Ingresses.List(ctx, opts...)
	if err != nil {
		return err
	}

	quietMode := cmd.Bool("quiet")

	if quietMode {
		for _, ing := range *ingresses {
			fmt.Println(ing.ID)
		}
		return nil
	}

	if len(*ingresses) == 0 {
		fmt.Fprintln(os.Stderr, "No ingresses found.")
		return nil
	}

	table := NewTableWriter(os.Stdout, "ID", "NAME", "HOSTNAME", "TARGET", "TLS", "CREATED")
	for _, ing := range *ingresses {
		// Extract first rule's hostname and target for display
		hostname := ""
		target := ""
		tlsEnabled := "-"
		if len(ing.Rules) > 0 {
			rule := ing.Rules[0]
			hostname = rule.Match.Hostname
			target = fmt.Sprintf("%s:%d", rule.Target.Instance, rule.Target.Port)
			if rule.Tls {
				tlsEnabled = "yes"
			} else {
				tlsEnabled = "no"
			}
		}

		table.AddRow(
			TruncateID(ing.ID),
			TruncateString(ing.Name, 20),
			TruncateString(hostname, 25),
			target,
			tlsEnabled,
			FormatTimeAgo(ing.CreatedAt),
		)
	}
	table.Render()

	return nil
}

func handleIngressDelete(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args().Slice()
	if len(args) < 1 {
		return fmt.Errorf("ingress ID or name required\nUsage: hypeman ingress delete <id>")
	}

	id := args[0]

	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)

	var opts []option.RequestOption
	if cmd.Root().Bool("debug") {
		opts = append(opts, debugMiddlewareOption)
	}

	err := client.Ingresses.Delete(ctx, id, opts...)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "Ingress %s deleted.\n", id)
	return nil
}

// generateIngressName generates an ingress name from hostname
func generateIngressName(hostname string) string {
	// Replace dots with dashes
	name := strings.ReplaceAll(hostname, ".", "-")
	name = strings.ToLower(name)

	// Remove invalid characters (only allow a-z, 0-9, and -)
	var cleaned strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			cleaned.WriteRune(r)
		}
	}
	name = cleaned.String()

	// Trim leading/trailing dashes
	name = strings.Trim(name, "-")

	// Add random suffix
	suffix := randomSuffix(4)
	return fmt.Sprintf("%s-%s", name, suffix)
}
