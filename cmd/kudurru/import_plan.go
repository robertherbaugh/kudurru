package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/cli"
	"github.com/robertherbaugh/kudurru/internal/importers"
)

// selectMigrationPlan validates the entire explicit selection before any writes.
func selectMigrationPlan(cmd *cobra.Command, plan *importers.Plan, selectedObjects, selectedSettings []string, skipSettings bool) error {
	if cmd.Flags().Changed("select-object") || cmd.Flags().Changed("select-setting") || skipSettings {
		objects := append([]string{}, selectedObjects...)
		settings := append([]string{}, selectedSettings...)
		if skipSettings && !cmd.Flags().Changed("select-object") && !cmd.Flags().Changed("select-setting") {
			for _, doc := range plan.Objects {
				objects = append(objects, importers.ObjectID(doc))
			}
		}
		if skipSettings {
			settings = []string{}
		}
		plan.Selection = &importers.Selection{Objects: objects, Settings: settings}
		if _, _, err := plan.Selected(); err != nil {
			return err
		}
	}
	return nil
}

// finishMigration shares preview, one-shot start, and bounded report polling for
// live and offline sources. It never replays an import-start request.
func finishMigration(cmd *cobra.Command, o *cliOpts, plan *importers.Plan, f cli.Format, apply, wait bool) error {
	for _, w := range plan.Warnings {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "warning: "+w)
	}
	if !apply {
		return cli.WriteStructured(cmd.OutOrStdout(), f, plan)
	}
	c, _, err := o.client()
	if err != nil {
		return err
	}
	var run api.MigrationRun
	if err := c.Post(cmd.Context(), "/imports/start", nil, plan, &run); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Import started: %s. Inspect with kudurru import runs %s\n", run.ID, run.ID)
	pollFailures := 0
	for wait && run.Data.Status == "running" {
		timer := time.NewTimer(time.Second)
		select {
		case <-cmd.Context().Done():
			timer.Stop()
			return fmt.Errorf("import continues in the background as %s: %w", run.ID, cmd.Context().Err())
		case <-timer.C:
		}
		var next api.MigrationRun
		err := c.Get(cmd.Context(), "/imports/runs/"+run.ID, nil, &next)
		if err == nil && (next.ID != run.ID || next.Data.Status == "") {
			err = errors.New("server returned an invalid migration report")
		}
		if err != nil {
			pollFailures++
			if cmd.Context().Err() != nil || !transientMigrationPollError(err) || pollFailures >= maxMigrationPollFailures {
				return fmt.Errorf("cannot read import %s; the job may still be running; inspect with kudurru import runs %s: %w", run.ID, run.ID, err)
			}
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Cannot read import %s; retrying (%d/%d).\n", run.ID, pollFailures, maxMigrationPollFailures-1)
			continue
		}
		pollFailures = 0
		run = next
	}
	if err := cli.WriteStructured(cmd.OutOrStdout(), f, run); err != nil {
		return err
	}
	if wait && run.Data.Status != "completed" {
		return fmt.Errorf("import finished with errors or unconfirmed items; successful writes remain; inspect run %s", run.ID)
	}
	return nil
}

// migrationNamespace reads only local CLI context state, never the destination.
func migrationNamespace(o *cliOpts) (string, error) {
	if o.namespace != "" {
		return o.namespace, nil
	}
	cfg, err := o.loadConfig()
	if err != nil {
		return "", err
	}
	name := firstNonEmpty(o.context, o.getenv(cli.EnvContext), cfg.CurrentContext)
	for _, c := range cfg.Contexts {
		if c.Name == name && c.Namespace != "" {
			return c.Namespace, nil
		}
	}
	return "default", nil
}
