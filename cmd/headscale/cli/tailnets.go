package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/juanfont/headscale/hscontrol/types"
	"github.com/pterm/pterm"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(tailnetCmd)

	// list
	tailnetCmd.AddCommand(listTailnetsCmd)

	// create
	tailnetCmd.AddCommand(createTailnetCmd)
	createTailnetCmd.Flags().StringP("ipv4-prefix", "4", "", "IPv4 prefix for this tailnet (e.g. 100.64.0.0/10)")
	createTailnetCmd.Flags().StringP("ipv6-prefix", "6", "", "IPv6 prefix for this tailnet")
	createTailnetCmd.Flags().StringP("base-domain", "d", "", "MagicDNS base domain (e.g. example.ts.net)")
	createTailnetCmd.Flags().StringP("policy-file", "p", "", "Path to HuJSON ACL policy file")

	// get
	tailnetCmd.AddCommand(getTailnetCmd)

	// update
	tailnetCmd.AddCommand(updateTailnetCmd)
	updateTailnetCmd.Flags().StringP("base-domain", "d", "", "New MagicDNS base domain")
	updateTailnetCmd.Flags().StringP("policy-file", "p", "", "Path to HuJSON ACL policy file")

	// delete
	tailnetCmd.AddCommand(deleteTailnetCmd)
	deleteTailnetCmd.Flags().BoolP("force", "f", false, "Skip confirmation prompt")

	// set-policy
	tailnetCmd.AddCommand(setTailnetPolicyCmd)
	setTailnetPolicyCmd.Flags().StringP("policy-file", "p", "", "Path to HuJSON ACL policy file")
	mustMarkRequired(setTailnetPolicyCmd, "policy-file")
}

var tailnetCmd = &cobra.Command{
	Use:     "tailnets",
	Short:   "Manage tailnets (multi-tenancy)",
	Aliases: []string{"tailnet"},
}

// ---- list -------------------------------------------------------------------

var listTailnetsCmd = &cobra.Command{
	Use:     "list",
	Short:   "List all tailnets",
	Aliases: []string{"ls"},
	RunE: func(cmd *cobra.Command, args []string) error {
		output, _ := cmd.Flags().GetString("output")

		data, err := tailnetAPICall(http.MethodGet, "/api/v1/tailnet", nil)
		if err != nil {
			return err
		}

		var tailnets []tailnetJSON
		if err := json.Unmarshal(data, &tailnets); err != nil {
			return fmt.Errorf("parsing response: %w", err)
		}

		if strings.HasPrefix(output, "json") {
			fmt.Println(string(data))
			return nil
		}

		if len(tailnets) == 0 {
			pterm.Info.Println("No tailnets found.")
			return nil
		}

		tableData := pterm.TableData{{"ID", "Name", "IPv4 Prefix", "Base Domain", "Created"}}
		for _, tn := range tailnets {
			tableData = append(tableData, []string{
				fmt.Sprintf("%d", tn.ID),
				tn.Name,
				tn.IPv4Prefix,
				tn.BaseDomain,
				tn.CreatedAt.Format(HeadscaleDateTimeFormat),
			})
		}

		return pterm.DefaultTable.WithHasHeader().WithData(tableData).Render()
	},
}

// ---- get --------------------------------------------------------------------

var getTailnetCmd = &cobra.Command{
	Use:     "get <id>",
	Short:   "Get a tailnet by ID",
	Args:    cobra.ExactArgs(1),
	Aliases: []string{"show"},
	RunE: func(cmd *cobra.Command, args []string) error {
		data, err := tailnetAPICall(http.MethodGet, "/api/v1/tailnet/"+args[0], nil)
		if err != nil {
			return err
		}

		var tn tailnetJSON
		if err := json.Unmarshal(data, &tn); err != nil {
			return fmt.Errorf("parsing response: %w", err)
		}

		printTailnetDetails(tn)

		return nil
	},
}

// ---- create -----------------------------------------------------------------

var createTailnetCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create a new tailnet",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		req := map[string]string{
			"name": args[0],
		}

		if v, _ := cmd.Flags().GetString("ipv4-prefix"); v != "" {
			req["ipv4_prefix"] = v
		}

		if v, _ := cmd.Flags().GetString("ipv6-prefix"); v != "" {
			req["ipv6_prefix"] = v
		}

		if v, _ := cmd.Flags().GetString("base-domain"); v != "" {
			req["base_domain"] = v
		}

		if pf, _ := cmd.Flags().GetString("policy-file"); pf != "" {
			pol, err := os.ReadFile(pf)
			if err != nil {
				return fmt.Errorf("reading policy file: %w", err)
			}

			req["acl_policy"] = string(pol)
		}

		data, err := tailnetAPICall(http.MethodPost, "/api/v1/tailnet", req)
		if err != nil {
			return err
		}

		var tn tailnetJSON
		if err := json.Unmarshal(data, &tn); err != nil {
			return fmt.Errorf("parsing response: %w", err)
		}

		pterm.Success.Printf("Tailnet %q created (id: %d)\n", tn.Name, tn.ID)
		printTailnetDetails(tn)

		return nil
	},
}

// ---- update -----------------------------------------------------------------

var updateTailnetCmd = &cobra.Command{
	Use:   "update <id>",
	Short: "Update a tailnet's base domain or ACL policy",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		req := map[string]string{}

		if v, _ := cmd.Flags().GetString("base-domain"); v != "" {
			req["base_domain"] = v
		}

		if pf, _ := cmd.Flags().GetString("policy-file"); pf != "" {
			pol, err := os.ReadFile(pf)
			if err != nil {
				return fmt.Errorf("reading policy file: %w", err)
			}

			req["acl_policy"] = string(pol)
		}

		if len(req) == 0 {
			return fmt.Errorf("provide at least --base-domain or --policy-file")
		}

		data, err := tailnetAPICall(http.MethodPut, "/api/v1/tailnet/"+args[0], req)
		if err != nil {
			return err
		}

		var tn tailnetJSON
		if err := json.Unmarshal(data, &tn); err != nil {
			return fmt.Errorf("parsing response: %w", err)
		}

		pterm.Success.Printf("Tailnet %q updated\n", tn.Name)
		printTailnetDetails(tn)

		return nil
	},
}

// ---- delete -----------------------------------------------------------------

var deleteTailnetCmd = &cobra.Command{
	Use:     "delete <id>",
	Short:   "Delete a tailnet",
	Args:    cobra.ExactArgs(1),
	Aliases: []string{"rm", "remove"},
	RunE: func(cmd *cobra.Command, args []string) error {
		force, _ := cmd.Flags().GetBool("force")
		if !force {
			confirm, _ := pterm.DefaultInteractiveConfirm.
				WithDefaultValue(false).
				Show(fmt.Sprintf("Delete tailnet %s? This cannot be undone.", args[0]))
			if !confirm {
				pterm.Info.Println("Aborted.")
				return nil
			}
		}

		_, err := tailnetAPICall(http.MethodDelete, "/api/v1/tailnet/"+args[0], nil)
		if err != nil {
			return err
		}

		pterm.Success.Printf("Tailnet %s deleted\n", args[0])

		return nil
	},
}

// ---- set-policy -------------------------------------------------------------

var setTailnetPolicyCmd = &cobra.Command{
	Use:   "set-policy <id>",
	Short: "Set the ACL policy for a tailnet",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		pf, _ := cmd.Flags().GetString("policy-file")

		pol, err := os.ReadFile(pf)
		if err != nil {
			return fmt.Errorf("reading policy file: %w", err)
		}

		data, err := tailnetAPICall(http.MethodPut, "/api/v1/tailnet/"+args[0]+"/policy",
			map[string]string{"policy": string(pol)})
		if err != nil {
			return err
		}

		var tn tailnetJSON
		if err := json.Unmarshal(data, &tn); err != nil {
			return fmt.Errorf("parsing response: %w", err)
		}

		pterm.Success.Printf("ACL policy applied to tailnet %q (id: %d)\n", tn.Name, tn.ID)

		return nil
	},
}

// ---- Shared helpers ---------------------------------------------------------

type tailnetJSON struct {
	ID         uint      `json:"id"`
	Name       string    `json:"name"`
	IPv4Prefix string    `json:"ipv4_prefix"`
	IPv6Prefix string    `json:"ipv6_prefix"`
	BaseDomain string    `json:"base_domain"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func printTailnetDetails(tn tailnetJSON) {
	pterm.DefaultSection.Println("Tailnet Details")

	tableData := pterm.TableData{
		{"Field", "Value"},
		{"ID", fmt.Sprintf("%d", tn.ID)},
		{"Name", tn.Name},
		{"IPv4 Prefix", tn.IPv4Prefix},
		{"IPv6 Prefix", tn.IPv6Prefix},
		{"Base Domain", tn.BaseDomain},
		{"Created", tn.CreatedAt.Format(HeadscaleDateTimeFormat)},
		{"Updated", tn.UpdatedAt.Format(HeadscaleDateTimeFormat)},
	}

	_ = pterm.DefaultTable.WithHasHeader().WithData(tableData).Render()
}

// tailnetAPICall makes an authenticated HTTP call to the tailnet REST API.
// It reads the address and API key from the CLI config (same as gRPC commands).
func tailnetAPICall(method, path string, body any) ([]byte, error) {
	cfg, err := loadCLIConfigForREST()
	if err != nil {
		return nil, err
	}

	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encoding request: %w", err)
		}

		bodyReader = bytes.NewReader(b)
	}

	url := strings.TrimRight(cfg.address, "/") + path

	req, err := http.NewRequestWithContext(context.Background(), method, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+cfg.apiKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling API: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode == http.StatusNoContent {
		return data, nil
	}

	if resp.StatusCode >= 400 {
		var e map[string]string
		if json.Unmarshal(data, &e) == nil {
			if msg, ok := e["error"]; ok {
				return nil, fmt.Errorf("API error (%d): %s", resp.StatusCode, msg)
			}
		}

		return nil, fmt.Errorf("API error (%d): %s", resp.StatusCode, string(data))
	}

	return data, nil
}

type cliRESTConfig struct {
	address string
	apiKey  string
}

func loadCLIConfigForREST() (cliRESTConfig, error) {
	cfg, err := types.LoadCLIConfig()
	if err != nil {
		return cliRESTConfig{}, fmt.Errorf("loading CLI config: %w", err)
	}

	address := cfg.CLI.Address
	if address == "" {
		return cliRESTConfig{}, fmt.Errorf("server address not configured (set cli.address in config or HEADSCALE_CLI_ADDRESS)")
	}

	// Prefer HTTPS for remote addresses; the gRPC address typically includes
	// the port — build an HTTP base URL from it.
	if !strings.HasPrefix(address, "http://") && !strings.HasPrefix(address, "https://") {
		address = "https://" + address
	}

	apiKey := cfg.CLI.APIKey
	if apiKey == "" {
		return cliRESTConfig{}, fmt.Errorf("API key not set (set cli.api_key in config or HEADSCALE_CLI_API_KEY)")
	}

	return cliRESTConfig{address: address, apiKey: apiKey}, nil
}
