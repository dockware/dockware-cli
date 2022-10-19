package cmd

import (
	"fmt"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"os"
	"os/exec"
	"syscall"
)

func init() {
	rootCmd.AddCommand(purgeCmd)
}


var purgeCmd = &cobra.Command{
	Use:   "purge",
	Short: "Purge the system by deleting all dockware images on your local machine.",
	Long:  "",
	Run: func(cmd *cobra.Command, args []string) {
		
		if !term.IsTerminal(int(syscall.Stdin)) {
			fmt.Println("interactive terminal required")
			os.Exit(1)
		}


		purgeSystem := askYesNo("Do you really want to delete all dockware images from your system?")
			
		if (purgeSystem) {
			
			f, _ := os.Create("delete-dockware.sh")

			line1 := "docker rm -f  $(docker ps | grep \"dockware\" | awk '{ print $1 }')"
			line2 := "docker rmi -f $(docker images --filter=reference='dockware/*:*' -q)"
			f.WriteString(line1 + "\n\n" + line2)
			f.Close()

			cmd := exec.Command("/bin/sh", "delete-dockware.sh")
			
			cmd.Stdin = os.Stdin
			cmd.Stderr = os.Stderr
			cmd.Stdout = os.Stdout
			err := cmd.Run()

			os.Remove("delete-dockware.sh")

			if err != nil {
				fmt.Println(err.Error())
				os.Exit(1)
			}
		}
		
	},
}
