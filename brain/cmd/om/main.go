// Command om is the Overmind CLI: plan a goal, approve the plan, then run
// and monitor sessions on the AO daemon.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/OmniMintX/overmind/internal/config"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	var cfgPath string

	root := &cobra.Command{
		Use:           "om",
		Short:         "Overmind — plan, dispatch and monitor agent sessions on the AO daemon",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&cfgPath, "config", "", "config file path (default ~/.overmind/config.yaml)")

	root.AddCommand(
		newPlanCmd(&cfgPath),
		newApproveCmd(&cfgPath),
		newRunCmd(&cfgPath),
		newStatusCmd(&cfgPath),
		newEventsCmd(&cfgPath),
	)
	return root
}

func loadConfig(cfgPath *string) (config.Config, error) {
	cfg, err := config.Load(*cfgPath)
	for _, w := range cfg.Warnings {
		fmt.Fprintln(os.Stderr, "Warning:", w)
	}
	return cfg, err
}

func newPlanCmd(cfgPath *string) *cobra.Command {
	var project string
	var edit bool

	cmd := &cobra.Command{
		Use:   "plan \"<goal>\"",
		Short: "Generate a DAG plan from a goal and save it as a draft",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cfgPath)
			if err != nil {
				return err
			}
			return runPlan(cfg, args[0], project, edit)
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "AO project id or repository path (required)")
	cmd.Flags().BoolVar(&edit, "edit", false, "open the generated plan in $EDITOR before saving")
	return cmd
}

func newApproveCmd(cfgPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "approve <plan-id>",
		Short: "Approve a draft plan for execution",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cfgPath)
			if err != nil {
				return err
			}
			return runApprove(cfg, args[0])
		},
	}
}

func newRunCmd(cfgPath *string) *cobra.Command {
	var autonomy string
	cmd := &cobra.Command{
		Use:   "run <plan-id>",
		Short: "Run an approved plan: dispatch sessions to the AO daemon until done/failed",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cfgPath)
			if err != nil {
				return err
			}
			return runRun(cfg, args[0], autonomy)
		},
	}
	cmd.Flags().StringVar(&autonomy, "autonomy", "",
		"override config autonomy: auto | accept-edits | bypass-permissions | off")
	return cmd
}

func newStatusCmd(cfgPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "status [plan-id]",
		Short: "Show status of a plan (event-derived) or list all plans",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cfgPath)
			if err != nil {
				return err
			}
			planID := ""
			if len(args) == 1 {
				planID = args[0]
			}
			return runStatus(cfg, planID)
		},
	}
}

func newEventsCmd(cfgPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "events <plan-id>",
		Short: "Show the append-only brain event log for a plan",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cfgPath)
			if err != nil {
				return err
			}
			return runEvents(cfg, args[0])
		},
	}
}
