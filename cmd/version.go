package cmd

import (
	"fmt"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(cmdVersion)
}

var cmdVersion = &cobra.Command{
	Use:   "version",
	Short: "",
	Long:  "",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("dockware-cli v0.6")
	},
}
