// The cnosdb-tools command displays detailed information about CnosDB data files.
package main

import (
	"fmt"

	"github.com/cnosdb/cnosdb/cmd/cnosdb-tools/compact"

	_ "github.com/cnosdb/cnosdb/cmd/cnosdb-tools/export"
	_ "github.com/cnosdb/cnosdb/cmd/cnosdb-tools/generate/exec"
	_ "github.com/cnosdb/cnosdb/cmd/cnosdb-tools/generate/init"
	_ "github.com/cnosdb/cnosdb/cmd/cnosdb-tools/help"
	_ "github.com/cnosdb/cnosdb/cmd/cnosdb-tools/importer"
	_ "github.com/cnosdb/cnosdb/cmd/cnosdb-tools/server"
	_ "github.com/cnosdb/cnosdb/cmd/cnosdb/run"
	_ "github.com/cnosdb/cnosdb/meta"
	_ "github.com/cnosdb/cnosdb/vend/db/tsdb"
	_ "github.com/cnosdb/cnosdb/vend/db/tsdb/engine"

	geninit "github.com/cnosdb/cnosdb/cmd/cnosdb-tools/generate/init"
	"github.com/spf13/cobra"
)

func main() {

	mainCmd := GetCommand()

	geninit := geninit.GetCommand()
	mainCmd.AddCommand(geninit)

	compact := compact.GetCommand()
	mainCmd.AddCommand(compact)

	if err := mainCmd.Execute(); err != nil {
		fmt.Printf("Error : %+v\n", err)
	}

}

func GetCommand() *cobra.Command {
	c := &cobra.Command{
		Use:  "cnosdb-tools",
		Long: "tools for managing and querying CnosDB data",
		CompletionOptions: cobra.CompletionOptions{
			DisableDefaultCmd:   true,
			DisableNoDescFlag:   true,
			DisableDescriptions: true},
	}

	return c
}