package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/gliese129/runq/internal/api"
)

func getSocketPath() string {
	if socketPathFlags, _ := rootCmd.PersistentFlags().GetString("socket"); socketPathFlags != "" {
		return socketPathFlags
	}
	if socketPathEnv := os.Getenv("RUNQ_SOCKET"); socketPathEnv != "" {
		return socketPathEnv
	}
	return api.DefaultSocketPath()
}

// newClient creates an API client connecting to the daemon's unix socket.
func newClient() *api.Client {
	return api.NewClient(getSocketPath())
}

// doAndDecode sends a request to the daemon and decodes the JSON response.
// If v is nil, the response body is drained and discarded.
func doAndDecode(method, path string, body any, v any) error {
	client := newClient()
	resp, err := client.Do(method, path, body)
	if err != nil {
		msg := api.DiagnoseDaemon(getSocketPath(), api.DefaultPIDPath())
		return fmt.Errorf("%s", msg)
	}

	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		type ErrResp struct {
			Error string
		}
		var errResp ErrResp
		err := json.NewDecoder(resp.Body).Decode(&errResp)
		if err != nil {
			return err
		}
		return fmt.Errorf("%s", errResp.Error)
	}
	if v == nil {
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			return err
		}
		return nil
	}
	err = json.NewDecoder(resp.Body).Decode(v)
	if err != nil {
		return err
	}
	return nil
}

// ── Helpers (provided) ──

// printJSON prints v as indented JSON to stdout.
func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

// printError formats an API error for the user.
// Used when doAndDecode returns an error — Cobra will print it, but if you
// want to customize the format, wrap through here.
func printError(err error) {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
}

// newTable creates a tab-aligned writer for CLI table output.
// Call w.Flush() after writing all rows.
func newTable() *tabwriter.Writer {
	return tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
}
